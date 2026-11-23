package logs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHubKeepsRecentLinesAndSplitsWrites(t *testing.T) {
	h := NewHub(3)
	h.Write([]byte("one\ntwo\nthr"))
	h.Write([]byte("ee\nfour\r\nfive\n"))
	recent, _, cancel := h.Subscribe(10)
	defer cancel()
	if got := strings.Join(recent, "|"); got != "three|four|five" {
		t.Fatalf("only the newest 3 complete lines should be kept: %q", got)
	}
	if recent2, _, c2 := h.Subscribe(2); strings.Join(recent2, "|") != "four|five" {
		t.Fatalf("Subscribe(2): %v", recent2)
	} else {
		c2()
	}
}

func TestHubStreamsLiveLinesWithoutGapOrDuplicate(t *testing.T) {
	h := NewHub(10)
	h.Write([]byte("before\n"))
	recent, live, cancel := h.Subscribe(10)
	h.Write([]byte("after\n"))
	if len(recent) != 1 || recent[0] != "before" {
		t.Fatalf("recent: %v", recent)
	}
	select {
	case l := <-live:
		if l != "after" {
			t.Fatalf("live line: %q", l)
		}
	case <-time.After(time.Second):
		t.Fatal("no live line")
	}
	cancel()
	h.Write([]byte("nobody listening\n")) // must not block or panic after cancel
}

func TestHubSlowSubscriberDoesNotBlockLogging(t *testing.T) {
	h := NewHub(5)
	_, _, cancel := h.Subscribe(1)
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ { // far more than the 256-line channel, nobody reading
			h.Write([]byte(fmt.Sprintf("line %d\n", i)))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("logging blocked on a slow viewer")
	}
}

func TestFileRotatesBySizeAndKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs", "berth.log")
	f, err := OpenFile(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := 1; i <= 12; i++ {
		f.Write([]byte(fmt.Sprintf("entry %02d %s\n", i, strings.Repeat("x", 20)))) // 30 bytes each
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	cur, old1, old2 := read(path), read(path+".1"), read(path+".2")
	if !strings.Contains(cur, "entry 12") || !strings.Contains(old1, "entry") || old2 == "" {
		t.Fatalf("rotation did not produce current + 2 old files:\ncur=%q\n.1=%q\n.2=%q", cur, old1, old2)
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Fatal("more than `keep` old files were kept")
	}
	for _, p := range []string{path, path + ".1", path + ".2"} {
		if fi, _ := os.Stat(p); fi.Size() > 100 {
			t.Errorf("%s is %d bytes, over the 100-byte limit", p, fi.Size())
		}
	}
	// newest entries survive, oldest are dropped
	all := old2 + old1 + cur
	if !strings.Contains(all, "entry 12") || strings.Contains(all, "entry 01") {
		t.Fatalf("the oldest entries should have been dropped:\n%s", all)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Errorf("log file mode is %v, want 0640", fi.Mode().Perm())
	}
}
