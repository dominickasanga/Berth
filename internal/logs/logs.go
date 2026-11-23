// Package logs is Berth's own logging: a bounded in-memory history that the Logs page can
// stream, and a size-rotated log file.
package logs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Hub keeps the most recent log lines and fans new ones out to live subscribers.
// It implements io.Writer so it can be handed to the standard log package.
type Hub struct {
	mu      sync.Mutex
	buf     []string
	max     int
	partial string
	subs    map[chan string]struct{}
}

func NewHub(max int) *Hub { return &Hub{max: max, subs: map[chan string]struct{}{}} }

func (h *Hub) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	parts := strings.Split(h.partial+string(p), "\n")
	h.partial = parts[len(parts)-1]
	for _, line := range parts[:len(parts)-1] {
		line = strings.TrimRight(line, "\r")
		h.buf = append(h.buf, line)
		if len(h.buf) > h.max {
			h.buf = h.buf[len(h.buf)-h.max:]
		}
		for ch := range h.subs {
			select {
			case ch <- line:
			default: // a slow viewer drops lines rather than blocking logging
			}
		}
	}
	return len(p), nil
}

// Subscribe returns the last n lines and a channel of lines logged after them, with no gap
// or duplicate in between. Call cancel when done.
func (h *Hub) Subscribe(n int) (recent []string, live <-chan string, cancel func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	start := len(h.buf) - n
	if start < 0 {
		start = 0
	}
	recent = append([]string(nil), h.buf[start:]...)
	ch := make(chan string, 256)
	h.subs[ch] = struct{}{}
	return recent, ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// File is a log file that rotates by size: path, path.1 (newest old file) ... path.<keep>.
type File struct {
	mu   sync.Mutex
	path string
	max  int64
	keep int
	f    *os.File
	size int64
}

func OpenFile(path string, maxBytes int64, keep int) (*File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	r := &File{path: path, max: maxBytes, keep: keep}
	return r, r.open()
}

func (r *File) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, fi.Size()
	return nil
}

func (r *File) rotate() error {
	r.f.Close()
	os.Remove(fmt.Sprintf("%s.%d", r.path, r.keep))
	for i := r.keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
	}
	if r.keep >= 1 {
		os.Rename(r.path, r.path+".1")
	} else {
		os.Remove(r.path)
	}
	return r.open()
}

func (r *File) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size > 0 && r.size+int64(len(p)) > r.max {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *File) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
