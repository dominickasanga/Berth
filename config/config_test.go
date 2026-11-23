package config

import "testing"

func TestEmbeddedListIsValid(t *testing.T) {
	apps, err := Expected()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"kite360-web", "kite360-api", "nyasahub-api", "nyasahub-web"}
	if len(apps) != len(want) {
		t.Fatalf("unexpected list: %+v", apps)
	}
	for i, n := range want {
		if apps[i].Name != n {
			t.Fatalf("entry %d is %q, want %q", i, apps[i].Name, n)
		}
	}
	if apps[2].Subfolder != "deployments/docker" || apps[2].ComposeFile != "docker-compose.prod.yml" {
		t.Errorf("NyasaHub API must use the production compose file, got %q in %q", apps[2].ComposeFile, apps[2].Subfolder)
	}
	for _, a := range apps {
		if !a.Monitor {
			t.Errorf("%s should be flagged for monitoring", a.Name)
		}
	}
}

func TestRejectsBadEntries(t *testing.T) {
	for name, y := range map[string]string{
		"no repo":   "apps:\n  - name: x\n",
		"bad name":  "apps:\n  - name: Bad Name\n    repo: r\n",
		"duplicate": "apps:\n  - {name: x, repo: r}\n  - {name: x, repo: r}\n",
	} {
		if _, err := parse([]byte(y)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
