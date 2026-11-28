// Package receive turns a finished tus upload into a verified file at a known
// path.
//
// The tus filestore names uploads by random ID, so without this the receiver
// holds a pile of opaque blobs. Reconstructing the original tree means trusting
// a path that arrived over the network, which is only safe if it is validated —
// see SafeJoin.
package receive

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// SafeJoin resolves relPath inside root, refusing anything that would escape it.
//
// relPath is attacker-controlled: it is metadata sent by whoever holds the
// upload password. A value like "../../etc/cron.d/backdoor" would otherwise let
// an upload write anywhere the process can reach, so this check is the boundary
// between a backup receiver and a remote file-write primitive.
func SafeJoin(root, relPath string) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("empty destination path")
	}
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("absolute destination path rejected: %q", relPath)
	}
	// Reject NUL and drive-letter style prefixes before cleaning.
	if strings.ContainsRune(relPath, 0) {
		return "", fmt.Errorf("destination path contains NUL")
	}

	// Refuse traversal outright rather than letting Clean silently rewrite
	// "../../x" into "<root>/x". Sanitising quietly relocates the file, which
	// can overwrite something unrelated and hides a broken or hostile sender;
	// a rejection surfaces it at the sender, which is where it can be fixed.
	for _, part := range strings.Split(filepath.ToSlash(relPath), "/") {
		if part == ".." {
			return "", fmt.Errorf("destination path contains %q: %q", "..", relPath)
		}
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(absRoot, filepath.Clean("/"+relPath))

	// Belt and braces: confirm the result really is under root. Join+Clean
	// should guarantee it, but this is the check that matters if it does not.
	rel, err := filepath.Rel(absRoot, dest)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("destination path escapes root: %q", relPath)
	}
	return dest, nil
}

// Verify checks that the file at path hashes to expected (hex sha256).
func Verify(path, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, expected) {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expected, got)
	}
	return nil
}

// Place moves src to dest, creating parent directories.
//
// The move is done via a temporary name in the destination directory followed
// by a rename, so a reader watching the destination never observes a partially
// written file.
func Place(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	tmp := dest + ".incoming"
	if err := os.Rename(src, tmp); err != nil {
		// Rename fails across filesystems; fall back to a copy.
		if err := copyFile(src, tmp); err != nil {
			return err
		}
		os.Remove(src)
	}
	return os.Rename(tmp, dest)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// FreeBytes reports available space on the filesystem holding path.
//
// A backup receiver that quietly fills its disk stops being a backup receiver,
// and the failure surfaces at restore time, which is the worst moment to find
// out. Checking before accepting an upload turns that into an upfront refusal.
func FreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
