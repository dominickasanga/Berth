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
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

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
