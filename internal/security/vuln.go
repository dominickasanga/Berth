// Package security looks for problems in what Berth runs: known vulnerabilities in the Go
// packages of an app, risky container settings, and secrets committed to a repository.
// It only reads. Nothing is changed and no secret value is ever read into a result.
package security

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

// VulnDBURL is the public Go vulnerability database. Only its generic index (the same file for
// everyone) and the entries of vulnerabilities that match are downloaded: the list of modules an app
// uses is compared on this server and is never sent anywhere.
const VulnDBURL = "https://vuln.go.dev"

// Vuln is one known vulnerability that affects a version in use.
type Vuln struct {
	ID      string   `json:"id"`
	Summary string   `json:"summary"`
	Aliases []string `json:"aliases,omitempty"` // CVE and GHSA numbers
	Fixed   string   `json:"fixed,omitempty"`   // the first version without it, if there is one
	URL     string   `json:"url"`
}

// VulnFinding is one module an app uses that has at least one known vulnerability.
type VulnFinding struct {
	Module   string `json:"module"`
	Version  string `json:"version"`
	Indirect bool   `json:"indirect,omitempty"`
	Vulns    []Vuln `json:"vulns"`
}

// UpdateTo is the version that fixes all of this module's vulnerabilities that have a fix, or "" when none has.
func (f VulnFinding) UpdateTo() string {
	best := ""
	for _, v := range f.Vulns {
		if v.Fixed == "" {
			continue
		}
		if best == "" || semver.Compare(sv(v.Fixed), sv(best)) > 0 {
			best = v.Fixed
		}
	}
	return best
}

// NoFix counts the vulnerabilities that have no fixed version yet.
func (f VulnFinding) NoFix() int {
	n := 0
	for _, v := range f.Vulns {
		if v.Fixed == "" {
			n++
		}
	}
	return n
}

// VulnResult is the outcome of checking one go.mod.
type VulnResult struct {
	GoMod    string        `json:"go_mod"` // path relative to the app folder
	Modules  int           `json:"modules"`
	Findings []VulnFinding `json:"findings"`
}

type indexVuln struct {
	ID    string `json:"id"`
	Fixed string `json:"fixed"`
}

type indexModule struct {
	Path  string      `json:"path"`
	Vulns []indexVuln `json:"vulns"`
}

type osvEntry struct {
	ID       string   `json:"id"`
	Summary  string   `json:"summary"`
	Aliases  []string `json:"aliases"`
	Affected []struct {
		Package struct {
			Name string `json:"name"`
		} `json:"package"`
		Ranges []struct {
			Type   string `json:"type"`
			Events []struct {
				Introduced   string `json:"introduced"`
				Fixed        string `json:"fixed"`
				LastAffected string `json:"last_affected"`
			} `json:"events"`
		} `json:"ranges"`
	} `json:"affected"`
	DatabaseSpecific struct {
		URL string `json:"url"`
	} `json:"database_specific"`
}

// VulnChecker compares go.mod files with the vulnerability database. The database index is kept for
// an hour, so checking several apps in a row downloads it once.
type VulnChecker struct {
	BaseURL string // VulnDBURL unless a test points it elsewhere
	Client  *http.Client

	mu      sync.Mutex
	index   map[string][]indexVuln
	fetched time.Time
	entries map[string]*osvEntry
}

func (c *VulnChecker) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return VulnDBURL
}

