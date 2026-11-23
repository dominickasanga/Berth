// Package discover finds the expected apps on disk. It only reads directory names
// and each folder's .git/config and .git/HEAD; it never runs anything in them.
package discover

import (
	"bufio"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dominickasanga/berth/config"
)

// Candidate is one folder that looks like an expected app.
type Candidate struct {
	Path        string
	Folder      string
	Remote      string // origin URL with any credentials removed; "" if none
	Branch      string
	HasGit      bool
	RemoteMatch bool // origin URL is the expected repo
	NameMatch   bool // folder name matches the expected name or one of its folder_names
}

// Warning explains a half-match: the name fits but the git remote does not.
func (c Candidate) Warning() string {
	switch {
	case c.RemoteMatch:
		return ""
	case !c.HasGit:
		return "folder name matches, but it is not a git checkout"
	case c.Remote == "":
		return "folder name matches, but it has no git remote"
	default:
		return "folder name matches, but its git remote is a different repository: " + c.Remote
	}
}

// PathNote reports a scan path that could not be read.
type PathNote struct{ Path, Problem string }

// Scan looks one level deep in each path. Results are keyed by expected app name.
func Scan(paths []string, expected []config.ExpectedApp) (map[string][]Candidate, []PathNote) {
	found := map[string][]Candidate{}
	var notes []PathNote
	for _, p := range paths {
		entries, err := os.ReadDir(p)
		if err != nil {
			problem := "cannot be read: " + err.Error()
			if os.IsNotExist(err) {
				problem = "does not exist on this machine"
			}
			notes = append(notes, PathNote{p, problem})
			continue
		}
		for _, e := range entries {
			dir := filepath.Join(p, e.Name())
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() { // follows symlinks
				continue
			}
			remote, branch, hasGit := readGit(dir)
			for _, x := range expected {
				nameMatch, remoteMatch := matches(x, e.Name(), remote)
				if nameMatch || remoteMatch {
					found[x.Name] = append(found[x.Name], Candidate{
						Path: dir, Folder: e.Name(), Remote: StripCredentials(remote), Branch: branch,
						HasGit: hasGit, RemoteMatch: remoteMatch, NameMatch: nameMatch,
					})
				}
			}
		}
	}
	for k := range found {
		sort.Slice(found[k], func(i, j int) bool { return found[k][i].Path < found[k][j].Path })
	}
	return found, notes
}

// matches applies the expected-app rule: folder name and/or git remote.
func matches(x config.ExpectedApp, folder, remote string) (nameMatch, remoteMatch bool) {
	nameMatch = strings.EqualFold(folder, x.Name)
	for _, n := range x.FolderNames {
		nameMatch = nameMatch || strings.EqualFold(folder, n)
	}
	if remote != "" {
		n := Normalize(remote)
		remoteMatch = n == Normalize(x.Repo)
		for _, alias := range x.RepoAliases {
			remoteMatch = remoteMatch || n == Normalize(alias)
		}
	}
	return
}

// Other is a scanned folder with a compose file that is not one of the expected apps.
type Other struct {
	Path, Folder, Remote, Branch string
	HasGit                       bool
	Composes                     []string // compose files relative to Path, e.g. "deployments/docker/docker-compose.prod.yml"
	SuggestedName                string
}

var (
	composeRe = regexp.MustCompile(`^(docker-)?compose(\..+)?\.ya?ml$`)
	skipDirs  = map[string]bool{"node_modules": true, ".git": true, "vendor": true, ".nuxt": true, ".output": true,
		"dist": true, "build": true, ".next": true, "target": true, "__pycache__": true, "venv": true, ".venv": true,
		".cache": true, ".devcontainer": true}
	slugRe = regexp.MustCompile(`[^a-z0-9]+`)
)

// Slug turns a folder name into a valid app name (lowercase letters, digits, dashes).
func Slug(folder string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(folder), "-"), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

