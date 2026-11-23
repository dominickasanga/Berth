// Package deploy runs git pull -> compose build -> pre-deploy step -> compose up for one app.
//
// Berth never runs "compose down" and never passes -v: volumes survive every deploy.
// Each app is one compose project, so there is exactly one instance of it.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dominickasanga/berth/internal/crypt"
	"github.com/dominickasanga/berth/internal/store"
)

var (
	ErrBusy     = errors.New("a deploy for this app is already running")
	branchRe    = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)
	ErrBadInput = errors.New("invalid input")
)

type Engine struct {
	Store   *store.Store
	Box     *crypt.Box
	DataDir string // holds checkouts/ and env/ (never inside a git checkout)
	// ComposeCmd is the compose invocation, e.g. ["docker","compose"] (default) or ["docker-compose"].
	ComposeCmd []string
	KeepImages int // rollback image tags kept per image (default 5)

	mu    sync.Mutex
	locks map[int64]*sync.Mutex
}

func (e *Engine) lockFor(id int64) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.locks == nil {
		e.locks = map[int64]*sync.Mutex{}
	}
	if e.locks[id] == nil {
		e.locks[id] = &sync.Mutex{}
	}
	return e.locks[id]
}

// ValidateApp rejects values that could be read as options or escape the checkout.
func ValidateApp(a store.App) error {
	if strings.HasPrefix(a.RepoURL, "-") || strings.TrimSpace(a.RepoURL) == "" {
		return fmt.Errorf("%w: repo URL", ErrBadInput)
	}
	if !branchRe.MatchString(a.Branch) || strings.HasPrefix(a.Branch, "-") || strings.Contains(a.Branch, "..") {
		return fmt.Errorf("%w: branch", ErrBadInput)
	}
	for _, p := range []string{a.Subfolder, a.ComposeFile} {
		if filepath.IsAbs(p) || strings.HasPrefix(filepath.Clean(p), "..") {
			return fmt.Errorf("%w: path must stay inside the repository", ErrBadInput)
		}
	}
	if a.ComposeFile == "" {
		return fmt.Errorf("%w: compose file", ErrBadInput)
	}
	if a.PredeploySvc != "" && !regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(a.PredeploySvc) {
		return fmt.Errorf("%w: pre-deploy service", ErrBadInput)
	}
	if a.ComposeProject != "" && !projectRe.MatchString(a.ComposeProject) {
		return fmt.Errorf("%w: compose project name", ErrBadInput)
	}
	switch a.SourceMode {
	case "", "clone":
	case "existing":
		if !filepath.IsAbs(a.SourcePath) || filepath.Clean(a.SourcePath) != a.SourcePath {
			return fmt.Errorf("%w: the existing folder must be a clean absolute path", ErrBadInput)
		}
	default:
		return fmt.Errorf("%w: source mode", ErrBadInput)
	}
	return nil
}

// checkoutDir is the app's own clone, or the user's existing folder in "existing" mode.
func (e *Engine) checkoutDir(a store.App) string {
	if a.SourceMode == "existing" {
		return a.SourcePath
	}
	return filepath.Join(e.DataDir, "checkouts", a.Name)
}
func (e *Engine) envFile(a store.App) string { return filepath.Join(e.DataDir, "env", a.Name+".env") }

func (e *Engine) compose(a store.App, args ...string) *exec.Cmd {
	return e.composeCtx(context.Background(), a, args...)
}

func (e *Engine) composeCtx(ctx context.Context, a store.App, args ...string) *exec.Cmd {
	return e.composeCmd(ctx, a, true, args...)
}

var projectRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ProjectName is the compose project: the app's own setting (to adopt an existing stack and
// keep its containers and named volumes), or "berth-<name>".
func ProjectName(a store.App) string {
	if a.ComposeProject != "" {
		return a.ComposeProject
	}
	return "berth-" + a.Name
}

// overrideFile is the generated compose override that injects variables saved in Berth.
func (e *Engine) overrideFile(a store.App) string {
	return filepath.Join(e.DataDir, "env", a.Name+".override.yml")
}

func (e *Engine) composeCmd(ctx context.Context, a store.App, withOverride bool, args ...string) *exec.Cmd {
	dir := filepath.Join(e.checkoutDir(a), a.Subfolder)
	base := []string{"-p", ProjectName(a), "-f", filepath.Join(dir, a.ComposeFile)}
	if _, err := os.Stat(e.overrideFile(a)); err == nil && withOverride {
		base = append(base, "-f", e.overrideFile(a))
	}
	base = append(base, e.envArgs(a, dir)...)
	prefix := e.ComposeCmd
	if len(prefix) == 0 {
		prefix = []string{"docker", "compose"}
	}
	cmd := exec.CommandContext(ctx, prefix[0], append(append(append([]string{}, prefix[1:]...), base...), args...)...)
	cmd.Dir = dir
	return cmd
}

