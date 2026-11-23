package web

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"
)

type reqInfo struct{ User string }

type reqInfoKey struct{}

// setLoggedUser lets the auth middleware tell the request logger who made the request.
func setLoggedUser(ctx context.Context, user string) {
	if ri, ok := ctx.Value(reqInfoKey{}).(*reqInfo); ok {
		ri.User = user
	}
}

// statusWriter records the status and bytes while still supporting streaming (Flush).
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logRequests writes one line per request: method, path, status, time, user and address.
//
//	mode "quiet" (default): skips static files and the pages' own background polling (htmx GETs)
//	                        unless they fail, so real traffic stays readable
//	mode "all":             logs everything
//	mode "off":             logs nothing
//
// Only the path is logged, never the query string or a body, so flash messages and form values
// (passwords, secrets) cannot reach the log. Long-lived streams are logged when they end.
func logRequests(mode string, base string, next http.Handler) http.Handler {
	if mode == "off" {
		return next
	}
	staticPrefix := base + "/static/"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ri := &reqInfo{}
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, ri)))
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		if mode != "all" && status < 400 {
			if strings.HasPrefix(r.URL.Path, staticPrefix) {
				return
			}
			if r.Method == http.MethodGet && r.Header.Get("HX-Request") != "" {
				return
			}
		}
		user := ri.User
		if user == "" {
			user = "-"
		}
		line := "http: " + r.Method + " " + r.URL.Path + " " + itoa(status) + " " + roundDur(time.Since(start)) + " user=" + user + " ip=" + clientHost(r.RemoteAddr)
		if f := r.Header.Get("X-Forwarded-For"); f != "" { // informational only: the client can set it
			line += " xff=" + oneToken(f)
		}
		log.Print(line)
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return d.Round(time.Microsecond).String()
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

func clientHost(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return strings.Trim(addr[:i], "[]")
	}
	return addr
}

// oneToken keeps log lines on one line and short: the first address of a forwarded list.
func oneToken(s string) string {
	s = strings.TrimSpace(strings.SplitN(s, ",", 2)[0])
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.Map(func(r rune) rune {
		if r < 33 || r > 126 {
			return -1
		}
		return r
	}, s)
}