// composeFiles finds compose files at most two folders below the app folder
// (go-api/docker-compose.yml, deployments/docker/docker-compose.yml), shallowest first.
func composeFiles(dir string) []string {
	var found []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		depth := strings.Count(rel, string(filepath.Separator))
		if d.IsDir() {
			if rel != "." && (skipDirs[d.Name()] || depth >= 2) {
				return filepath.SkipDir
			}
			return nil
		}
		if composeRe.MatchString(d.Name()) {
			found = append(found, rel)
		}
		return nil
	})
	sort.SliceStable(found, func(i, j int) bool {
		di, dj := strings.Count(found[i], "/"), strings.Count(found[j], "/")
		if di != dj {
			return di < dj
		}
		return found[i] < found[j]
	})
	return found
}

// ScanOthers lists folders (one level deep in each path) that have a compose file but match no
// expected app. Folders in skip (paths already registered as apps) are left out.
func ScanOthers(paths []string, expected []config.ExpectedApp, skip map[string]bool) []Other {
	var out []Other
	for _, p := range paths {
		entries, err := os.ReadDir(p)
		if err != nil {
			continue
		}
		for _, e := range entries {
			dir := filepath.Join(p, e.Name())
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() || skip[dir] {
				continue
			}
			remote, branch, hasGit := readGit(dir)
			expectedApp := false
			for _, x := range expected {
				if n, r := matches(x, e.Name(), remote); n || r {
					expectedApp = true
				}
			}
			if expectedApp {
				continue
			}
			if composes := composeFiles(dir); len(composes) > 0 {
				out = append(out, Other{Path: dir, Folder: e.Name(), Remote: StripCredentials(remote), Branch: branch,
					HasGit: hasGit, Composes: composes, SuggestedName: Slug(e.Name())})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Folder) < strings.ToLower(out[j].Folder) })
	return out
}

// Inspect describes one folder against an expected repository URL (for apps not in expected-apps.yaml).
func Inspect(dir, repo string) Candidate {
	remote, branch, hasGit := readGit(dir)
	return Candidate{Path: dir, Folder: filepath.Base(dir), Remote: StripCredentials(remote), Branch: branch,
		HasGit: hasGit, RemoteMatch: remote != "" && Normalize(remote) == Normalize(repo)}
}

// Resolve picks the copy to use: the user's flagged one if it is still found,
// else the only copy. needsChoice is true when there are several and none is flagged.
func Resolve(cands []Candidate, chosenPath string) (c *Candidate, needsChoice bool) {
	for i := range cands {
		if chosenPath != "" && cands[i].Path == chosenPath {
			return &cands[i], false
		}
	}
	switch len(cands) {
	case 0:
		return nil, false
	case 1:
		return &cands[0], false
	}
	return nil, true
}

// readGit reads origin URL and current branch straight from .git, without running git
// (so it works regardless of who owns the folder).
func readGit(dir string) (remote, branch string, hasGit bool) {
	gitDir := filepath.Join(dir, ".git")
	if fi, err := os.Stat(gitDir); err != nil || !fi.IsDir() {
		return "", "", false
	}
	if f, err := os.Open(filepath.Join(gitDir, "config")); err == nil {
		defer f.Close()
		inOrigin := false
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "[") {
				inOrigin = line == `[remote "origin"]`
				continue
			}
			if k, v, ok := strings.Cut(line, "="); ok && inOrigin && strings.TrimSpace(k) == "url" {
				remote = strings.TrimSpace(v)
				break
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(gitDir, "HEAD")); err == nil {
		if ref, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: refs/heads/"); ok {
			branch = ref
		}
	}
	return remote, branch, true
}

// Normalize makes https, ssh and scp-style URLs of the same repo compare equal.
func Normalize(u string) string {
	s := strings.ToLower(strings.TrimSpace(u))
	scp := !strings.Contains(s, "://")
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if at := strings.Index(s, "@"); at >= 0 && at < strings.IndexAny(s+"/", "/") {
		s = s[at+1:]
	}
	if scp {
		s = strings.Replace(s, ":", "/", 1)
	}
	s = strings.TrimRight(s, "/")
	return strings.TrimSuffix(s, ".git")
}

// StripCredentials removes user:password from http(s) URLs so a token never reaches the UI or database.
func StripCredentials(raw string) string {
	if !strings.Contains(raw, "://") {
		return raw // scp-style git@host:path carries no secret
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if u.Scheme == "ssh" {
		u.User = url.User(u.User.Username())
	} else {
		u.User = nil
	}
	return u.String()
}