func (c *VulnChecker) get(ctx context.Context, path string, into any) error {
	cl := c.Client
	if cl == nil {
		cl = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.base()+path, nil)
	if err != nil {
		return err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("the vulnerability database answered %s for %s", resp.Status, path)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(into)
}

func (c *VulnChecker) loadIndex(ctx context.Context) (map[string][]indexVuln, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.index != nil && time.Since(c.fetched) < time.Hour {
		return c.index, nil
	}
	var mods []indexModule
	if err := c.get(ctx, "/index/modules.json", &mods); err != nil {
		return nil, err
	}
	c.index = make(map[string][]indexVuln, len(mods))
	for _, m := range mods {
		c.index[m.Path] = m.Vulns
	}
	c.fetched = time.Now()
	c.entries = map[string]*osvEntry{}
	return c.index, nil
}

func (c *VulnChecker) entry(ctx context.Context, id string) (*osvEntry, error) {
	c.mu.Lock()
	if e, ok := c.entries[id]; ok {
		c.mu.Unlock()
		return e, nil
	}
	c.mu.Unlock()
	var e osvEntry
	if err := c.get(ctx, "/ID/"+id+".json", &e); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.entries[id] = &e
	c.mu.Unlock()
	return &e, nil
}

// semver turns the database's "1.2.3" (no leading v) into Go's "v1.2.3"; "0" is the very first version.
func sv(v string) string {
	if v == "0" {
		return "v0.0.0"
	}
	if !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}

// affectedBy reports whether version falls in a range of the entry for the module, and the version
// that fixes that range ("" when nothing does).
func affectedBy(e *osvEntry, module, version string) (bool, string) {
	for _, a := range e.Affected {
		if a.Package.Name != module {
			continue
		}
		for _, r := range a.Ranges {
			if r.Type != "SEMVER" {
				continue
			}
			intro := ""
			for _, ev := range r.Events {
				switch {
				case ev.Introduced != "":
					intro = sv(ev.Introduced)
				case ev.Fixed != "":
					if intro != "" && semver.Compare(version, intro) >= 0 && semver.Compare(version, sv(ev.Fixed)) < 0 {
						return true, strings.TrimPrefix(sv(ev.Fixed), "v")
					}
					intro = ""
				case ev.LastAffected != "":
					if intro != "" && semver.Compare(version, intro) >= 0 && semver.Compare(version, sv(ev.LastAffected)) <= 0 {
						return true, ""
					}
					intro = ""
				}
			}
			if intro != "" && semver.Compare(version, intro) >= 0 { // introduced and never fixed
				return true, ""
			}
		}
	}
	return false, ""
}

// CheckGoMod finds the known vulnerabilities in the modules a go.mod requires. A module that
// is listed here is not necessarily called by the code: this checks the versions in use, which
// is the reason to update, but it is not proof that an attacker can reach the flaw.
func (c *VulnChecker) CheckGoMod(ctx context.Context, name string, data []byte) (*VulnResult, error) {
	f, err := modfile.Parse(name, data, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", name, err)
	}
	index, err := c.loadIndex(ctx)
	if err != nil {
		return nil, err
	}
	res := &VulnResult{GoMod: name, Modules: len(f.Require)}
	for _, rq := range f.Require {
		recs := index[rq.Mod.Path]
		if len(recs) == 0 || !semver.IsValid(rq.Mod.Version) {
			continue
		}
		// An entry matters unless this version is at or past every fix the index lists for it.
		need := map[string]bool{}
		for _, r := range recs {
			if r.Fixed == "" || semver.Compare(rq.Mod.Version, sv(r.Fixed)) < 0 {
				need[r.ID] = true
			}
		}
		var found []Vuln
		for id := range need {
			e, err := c.entry(ctx, id)
			if err != nil {
				return nil, err
			}
			if hit, fixed := affectedBy(e, rq.Mod.Path, rq.Mod.Version); hit {
				u := e.DatabaseSpecific.URL
				if u == "" {
					u = "https://pkg.go.dev/vuln/" + id
				}
				found = append(found, Vuln{ID: id, Summary: e.Summary, Aliases: e.Aliases, Fixed: fixed, URL: u})
			}
		}
		if len(found) > 0 {
			sort.Slice(found, func(i, j int) bool { return found[i].ID > found[j].ID })
			res.Findings = append(res.Findings, VulnFinding{Module: rq.Mod.Path, Version: rq.Mod.Version, Indirect: rq.Indirect, Vulns: found})
		}
	}
	sort.Slice(res.Findings, func(i, j int) bool { return res.Findings[i].Module < res.Findings[j].Module })
	return res, nil
}

// FindGoMods lists the go.mod files of an app folder: the folder itself and its subfolders up to two
// levels down (Kite360's lives in go-api/), leaving out dependencies and hidden folders.
func FindGoMods(dir string, glob func(string) ([]string, error)) []string {
	var out []string
	for _, pattern := range []string{"go.mod", "*/go.mod", "*/*/go.mod"} {
		matches, err := glob(filepath.Join(dir, pattern))
		if err != nil {
			continue
		}
		for _, m := range matches {
			rel, err := filepath.Rel(dir, m)
			if err != nil {
				continue
			}
			skip := false
			for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
				if strings.HasPrefix(part, ".") || part == "node_modules" || part == "vendor" || part == "testdata" {
					skip = true
				}
			}
			if !skip {
				out = append(out, rel)
			}
		}
	}
	sort.Strings(out)
	return out
}
