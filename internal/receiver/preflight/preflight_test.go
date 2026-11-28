package preflight

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWritableOnGoodDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "created", "nested")
	if err := Writable(dir); err != nil {
		t.Fatalf("expected writable, got %v", err)
	}
	// The probe must not be left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("probe file left behind: %v", entries)
	}
}

// A read-only directory must be reported as unusable. This is the container
// case: the volume exists and stats fine, but the process cannot write to it.
func TestWritableOnReadOnlyDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, which bypasses permission bits")
	}
	if runtime.GOOS == "windows" {
		t.Skip("permission model differs")
	}

	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil { // r-x, no write
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })

	if err := Writable(locked); err == nil {
		t.Fatal("expected a read-only directory to be reported as not writable")
	}
}

func TestSameDevice(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	for _, d := range []string{a, b} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	same, err := SameDevice(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if !same {
		t.Error("two dirs in one temp dir should share a filesystem")
	}
}

func TestRunFlagsUnwritableAsFatal(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })

	results := Run(Config{
		UploadDir:   filepath.Join(locked, "uploads"),
		ReceivedDir: filepath.Join(dir, "received"),
	})
	if !Fatal(results) {
		t.Fatal("an unwritable upload directory must be fatal")
	}
}

// A healthy configuration must not be fatal, or the container would refuse to
// start for no reason.
func TestRunHealthyIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	results := Run(Config{
		UploadDir:    filepath.Join(dir, "uploads"),
		ReceivedDir:  filepath.Join(dir, "received"),
		MinFreeBytes: 1,
		FreeBytes:    func(string) (uint64, error) { return 1 << 40, nil },
		Retention:    "7,4,12",
	})
	if Fatal(results) {
		for _, r := range results {
			t.Logf("%s %s: %s", r.Level.Symbol(), r.Name, r.Detail)
		}
		t.Fatal("healthy config should not be fatal")
	}
}

// Not enough disk to honour the reserve means every upload would be refused,
// so it is better to fail at startup than to accept the container as healthy.
func TestRunFlagsFullDiskAsFatal(t *testing.T) {
	dir := t.TempDir()
	results := Run(Config{
		UploadDir:    filepath.Join(dir, "uploads"),
		ReceivedDir:  filepath.Join(dir, "received"),
		MinFreeBytes: 1 << 30,
		FreeBytes:    func(string) (uint64, error) { return 1 << 20, nil },
	})
	if !Fatal(results) {
		t.Fatal("free space below the reserve must be fatal")
	}
}
