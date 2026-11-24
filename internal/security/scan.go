package security

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/dominickasanga/berth/internal/store"
)

// AppScan is everything found about one app. It is stored as JSON and shown on the Security page.
type AppScan struct {
	App    string    `json:"app"`
	At     time.Time `json:"at"`
	Folder string    `json:"folder"`

	GoMods    []VulnResult `json:"go_mods"`
	VulnError string       `json:"vuln_error,omitempty"`

	Containers     []ContainerIssue `json:"containers"`
	ContainerCount int              `json:"container_count"`
	ContainerError string           `json:"container_error,omitempty"`

	Secrets     []SecretFile `json:"secrets"`
	SecretError string       `json:"secret_error,omitempty"`
}

// VulnerableModules counts the modules with known vulnerabilities, over all of the app's go.mod files.
func (s AppScan) VulnerableModules() int {
	n := 0
	for _, g := range s.GoMods {
		n += len(g.Findings)
	}
	return n
}

// Runner scans the apps now and then. It reads app folders and asks Docker; it changes nothing.
type Runner struct {
	Store       *store.Store
	Dir         func(store.App) string // the app's source folder
	ProjectName func(store.App) string // the compose project its containers belong to
	Vuln        *VulnChecker

	scanning atomic.Bool
}

// Scanning reports whether a scan is running.
func (r *Runner) Scanning() bool { return r.scanning.Load() }

// ScanApp checks one app. A check that cannot run reports why instead of looking clean.
func (r *Runner) ScanApp(ctx context.Context, a store.App) AppScan {
	res := AppScan{App: a.Name, At: time.Now(), Folder: r.Dir(a)}
	if _, err := os.Stat(res.Folder); err != nil {
		msg := "the app's folder was not found on this server (it appears after the first deploy)"
		res.VulnError, res.SecretError = msg, msg
	} else {
		for _, rel := range FindGoMods(res.Folder, filepath.Glob) {
			data, err := os.ReadFile(filepath.Join(res.Folder, rel))
			if err != nil {
				res.VulnError = err.Error()
				break
			}
			g, err := r.Vuln.CheckGoMod(ctx, rel, data)
			if err != nil {
				res.VulnError = err.Error()
				break
			}
			res.GoMods = append(res.GoMods, *g)
		}
		if secrets, err := RepoSecrets(ctx, res.Folder); err != nil {
			res.SecretError = err.Error()
		} else {
			res.Secrets = secrets
		}
	}
	project := "berth-" + a.Name
	if r.ProjectName != nil {
		project = r.ProjectName(a)
	}
	issues, n, err := CheckContainers(ctx, project)
	if err != nil {
		res.ContainerError = err.Error()
	}
	res.Containers, res.ContainerCount = issues, n
	return res
}

// ScanAll checks every app and stores the results. It returns false if a scan was already running.
func (r *Runner) ScanAll(ctx context.Context) bool {
	if !r.scanning.CompareAndSwap(false, true) {
		return false
	}
	defer r.scanning.Store(false)
	apps, err := r.Store.ListApps(ctx)
	if err != nil {
		log.Printf("security: cannot list apps: %v", err)
		return true
	}
	for _, a := range apps {
		if ctx.Err() != nil {
			return true
		}
		res := r.ScanApp(ctx, a)
		b, err := marshal(res)
		if err == nil {
			err = r.Store.SaveSecurityScan(ctx, a.Name, b, res.At)
		}
		if err != nil {
			log.Printf("security: saving the scan of %s failed: %v", a.Name, err)
		}
	}
	log.Printf("security: scanned %d apps", len(apps))
	return true
}

// Run scans a couple of minutes after start (so the first page is not empty) and then daily.
func (r *Runner) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Minute):
	}
	for {
		r.ScanAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(24 * time.Hour):
		}
	}
}

func marshal(s AppScan) ([]byte, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("cannot encode the scan: %w", err)
	}
	return b, nil
}
