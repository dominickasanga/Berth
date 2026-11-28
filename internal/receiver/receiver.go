// Package receiver is the TusK backup receiver, run as `berth receive`.
//
// It accepts resumable (tus) uploads from the backup agents, checks each file's sha256, and files it
// under the path the agent asked for. It speaks exactly the protocol the TusK receiver did (same
// paths, same basic auth, same refusals), so agents already installed on other servers keep working
// without any change. It has no database and no dashboard: the files on disk are the record, and the
// Backups page of `berth serve` reads them.
package receiver

import (
	"crypto/subtle"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	// Embeds the timezone database. A minimal container image has no /usr/share/zoneinfo, so without
	// this every log timestamp silently renders as UTC regardless of TZ.
	_ "time/tzdata"

	"github.com/tus/tusd/v2/pkg/filelocker"
	"github.com/tus/tusd/v2/pkg/filestore"
	"github.com/tus/tusd/v2/pkg/handler"

	"github.com/dominickasanga/berth/internal/receiver/bootstrap"
	"github.com/dominickasanga/berth/internal/receiver/preflight"
	"github.com/dominickasanga/berth/internal/receiver/receive"
	"github.com/dominickasanga/berth/internal/receiver/retain"
)

// reportsDir is where agents ship their run reports. A report is data about a backup, not a backup, so
// it is never subject to retention.
const reportsDir = "reports"

type config struct {
	port            string
	basePath        string
	uploadDir       string // tus scratch space: partial uploads live here
	receivedDir     string // verified files, laid out by their relpath
	clientPath      string
	agentPath       string
	username        string
	password        string
	allowUnverified bool
	minFreeBytes    uint64
	retention       *retain.Policy // nil disables deletion entirely
	retentionDryRun bool
	retentionDirs   map[string]bool // which top-level dirs the policy applies to
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func loadConfig() (*config, error) {
	c := &config{
		port:        envOr("TUSK_PORT", "5183"),
		basePath:    envOr("TUSK_BASE_PATH", "/beam/"),
		uploadDir:   envOr("TUSK_UPLOAD_DIR", "./uploads"),
		receivedDir: envOr("TUSK_RECEIVED_DIR", "./received"),
		clientPath:  envOr("TUSK_CLIENT_BINARY", "./tusk-send"),
		agentPath:   envOr("TUSK_AGENT_BINARY", "./tusk-agent"),
		username:    envOr("TUSK_USER", "admin"),
		password:    os.Getenv("TUSK_PASSWORD"),
	}

	// Refuse to start without a password rather than fall back to a built-in one. A default
	// credential in a backup receiver is a default credential in front of the entire database.
	if c.password == "" {
		return nil, fmt.Errorf("TUSK_PASSWORD is not set; refusing to start")
	}
	c.allowUnverified = os.Getenv("TUSK_ALLOW_UNVERIFIED") == "1"

	// Without the trailing slash the mux registers an exact path rather than a subtree, so uploads
	// would 404.
	if !strings.HasSuffix(c.basePath, "/") {
		c.basePath += "/"
	}

	c.minFreeBytes = 1 << 30 // 1 GiB
	if v := os.Getenv("TUSK_MIN_FREE_BYTES"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("TUSK_MIN_FREE_BYTES: %w", err)
		}
		c.minFreeBytes = n
	}

	// Retention is opt-in. An operator who has not asked for deletion must never get it as a side
	// effect of upgrading.
	if v := os.Getenv("TUSK_RETENTION"); v != "" {
		p, err := parseRetention(v)
		if err != nil {
			return nil, fmt.Errorf("TUSK_RETENTION: %w", err)
		}
		c.retention = p
		c.retentionDryRun = os.Getenv("TUSK_RETENTION_DRYRUN") == "1"

		c.retentionDirs = map[string]bool{}
		for _, name := range strings.Split(envOr("TUSK_RETENTION_DIRS", "backups"), ",") {
			if name = strings.TrimSpace(name); name != "" {
				c.retentionDirs[name] = true
			}
		}
	}
	return c, nil
}

