package web

import (
	"net"
	"strings"
)

// PlainHTTPExposed reports whether Berth would accept connections from other machines over
// plain HTTP: it listens beyond loopback and its cookie is not marked Secure (so there is no HTTPS
// in front of it). Passwords, 2FA codes and sessions would then travel unencrypted.
func PlainHTTPExposed(addr string, secureCookie bool) bool {
	if secureCookie {
		return false // the cookie only works over HTTPS, so a TLS proxy must be in front
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return true
	}
	host = strings.Trim(host, "[]")
	if host == "" || host == "0.0.0.0" || host == "::" {
		return true
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}
