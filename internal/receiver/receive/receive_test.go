package receive

import (
	"path/filepath"
	"testing"
)

func TestSafeJoinRejectsEscapes(t *testing.T) {
	root := t.TempDir()

	// Every one of these is a path an upload could claim. None may resolve
	// outside root.
	for _, relPath := range []string{
		"../escaped",
		"../../escaped",
		"a/../../escaped",
		"a/b/../../../escaped",
		"/etc/cron.d/backdoor",
		"/absolute",
		"",
		"..",
		"./../../escaped",
		"a/./../../escaped",
	} {
		t.Run(relPath, func(t *testing.T) {
			// These must be REJECTED, not quietly sanitised into some other
			// path inside root: silent rewriting can clobber an unrelated file
			// and hides the misbehaving sender.
			dest, err := SafeJoin(root, relPath)
			if err == nil {
				t.Fatalf("SafeJoin(%q) was accepted and resolved to %q; expected rejection", relPath, dest)
			}
		})
	}
}

func TestSafeJoinAllowsNormalPaths(t *testing.T) {
	root := t.TempDir()
	absRoot, _ := filepath.Abs(root)

	cases := map[string]string{
		"backup.dump":                 filepath.Join(absRoot, "backup.dump"),
		"documents/report.pdf":        filepath.Join(absRoot, "documents", "report.pdf"),
		"profile_images/u/avatar.png": filepath.Join(absRoot, "profile_images", "u", "avatar.png"),
		"a/b/c/d/deep.tar.gz":         filepath.Join(absRoot, "a", "b", "c", "d", "deep.tar.gz"),
		"documents/with space/x.pdf":  filepath.Join(absRoot, "documents", "with space", "x.pdf"),
	}
	for relPath, want := range cases {
		got, err := SafeJoin(root, relPath)
		if err != nil {
			t.Fatalf("SafeJoin(%q) unexpected error: %v", relPath, err)
		}
		if got != want {
			t.Errorf("SafeJoin(%q) = %q, want %q", relPath, got, want)
		}
	}
}

func TestSafeJoinRejectsNUL(t *testing.T) {
	if _, err := SafeJoin(t.TempDir(), "ok\x00/../../escaped"); err == nil {
		t.Fatal("expected NUL in path to be rejected")
	}
}
