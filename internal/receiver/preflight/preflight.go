// Package preflight verifies the receiver's environment before it starts
// accepting uploads.
//
// The failures this catches — an unwritable volume, a cross-device layout, a
// full disk — otherwise surface at 23:30 in the middle of a transfer, hours
// after anyone was watching. Checking at startup turns a silent nightly failure
// into a container that refuses to come up, which is the kind of failure an
// operator actually notices.
package preflight

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Level distinguishes "this will not work" from "this will work badly".
type Level int

const (
	Pass Level = iota
	Warn
	Fail
)

func (l Level) Symbol() string {
	switch l {
	case Pass:
		return "✓"
	case Warn:
		return "⚠"
	default:
		return "✗"
	}
}

// Result is one check's outcome.
type Result struct {
	Name   string
	Level  Level
	Detail string
}

// Writable proves a directory is usable by writing to it, rather than trusting
// a stat.
//
// Checking permission bits is not the same as having permission: a container
// running as a non-root user against a volume owned by root passes every
// os.Stat inspection and then fails on the first real write. The only honest
// test is to create a file.
func Writable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cannot create: %w", err)
	}
	probe := filepath.Join(dir, ".tusk-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("cannot write (uid %d): %w", os.Getuid(), err)
	}
	if _, err := f.WriteString("ok"); err != nil {
		f.Close()
		os.Remove(probe)
		return fmt.Errorf("cannot write: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(probe)
		return err
	}
	return os.Remove(probe)
}

// SameDevice reports whether two paths live on one filesystem.
func SameDevice(a, b string) (bool, error) {
	var sa, sb syscall.Stat_t
	if err := syscall.Stat(a, &sa); err != nil {
		return false, err
	}
	if err := syscall.Stat(b, &sb); err != nil {
		return false, err
	}
	return sa.Dev == sb.Dev, nil
}

// Config is what the checks need to know.
type Config struct {
	UploadDir    string
	ReceivedDir  string
	MinFreeBytes uint64
	FreeBytes    func(string) (uint64, error)
	Retention    string // empty when disabled
	RetentionDry bool
}

// Run performs every check and returns the results in report order.
func Run(cfg Config) []Result {
	var out []Result

	for _, d := range []struct{ label, path string }{
		{"Upload directory writable", cfg.UploadDir},
		{"Received directory writable", cfg.ReceivedDir},
	} {
		if err := Writable(d.path); err != nil {
			out = append(out, Result{d.label, Fail, fmt.Sprintf("%s: %v", d.path, err)})
		} else {
			out = append(out, Result{d.label, Pass, d.path})
		}
	}

	// Cross-device is not fatal — the code copies instead of renaming — but on a
	// multi-gigabyte archive that is minutes of avoidable I/O per arrival, and
	// the file is briefly duplicated on disk.
	if same, err := SameDevice(cfg.UploadDir, cfg.ReceivedDir); err == nil {
		if same {
			out = append(out, Result{"Atomic placement", Pass, "upload and received dirs share a filesystem"})
		} else {
			out = append(out, Result{"Atomic placement", Warn,
				"upload and received dirs are on DIFFERENT filesystems; every arrival will be copied instead of renamed. Put both on one volume."})
		}
	}

	if cfg.FreeBytes != nil {
		if free, err := cfg.FreeBytes(cfg.UploadDir); err != nil {
			out = append(out, Result{"Free space", Warn, fmt.Sprintf("cannot determine: %v", err)})
		} else if free < cfg.MinFreeBytes {
			out = append(out, Result{"Free space", Fail,
				fmt.Sprintf("%s free, below the %s reserve; uploads would be refused immediately",
					humanBytes(free), humanBytes(cfg.MinFreeBytes))})
		} else {
			out = append(out, Result{"Free space", Pass,
				fmt.Sprintf("%s free (reserve %s)", humanBytes(free), humanBytes(cfg.MinFreeBytes))})
		}
	}

	switch {
	case cfg.Retention == "":
		out = append(out, Result{"Retention", Warn, "disabled; received files accumulate until the disk fills"})
	case cfg.RetentionDry:
		out = append(out, Result{"Retention", Pass, cfg.Retention + " (dry run: nothing is deleted)"})
	default:
		out = append(out, Result{"Retention", Pass, cfg.Retention + " (deleting)"})
	}

	return out
}

// Fatal reports whether any check failed.
func Fatal(results []Result) bool {
	for _, r := range results {
		if r.Level == Fail {
			return true
		}
	}
	return false
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for k := n / unit; k >= unit; k /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
