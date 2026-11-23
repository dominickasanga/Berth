// Package discover finds the expected apps on disk. It only reads directory names
// and each folder's .git/config and .git/HEAD; it never runs anything in them.
package discover

import (
	"bufio"
	"net/url"
	"os"
	"path/filepath"
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
				nameMatch := strings.EqualFold(e.Name(), x.Name)
				for _, n := range x.FolderNames {
					nameMatch = nameMatch || strings.EqualFold(e.Name(), n)
				}
				remoteMatch := remote != "" && Normalize(remote) == Normalize(x.Repo)
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
