// Package config holds the built-in list of apps Berth expects to find.
package config

import (
	_ "embed"
	"fmt"
	"regexp"

	"gopkg.in/yaml.v3"
)

//go:embed expected-apps.yaml
var raw []byte

type ExpectedApp struct {
	Name             string   `yaml:"name"`
	Title            string   `yaml:"title"`
	Repo             string   `yaml:"repo"`
	Branch           string   `yaml:"branch"`
	Subfolder        string   `yaml:"subfolder"`
	ComposeFile      string   `yaml:"compose_file"`
	PredeployService string   `yaml:"predeploy_service"`
	FolderNames      []string `yaml:"folder_names"`
	PublicURL        string   `yaml:"public_url"`
	Monitor          bool     `yaml:"monitor"`
	// ComposeProject adopts an existing compose project (containers and named volumes) in place.
	ComposeProject string `yaml:"compose_project"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
var projectRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// Expected parses and validates the embedded expected-apps.yaml.
func Expected() ([]ExpectedApp, error) { return parse(raw) }

func parse(b []byte) ([]ExpectedApp, error) {
	var f struct {
		Apps []ExpectedApp `yaml:"apps"`
	}
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("expected-apps.yaml: %w", err)
	}
	seen := map[string]bool{}
	for i := range f.Apps {
		a := &f.Apps[i]
		if !nameRe.MatchString(a.Name) || a.Repo == "" || seen[a.Name] {
			return nil, fmt.Errorf("expected-apps.yaml: entry %d (%q) needs a unique lowercase name and a repo", i+1, a.Name)
		}
		if a.ComposeProject != "" && !projectRe.MatchString(a.ComposeProject) {
			return nil, fmt.Errorf("expected-apps.yaml: entry %q has an invalid compose_project", a.Name)
		}
		seen[a.Name] = true
		if a.Title == "" {
			a.Title = a.Name
		}
		if a.Branch == "" {
			a.Branch = "main"
		}
		if a.ComposeFile == "" {
			a.ComposeFile = "docker-compose.yml"
		}
	}
	return f.Apps, nil
}

//go:embed expected-services.yaml
var rawServices []byte

// ExpectedService is a service Berth suggests monitoring.
type ExpectedService struct {
	Name   string            `yaml:"name"`
	Kind   string            `yaml:"kind"`
	Host   string            `yaml:"host"`
	Port   int               `yaml:"port"`
	Params map[string]string `yaml:"params"`
	Note   string            `yaml:"note"`
}

var serviceKinds = map[string]bool{"postgres": true, "redis": true, "mongo": true, "rabbitmq": true, "mysql": true, "http": true, "tcp": true}

// ExpectedServices parses and validates the embedded expected-services.yaml.
func ExpectedServices() ([]ExpectedService, error) { return parseServices(rawServices) }

func parseServices(b []byte) ([]ExpectedService, error) {
	var f struct {
		Services []ExpectedService `yaml:"services"`
	}
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("expected-services.yaml: %w", err)
	}
	seen := map[string]bool{}
	for i, s := range f.Services {
		if !nameRe.MatchString(s.Name) || seen[s.Name] || !serviceKinds[s.Kind] || s.Port < 1 || s.Port > 65535 {
			return nil, fmt.Errorf("expected-services.yaml: entry %d (%q) needs a unique lowercase name, a known kind and a port", i+1, s.Name)
		}
		seen[s.Name] = true
	}
	return f.Services, nil
}