// envArgs lists the --env-file flags. For an existing folder the folder's own .env comes first
// (people edit it by hand) and variables saved in Berth, if any, come after and win.
func (e *Engine) envArgs(a store.App, dir string) []string {
	var args []string
	if a.SourceMode == "existing" {
		if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
			args = append(args, "--env-file", filepath.Join(dir, ".env"))
		}
		if fi, err := os.Stat(e.envFile(a)); err == nil && fi.Size() > 0 {
			args = append(args, "--env-file", e.envFile(a))
		}
		return args
	}
	if _, err := os.Stat(e.envFile(a)); err == nil {
		args = append(args, "--env-file", e.envFile(a))
	}
	return args
}

// updateExisting fast-forwards a folder that already exists on the server. It never resets,
// cleans, force-checks-out or switches branches there, so hand edits (.env, local files) survive.
func updateExisting(dir, branch string, exe func(*exec.Cmd) error, w io.Writer) error {
	if fi, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s is not a git checkout", dir)
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("cannot read the folder's current branch: %w", err)
	}
	if cur := strings.TrimSpace(string(out)); cur != branch {
		return fmt.Errorf("the folder is on branch %q but this app uses %q; Berth never switches branches in your own folder", cur, branch)
	}
	fmt.Fprintln(w, "using the existing folder; updating with fast-forward only (nothing is reset or deleted)")
	if err := exe(exec.Command("git", "-C", dir, "fetch", "--prune", "origin", branch)); err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}
	if err := exe(exec.Command("git", "-C", dir, "merge", "--ff-only", "origin/"+branch)); err != nil {
		return fmt.Errorf("git merge --ff-only failed (local changes to tracked files or a diverged branch stop the update; nothing was changed): %w", err)
	}
	return nil
}

