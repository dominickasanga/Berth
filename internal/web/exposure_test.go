package web

import "testing"

func TestPlainHTTPExposed(t *testing.T) {
	for _, c := range []struct {
		addr   string
		secure bool
		want   bool
	}{
		{"127.0.0.1:8095", false, false},
		{"localhost:8095", false, false},
		{"[::1]:8095", false, false},
		{"0.0.0.0:8095", false, true},
		{":8095", false, true},
		{"[::]:8095", false, true},
		{"192.168.1.10:8095", false, true},
		{"0.0.0.0:8095", true, false}, // Secure cookie: it only works behind HTTPS
		{"garbage", false, true},
	} {
		if got := PlainHTTPExposed(c.addr, c.secure); got != c.want {
			t.Errorf("PlainHTTPExposed(%q, %v) = %v, want %v", c.addr, c.secure, got, c.want)
		}
	}
}
