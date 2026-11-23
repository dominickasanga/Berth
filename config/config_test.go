package config

import "testing"

func TestEmbeddedListIsValid(t *testing.T) {
	apps, err := Expected()
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 || apps[0].Name != "kite360-web" || apps[1].Name != "kite360-api" {
		t.Fatalf("unexpected list: %+v", apps)
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