// WriteEnvFile decrypts the app's variables into DataDir/env/<app>.env (mode 0600).
func (e *Engine) WriteEnvFile(ctx context.Context, a store.App) error {
	vars, err := e.Store.ListEnv(ctx, a.ID)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, v := range vars {
		val, err := e.Box.Open(v.Value)
		if err != nil {
			return fmt.Errorf("decrypt %s: %w", v.Key, err)
		}
		b.WriteString(v.Key + "=" + quoteEnv(val) + "\n")
	}
	if err := os.MkdirAll(filepath.Dir(e.envFile(a)), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(e.envFile(a), []byte(b.String()), 0o600); err != nil {
		return err
	}
	return e.writeOverride(ctx, a, vars)
}

var serviceRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// buildOverride returns a compose file that sets each saved variable in every service's
// environment. It holds only ${NAME} references (values stay in the 0600 env file), and an
// environment entry beats the service's env_file, so saved variables win over a .env.
func buildOverride(services, keys []string) string {
	var b strings.Builder
	b.WriteString("# Generated by Berth on every deploy. Holds references only; values live in the env file.\nservices:\n")
	for _, svc := range services {
		b.WriteString("  " + svc + ":\n    environment:\n")
		for _, k := range keys {
			b.WriteString("      " + k + ": \"${" + k + "}\"\n")
		}
	}
	return b.String()
}

// writeOverride makes variables saved in Berth reach the containers, not just compose's
// ${...} interpolation (apps that use env_file: .env would otherwise never see them).
func (e *Engine) writeOverride(ctx context.Context, a store.App, vars []store.EnvVar) error {
	if len(vars) == 0 {
		os.Remove(e.overrideFile(a))
		return nil
	}
	// all services, including ones behind a profile (e.g. a pre-deploy "migrate" service)
	out, err := e.composeCmd(ctx, a, false, "--profile", "*", "config", "--services").Output()
	if err != nil {
		return fmt.Errorf("listing compose services: %w", err)
	}
	var services, keys []string
	for _, svc := range strings.Fields(string(out)) {
		if !serviceRe.MatchString(svc) {
			return fmt.Errorf("unexpected service name %q", svc)
		}
		services = append(services, svc)
	}
	for _, v := range vars {
		keys = append(keys, v.Key) // keys are validated by the database (^[A-Za-z_][A-Za-z0-9_]*$)
	}
	return os.WriteFile(e.overrideFile(a), []byte(buildOverride(services, keys)), 0o600)
}

func quoteEnv(v string) string {
	if !strings.Contains(v, "'") {
		return "'" + v + "'"
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`)
	return `"` + r.Replace(v) + `"`
}

// Idle runs fn only while no deploy or rollback of the app is running (ErrBusy otherwise).
func (e *Engine) Idle(appID int64, fn func() error) error {
	lk := e.lockFor(appID)
	if !lk.TryLock() {
		return ErrBusy
	}
	defer lk.Unlock()
	return fn()
}

// Start begins a deploy in the background and returns the deploy id.
func (e *Engine) Start(ctx context.Context, a store.App) (int64, error) {
	return e.launch(ctx, a, "deploy", func(w io.Writer, id int64) (string, error) {
		return e.run(context.Background(), a, w, id)
	})
}

// Rollback redeploys the images and commit of an earlier successful deploy.
// It does not run the pre-deploy step: database migrations are not undone.
func (e *Engine) Rollback(ctx context.Context, a store.App, targetID int64) (int64, error) {
	t, err := e.Store.DeployByID(ctx, targetID)
	if err != nil {
		return 0, err
	}
	if t == nil || t.AppID != a.ID || t.Status != "success" || !t.ImagesTagged || !shaRe.MatchString(t.CommitSHA) {
		return 0, fmt.Errorf("%w: that deploy cannot be rolled back to", ErrBadInput)
	}
	return e.launch(ctx, a, "rollback", func(w io.Writer, id int64) (string, error) {
		return e.rollback(context.Background(), a, *t, w, id)
	})
}

func (e *Engine) launch(ctx context.Context, a store.App, kind string, work func(io.Writer, int64) (string, error)) (int64, error) {
	if err := ValidateApp(a); err != nil {
		return 0, err
	}
	lk := e.lockFor(a.ID)
	if !lk.TryLock() {
		return 0, ErrBusy
	}
	id, err := e.Store.StartDeploy(ctx, a.ID, kind)
	if err != nil {
		lk.Unlock()
		return 0, err
	}
	log.Printf("%s #%d of %s started", kind, id, a.Name)
	go func() {
		defer lk.Unlock()
		bg := context.Background()
		w := &logWriter{store: e.Store, id: id}
		sha, err := work(w, id)
		status := "success"
		if err != nil {
			status = "failed"
			fmt.Fprintf(w, "\n%s FAILED: %v\n", strings.ToUpper(kind), err)
		} else {
			fmt.Fprintf(w, "\n%s OK\n", strings.ToUpper(kind))
		}
		e.Store.FinishDeploy(bg, id, status, sha)
		log.Printf("%s #%d of %s finished: %s (commit %.8s)", kind, id, a.Name, status, sha)
	}()
	return id, nil
}

func (e *Engine) run(ctx context.Context, a store.App, w io.Writer, deployID int64) (string, error) {
	step := func(name string) { fmt.Fprintf(w, "\n== %s ==\n", name) }
	exe := func(cmd *exec.Cmd) error {
		cmd.Stdout, cmd.Stderr = w, w
		return cmd.Run()
	}

	step("git")
	dir := e.checkoutDir(a)
	if a.SourceMode == "existing" {
		if err := updateExisting(dir, a.Branch, exe, w); err != nil {
			return "", err
		}
	} else if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return "", err
		}
		if err := exe(exec.Command("git", "clone", "--branch", a.Branch, "--", a.RepoURL, dir)); err != nil {
			return "", fmt.Errorf("git clone: %w", err)
		}
	} else {
		for _, args := range [][]string{
			{"-C", dir, "fetch", "--prune", "origin", a.Branch},
			{"-C", dir, "checkout", "--force", a.Branch},
			{"-C", dir, "reset", "--hard", "origin/" + a.Branch},
		} {
			if err := exe(exec.Command("git", args...)); err != nil {
				return "", fmt.Errorf("git %s: %w", args[2], err)
			}
		}
	}
	shaOut, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(shaOut))
	fmt.Fprintf(w, "commit %s\n", sha)

	if _, err := os.Stat(filepath.Join(dir, a.Subfolder, a.ComposeFile)); err != nil {
		return sha, fmt.Errorf("compose file not found in repo: %s", filepath.Join(a.Subfolder, a.ComposeFile))
	}

	step("env")
	if err := e.WriteEnvFile(ctx, a); err != nil {
		return sha, err
	}
	fmt.Fprintln(w, "variables written outside the checkout (values not shown)")
	if a.SourceMode == "existing" {
		fmt.Fprintln(w, "the folder's own .env is used as it is; variables saved in Berth override it")
	}

	step("build")
	if err := exe(e.compose(a, "build")); err != nil {
		return sha, fmt.Errorf("build: %w", err)
	}

	step("keep images for rollback")
	if err := e.tagImages(ctx, a, deployID, w); err != nil {
		return sha, err
	}

	if a.PredeploySvc != "" {
		step("pre-deploy: " + a.PredeploySvc)
		if err := exe(e.compose(a, "run", "--rm", "--no-deps", a.PredeploySvc)); err != nil {
			return sha, fmt.Errorf("pre-deploy step failed, nothing was restarted: %w", err)
		}
	}

	step("up")
	if err := exe(e.compose(a, "up", "-d", "--remove-orphans", "--wait", "--wait-timeout", "180")); err != nil {
		return sha, fmt.Errorf("up/health: %w", err)
	}
	e.Store.MarkImagesTagged(ctx, deployID)
	e.pruneImages(ctx, a, w)
	return sha, nil
}

func (e *Engine) rollback(ctx context.Context, a store.App, t store.Deploy, w io.Writer, deployID int64) (sha string, err error) {
	step := func(name string) { fmt.Fprintf(w, "\n== %s ==\n", name) }
	exe := func(cmd *exec.Cmd) error {
		cmd.Stdout, cmd.Stderr = w, w
		return cmd.Run()
	}
	dir := e.checkoutDir(a)
	step(fmt.Sprintf("rolling back to deploy #%d (%.8s)", t.ID, t.CommitSHA))
	if a.SourceMode == "existing" {
		// Never check out an old commit in the user's own folder: the kept images carry the old code.
		fmt.Fprintln(w, "existing folder: its files are left as they are; restoring the old images only")
	} else {
		prev, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
		defer func() { // a failed rollback must not leave the checkout on the old commit
			if err != nil && shaRe.MatchString(strings.TrimSpace(string(prev))) {
				exec.Command("git", "-C", dir, "checkout", "--force", strings.TrimSpace(string(prev))).Run()
			}
		}()
		if err := exe(exec.Command("git", "-C", dir, "checkout", "--force", t.CommitSHA)); err != nil {
			return t.CommitSHA, fmt.Errorf("git checkout: %w", err)
		}
	}
	if err := e.WriteEnvFile(ctx, a); err != nil {
		return t.CommitSHA, err
	}
	refs, err := e.imageRefs(ctx, a)
	if err != nil {
		return t.CommitSHA, err
	}
	// Check every image exists before touching anything that is running.
	for _, ref := range refs {
		repo, _ := splitRef(ref)
		if err := exec.Command("docker", "image", "inspect", repo+":"+rollbackTag(t.ID)).Run(); err != nil {
			return t.CommitSHA, fmt.Errorf("image %s:%s no longer exists (pruned?); nothing was changed", repo, rollbackTag(t.ID))
		}
	}
	for _, ref := range refs {
		repo, _ := splitRef(ref)
		if err := exe(exec.Command("docker", "tag", repo+":"+rollbackTag(t.ID), ref)); err != nil {
			return t.CommitSHA, err
		}
	}
	step("up (no build)")
	if err := exe(e.composeCtx(ctx, a, "up", "-d", "--no-build", "--remove-orphans", "--wait", "--wait-timeout", "180")); err != nil {
		return t.CommitSHA, fmt.Errorf("up/health: %w", err)
	}
	if err := e.tagImages(ctx, a, deployID, w); err == nil {
		e.Store.MarkImagesTagged(ctx, deployID)
	}
	e.pruneImages(ctx, a, w)
	return t.CommitSHA, nil
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

func rollbackTag(id int64) string { return fmt.Sprintf("berth-%d", id) }

// splitRef splits "repo/name:tag" into repo and tag (tag "latest" if absent).
func splitRef(ref string) (repo, tag string) {
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

// imageRefs lists the image names the compose file uses.
func (e *Engine) imageRefs(ctx context.Context, a store.App) ([]string, error) {
	out, err := e.composeCtx(ctx, a, "config", "--images").Output()
	if err != nil {
		return nil, fmt.Errorf("compose config --images: %w", err)
	}
	seen := map[string]bool{}
	var refs []string
	for _, l := range strings.Fields(string(out)) {
		if !seen[l] {
			seen[l] = true
			refs = append(refs, l)
		}
	}
	return refs, nil
}

// tagImages keeps each current image under <repo>:berth-<deployID>.
func (e *Engine) tagImages(ctx context.Context, a store.App, deployID int64, w io.Writer) error {
	refs, err := e.imageRefs(ctx, a)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		repo, _ := splitRef(ref)
		if out, err := exec.Command("docker", "tag", ref, repo+":"+rollbackTag(deployID)).CombinedOutput(); err != nil {
			return fmt.Errorf("tag %s: %s", ref, strings.TrimSpace(string(out)))
		}
		fmt.Fprintf(w, "kept %s:%s\n", repo, rollbackTag(deployID))
	}
	return nil
}

// pruneImages removes only the berth-<id> tags beyond the newest KeepImages (default 5).
// It never touches volumes, containers or other tags.
func (e *Engine) pruneImages(ctx context.Context, a store.App, w io.Writer) {
	var removed []int64
	defer func() {
		if len(removed) > 0 {
			e.Store.UnmarkImagesTagged(ctx, removed)
		}
	}()
	keep := e.KeepImages
	if keep <= 0 {
		keep = 5
	}
	refs, err := e.imageRefs(ctx, a)
	if err != nil {
		return
	}
	for _, ref := range refs {
		repo, _ := splitRef(ref)
		out, err := exec.Command("docker", "image", "ls", repo, "--format", "{{.Tag}}").Output()
		if err != nil {
			continue
		}
		var ids []int64
		for _, tag := range strings.Fields(string(out)) {
			if n, ok := strings.CutPrefix(tag, "berth-"); ok {
				if id, err := strconv.ParseInt(n, 10, 64); err == nil {
					ids = append(ids, id)
				}
			}
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
		for _, id := range ids[min(keep, len(ids)):] {
			if exec.Command("docker", "rmi", repo+":"+rollbackTag(id)).Run() == nil {
				removed = append(removed, id)
				fmt.Fprintf(w, "removed old %s:%s\n", repo, rollbackTag(id))
			}
		}
	}
}

// LogStream runs `compose logs -f` until ctx is cancelled, writing lines to w.
func (e *Engine) LogStream(ctx context.Context, a store.App, w io.Writer) error {
	cmd := e.composeCtx(ctx, a, "logs", "-f", "--tail", "200", "--no-color", "--timestamps")
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}

// Control runs start, stop or restart for the app's compose project.
func (e *Engine) Control(a store.App, action string) (string, error) {
	switch action {
	case "start", "stop", "restart":
	default:
		return "", fmt.Errorf("%w: action", ErrBadInput)
	}
	out, err := e.compose(a, action).CombinedOutput()
	return string(out), err
}

// Status returns the raw `compose ps` JSON lines (empty if never deployed).
func (e *Engine) Status(a store.App) (string, error) {
	if _, err := os.Stat(filepath.Join(e.checkoutDir(a), a.Subfolder, a.ComposeFile)); err != nil {
		return "", nil
	}
	cmd := e.compose(a, "ps", "-a", "--format", "{{.Service}}\t{{.State}}\t{{.Health}}\t{{.Status}}")
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
		if err != nil { // keep docker's own message, e.g. "Cannot connect to the Docker daemon"
			return "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		return "", errors.New("docker did not answer in 10s")
	}
}

// ServiceState is one container of the app's compose project.
type ServiceState struct{ Service, State, Health, Status string }

// Services lists the app's containers (empty if nothing was ever deployed).
func (e *Engine) Services(a store.App) ([]ServiceState, error) {
	out, err := e.Status(a)
	if err != nil {
		return nil, err
	}
	return parseServices(out), nil
}

func parseServices(out string) []ServiceState {
	var list []ServiceState
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 4 {
			list = append(list, ServiceState{f[0], f[1], f[2], f[3]})
		}
	}
	return list
}

// Summarize reduces the containers to "none" (never deployed / nothing built),
// "running" (at least one container is up) or "stopped" (built, none running).
func Summarize(list []ServiceState) string {
	if len(list) == 0 {
		return "none"
	}
	for _, s := range list {
		if s.State == "running" {
			return "running"
		}
	}
	return "stopped"
}

type logWriter struct {
	store *store.Store
	id    int64
}

func (l *logWriter) Write(p []byte) (int, error) {
	l.store.AppendLog(context.Background(), l.id, strings.ReplaceAll(strings.ToValidUTF8(string(p), "?"), "\x00", ""))
	return len(p), nil
}