// parseRetention reads a "daily,weekly,monthly" policy such as "7,4,12".
func parseRetention(s string) (*retain.Policy, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 3 {
		return nil, fmt.Errorf("expected daily,weekly,monthly (e.g. 7,4,12), got %q", s)
	}
	nums := make([]int, 3)
	for i, part := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", part)
		}
		if n < 0 {
			return nil, fmt.Errorf("negative count %d", n)
		}
		nums[i] = n
	}
	if nums[0]+nums[1]+nums[2] == 0 {
		return nil, fmt.Errorf("policy 0,0,0 would delete every backup")
	}
	return &retain.Policy{Daily: nums[0], Weekly: nums[1], Monthly: nums[2]}, nil
}

// Main runs `berth receive`. With -check it only runs the startup checks and exits.
func Main(args []string) {
	fs := flag.NewFlagSet("receive", flag.ExitOnError)
	checkOnly := fs.Bool("check", false, "run preflight checks and exit")
	fs.Parse(args)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("❌ %v", err)
	}
	// These belonged to TusK's own dashboard and history database, which `berth serve` replaces.
	for _, name := range []string{"TUSK_ADMIN_PASSWORD", "DB_HOST"} {
		if os.Getenv(name) != "" {
			log.Printf("ℹ️  %s is set but ignored: history and the dashboard are now Berth's Backups page", name)
		}
	}

	// Check the environment before accepting anything. A container that refuses to start is noticed;
	// a nightly upload that fails at 23:30 is not.
	results := preflight.Run(preflight.Config{
		UploadDir:    cfg.uploadDir,
		ReceivedDir:  cfg.receivedDir,
		MinFreeBytes: cfg.minFreeBytes,
		FreeBytes:    receive.FreeBytes,
		Retention:    os.Getenv("TUSK_RETENTION"),
		RetentionDry: cfg.retentionDryRun,
	})
	log.Printf("🔍 Preflight checks:")
	for _, r := range results {
		log.Printf("   %s %-28s %s", r.Level.Symbol(), r.Name, r.Detail)
	}
	if preflight.Fatal(results) {
		log.Fatalf("❌ Preflight failed; refusing to start. Fix the ✗ items above.")
	}
	if *checkOnly {
		log.Printf("✅ Preflight passed.")
		return
	}

	h, err := cfg.handler()
	if err != nil {
		log.Fatalf("❌ %v", err)
	}
	fmt.Printf("🐘 TusK receiver (berth receive): beaming on :%s\n", cfg.port)
	fmt.Printf("🚀 Upload path:  %s\n", cfg.basePath)
	fmt.Printf("📁 Received dir: %s\n", cfg.receivedDir)
	if err := http.ListenAndServe(":"+cfg.port, h); err != nil {
		log.Fatalf("❌ Server error: %v", err)
	}
}

