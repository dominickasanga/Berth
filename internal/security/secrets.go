package security

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SecretFile is a file in an app's repository that looks like it holds a secret. Only its name and the
// reason are recorded: the contents are never stored or shown.
type SecretFile struct {
	Path    string `json:"path"`
	Reason  string `json:"reason"`
	Tracked bool   `json:"tracked"` // still in the repository; false: it was committed once and removed, but is in the history
}

var envExamples = map[string]bool{".env.example": true, ".env.sample": true, ".env.template": true, ".env.dist": true, ".env.defaults": true}

// classify says why a file name looks like a secret, or "" when it does not.
func classify(p string) string {
	base := strings.ToLower(path.Base(p))
	switch {
	case envExamples[base]:
		return ""
	case base == ".env" || strings.HasPrefix(base, ".env."):
		return "environment file: usually holds passwords and keys"
	case strings.HasSuffix(base, ".bak") || strings.HasSuffix(base, ".backup") || strings.HasSuffix(base, ".orig"):
		return "backup copy of a file (may be a copy of a configuration with secrets)"
	case strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") || strings.HasSuffix(base, ".p12") || strings.HasSuffix(base, ".pfx") || strings.HasSuffix(base, ".jks") || base == "id_rsa" || base == "id_ed25519" || base == "id_ecdsa":
		return "private key or certificate bundle"
	case base == "cookies.txt" || strings.HasSuffix(base, ".cookies"):
		return "saved cookies: can contain a logged-in session"
	case base == ".htpasswd" || base == ".pgpass" || base == ".netrc" || base == ".npmrc":
		return "credentials file"
	case strings.HasSuffix(base, ".dump") || strings.HasSuffix(base, ".sqlite") || strings.HasSuffix(base, ".sqlite3") || strings.HasSuffix(base, ".sql.gz"):
		return "database dump or database file: real data"
	}
	return ""
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", append([]string{"-C", dir, "-c", "safe.directory=" + dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return string(out), nil
}

// looksLikeServiceAccount is true for a small tracked JSON file that holds a cloud service account key.
func looksLikeServiceAccount(file string) bool {
	st, err := os.Stat(file)
	if err != nil || st.Size() > 64<<10 {
		return false
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, `"private_key"`) && strings.Contains(s, `"client_email"`)
}

// RepoSecrets looks through the files git tracks in dir, and through every file name that was ever
// committed, for names that look like secrets.
func RepoSecrets(ctx context.Context, dir string) ([]SecretFile, error) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return nil, fmt.Errorf("%s is not a git repository", dir)
	}
	tracked, err := git(ctx, dir, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	var out []SecretFile
	trackedSet := map[string]bool{}
	for _, p := range strings.Split(tracked, "\x00") {
		if p == "" {
			continue
		}
		trackedSet[p] = true
		if reason := classify(p); reason != "" {
			out = append(out, SecretFile{Path: p, Reason: reason, Tracked: true})
		} else if strings.HasSuffix(strings.ToLower(p), ".json") && looksLikeServiceAccount(filepath.Join(dir, p)) {
			out = append(out, SecretFile{Path: p, Reason: "service account key (private key and account e-mail)", Tracked: true})
		}
	}
	if hist, err := git(ctx, dir, "log", "--all", "--name-only", "--pretty=format:"); err == nil {
		seen := map[string]bool{}
		for _, p := range strings.Split(hist, "\n") {
			p = strings.TrimSpace(p)
			if p == "" || seen[p] || trackedSet[p] {
				continue
			}
			seen[p] = true
			if reason := classify(p); reason != "" {
				out = append(out, SecretFile{Path: p, Reason: reason, Tracked: false})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Tracked != out[j].Tracked {
			return out[i].Tracked
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}
