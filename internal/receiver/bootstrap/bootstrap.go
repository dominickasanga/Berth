// Package bootstrap serves what a new sender needs to install itself.
//
// The receiver hands out the agent binary and an installer, but never the
// source list: that stays owned by the machine being backed up. A receiver that
// dictated which paths each sender reads would be a remote file-read primitive
// over the whole fleet, and it is the most exposed component in the system.
package bootstrap

import (
	_ "embed"
	"net/http"
	"strings"
)

//go:embed files/install.sh
var installScript string

// The placeholder is substituted per request rather than baked in at build
// time, so one image works behind any hostname or proxy path.
const serverPlaceholder = "__SERVER_URL__"

// InstallScript returns the installer with the receiver's own URL filled in.
func InstallScript(serverURL string) string {
	if !strings.HasSuffix(serverURL, "/") {
		serverURL += "/"
	}
	return strings.ReplaceAll(installScript, serverPlaceholder, serverURL)
}

// ServerURL reconstructs the URL a client used to reach us, so the installer it
// downloads points back at the right place through whatever proxy is in front.
func ServerURL(r *http.Request, basePath string) string {
	scheme := "http"
	// Behind TLS termination the connection here is plain, so the proxy's
	// header is the only evidence the client used https.
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = v
	}

	host := r.Host
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = v
	}
	return scheme + "://" + host + basePath
}