// handler builds the receiver's routes: the tus endpoint plus the sender bootstrap files.
func (c *config) handler() (http.Handler, error) {
	fileStore := filestore.New(c.uploadDir)
	locker := filelocker.New(c.uploadDir)
	composer := handler.NewStoreComposer()
	fileStore.UseIn(composer)
	locker.UseIn(composer)

	tusHandler, err := handler.NewHandler(handler.Config{
		BasePath:      c.basePath,
		StoreComposer: composer,
		// Reject an upload up front when there is no room for it, instead of discovering it at the
		// last chunk.
		PreUploadCreateCallback: c.checkSpace,
		// Runs before the sender receives its final response, so a corrupt transfer is reported to
		// the sender rather than silently stored.
		PreFinishResponseCallback: c.finishUpload,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create tus handler: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle(c.basePath+"get-client", c.auth(http.HandlerFunc(c.serveClient)))
	mux.Handle(c.basePath+"get-agent", c.auth(http.HandlerFunc(c.serveAgent)))
	mux.Handle(c.basePath+"install.sh", c.auth(http.HandlerFunc(c.serveInstaller)))
	mux.Handle(c.basePath, c.auth(http.StripPrefix(c.basePath, tusHandler)))
	return mux, nil
}

// checkSpace refuses uploads that would not fit, keeping a reserve free.
func (c *config) checkSpace(hook handler.HookEvent) (handler.HTTPResponse, handler.FileInfoChanges, error) {
	free, err := receive.FreeBytes(c.uploadDir)
	if err != nil {
		// If we cannot measure the disk, accepting is the lesser risk: a receiver that rejects
		// everything is a backup system that is not running at all.
		log.Printf("⚠️  Cannot determine free space: %v (allowing upload)", err)
		return handler.HTTPResponse{}, handler.FileInfoChanges{}, nil
	}

	needed := uint64(hook.Upload.Size) + c.minFreeBytes
	if hook.Upload.Size > 0 && free < needed {
		log.Printf("🛑 Rejecting %d byte upload: %d bytes free, need %d", hook.Upload.Size, free, needed)
		return handler.HTTPResponse{}, handler.FileInfoChanges{}, handler.NewError(
			"ERR_INSUFFICIENT_STORAGE",
			fmt.Sprintf("insufficient storage: %d bytes free, %d required", free, needed),
			http.StatusInsufficientStorage,
		)
	}
	return handler.HTTPResponse{}, handler.FileInfoChanges{}, nil
}

// finishUpload verifies the checksum and moves the upload to its final path.
func (c *config) finishUpload(hook handler.HookEvent) (handler.HTTPResponse, error) {
	info := hook.Upload
	srcPath := info.Storage[filestore.StorageKeyPath]
	if srcPath == "" {
		return handler.HTTPResponse{}, fmt.Errorf("upload %s has no storage path", info.ID)
	}

	relPath := info.MetaData["relpath"]
	if relPath == "" {
		relPath = info.MetaData["filename"]
	}
	dest, err := receive.SafeJoin(c.receivedDir, relPath)
	if err != nil {
		// 4xx, not 5xx: the path is the sender's mistake and will be identical on every retry. A 500
		// here would have the client re-upload the whole file up to its attempt limit for a request
		// that can never succeed.
		log.Printf("🛑 Rejected upload %s: %v", info.ID, err)
		discard(srcPath)
		return handler.HTTPResponse{}, handler.NewError(
			"ERR_INVALID_RELPATH", err.Error(), http.StatusBadRequest)
	}

	if sum := info.MetaData["sha256"]; sum != "" {
		if err := receive.Verify(srcPath, sum); err != nil {
			// Keep the bad file out of the received tree. The sender sees the error and can retry; a
			// silently stored corrupt backup cannot be distinguished from a good one later. Left as a
			// 5xx deliberately: corruption is usually transient, so retrying is worthwhile.
			log.Printf("🛑 %s failed verification: %v", relPath, err)
			discard(srcPath)
			return handler.HTTPResponse{}, fmt.Errorf("checksum verification failed: %w", err)
		}
	} else if !c.allowUnverified {
		log.Printf("🛑 Rejected %s: no sha256 metadata (set TUSK_ALLOW_UNVERIFIED=1 to permit)", relPath)
		discard(srcPath)
		return handler.HTTPResponse{}, handler.NewError(
			"ERR_NO_CHECKSUM", "upload has no sha256 metadata", http.StatusBadRequest)
	} else {
		log.Printf("⚠️  %s stored WITHOUT verification (no sha256 metadata)", relPath)
	}

	if err := receive.Place(srcPath, dest); err != nil {
		log.Printf("❌ Could not place %s: %v", relPath, err)
		return handler.HTTPResponse{}, err
	}
	os.Remove(srcPath + ".info")

	log.Printf("✅ Received %s (%d bytes) -> %s", relPath, info.Size, dest)

	// A run report is data about the backup, not a backup: it is kept as it arrived and never pruned.
	// Berth's Backups page reads it from here.
	if firstSegment(relPath) == reportsDir {
		log.Printf("📝 Run report %s stored", filepath.Base(relPath))
		return handler.HTTPResponse{}, nil
	}

	// Prune after a successful arrival, never before: the new backup is on disk and verified, so
	// applying retention here can never leave us with fewer good copies than we started with.
	c.prune(filepath.Dir(dest))
	return handler.HTTPResponse{}, nil
}

// prune applies the retention policy to dir, if one is configured.
//
// Deletion is opt-in. Files whose names carry no parseable timestamp are always kept, because "I do
// not understand this file" must never mean "delete it".
func (c *config) prune(dir string) {
	if c.retention == nil {
		return
	}

	// Only prune directories that were explicitly nominated. Relying on filenames alone would mean a
	// future change to the incremental archive naming could silently bring media archives into scope
	// of a policy written for nightly database dumps.
	if !c.retentionDirs[filepath.Base(dir)] {
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("⚠️  Retention: cannot read %s: %v", dir, err)
		return
	}

	var items []retain.Item
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ts, err := retain.ParseTimestamp(e.Name())
		if err != nil {
			continue // unparseable: keep
		}
		items = append(items, retain.Item{Name: e.Name(), Time: ts})
	}
	if len(items) == 0 {
		return
	}

	keep, prune := retain.Select(items, *c.retention)
	if len(prune) == 0 {
		return
	}

	deleted := 0
	for _, item := range prune {
		target := filepath.Join(dir, item.Name)
		if c.retentionDryRun {
			log.Printf("🧹 [dry-run] would delete %s", target)
			deleted++
			continue
		}
		if err := os.Remove(target); err != nil {
			log.Printf("⚠️  Retention: cannot delete %s: %v", target, err)
			continue
		}
		deleted++
		log.Printf("🧹 Deleted %s (retention)", item.Name)
	}
	log.Printf("🧹 Retention in %s: kept %d, pruned %d", dir, len(keep), deleted)
}

// discard removes a rejected upload and its sidecar. Without this, every rejected transfer leaves its
// full payload behind and the scratch directory grows until the disk is gone.
func discard(srcPath string) {
	os.Remove(srcPath)
	os.Remove(srcPath + ".info")
}

// firstSegment returns the leading path element, which is the source name the agent shipped the file
// under.
func firstSegment(relPath string) string {
	parts := strings.Split(filepath.ToSlash(relPath), "/")
	if len(parts) > 1 {
		return parts[0]
	}
	return ""
}

// serveInstaller returns the sender installer with this receiver's URL filled in, so the script a host
// downloads already points back at the right place.
func (c *config) serveInstaller(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Write([]byte(bootstrap.InstallScript(bootstrap.ServerURL(r, c.basePath))))
}

func (c *config) serveClient(w http.ResponseWriter, r *http.Request) {
	serveBinary(w, r, c.clientPath, "tusk-send")
}

func (c *config) serveAgent(w http.ResponseWriter, r *http.Request) {
	serveBinary(w, r, c.agentPath, "tusk-agent")
}

// serveBinary hands a sender-side executable to an authenticated machine. The binaries are built
// from the TusK repository and mounted into the receiver.
func serveBinary(w http.ResponseWriter, r *http.Request, path, name string) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		http.Error(w, "Binary not found. Build "+name+" first.", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename="+filepath.Base(path))
	http.ServeFile(w, r, path)
}

// auth guards every route. Comparison is constant-time so that response timing does not leak how much
// of the password was correct.
func (c *config) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 %s -> %s", r.Method, r.URL.Path)

		user, pass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(c.username)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(c.password)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="TusK"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
