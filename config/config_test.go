package config

import "testing"

func TestEmbeddedListIsValid(t *testing.T) {
	apps, err := Expected()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"kite360-web", "kite360-api", "nyasahub-api", "nyasahub-web", "gopherpost", "gotenberg", "sb-web", "tusk", "pgadmin4"}
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
		wantMonitor := a.Name != "pgadmin4" // an admin tool, not part of the product
		if a.Monitor != wantMonitor {
			t.Errorf("%s: monitor=%v, want %v", a.Name, a.Monitor, wantMonitor)
		}
	}
}

func TestExistingStacksAreAdoptedByProjectName(t *testing.T) {
	apps, err := Expected()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ExpectedApp{}
	for _, a := range apps {
		byName[a.Name] = a
	}
	for name, project := range map[string]string{"kite360-web": "tams-nuxt", "tusk": "tusk", "pgadmin4": "pgadmin4", "sb-web": "sb-web-app", "gopherpost": "gopherpost", "gotenberg": "gotenberg"} {
		if byName[name].ComposeProject != project {
			t.Errorf("%s should adopt project %q, got %q", name, project, byName[name].ComposeProject)
		}
	}
	// the new apps (no running stack to adopt) use Berth's own project name
	if byName["kite360-api"].ComposeProject != "" || byName["nyasahub-web"].ComposeProject != "" {
		t.Error("apps without an existing stack must not set compose_project")
	}
}

func TestRenamedRepositoryIsAnAlias(t *testing.T) {
	apps, _ := Expected()
	for _, a := range apps {
		if a.Name == "kite360-web" {
			if len(a.RepoAliases) != 1 || a.RepoAliases[0] != "https://github.com/dominickasanga/proworkwave_erp" {
				t.Fatalf("kite360-web should know its old repository name: %v", a.RepoAliases)
			}
			return
		}
	}
	t.Fatal("kite360-web missing")
}

func TestKite360APIKnowsItsOldName(t *testing.T) {
	apps, _ := Expected()
	for _, a := range apps {
		if a.Name == "kite360-api" {
			if len(a.RepoAliases) != 1 || a.RepoAliases[0] != "https://github.com/dominickasanga/visionary_erp_api" {
				t.Fatalf("kite360-api should know its old repository name: %v", a.RepoAliases)
			}
			return
		}
	}
	t.Fatal("kite360-api missing")
}

func TestRejectsBadEntries(t *testing.T) {
	for name, y := range map[string]string{
		"no repo":     "apps:\n  - name: x\n",
		"bad name":    "apps:\n  - name: Bad Name\n    repo: r\n",
		"duplicate":   "apps:\n  - {name: x, repo: r}\n  - {name: x, repo: r}\n",
		"bad project": "apps:\n  - {name: x, repo: r, compose_project: \"Bad Name\"}\n",
	} {
		if _, err := parse([]byte(y)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestExpectedServices(t *testing.T) {
	svcs, err := ExpectedServices()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]ExpectedService{}
	for _, s := range svcs {
		have[s.Name] = s
	}
	for name, port := range map[string]int{"postgres-main": 5432, "redis-main": 6379, "mongo-main": 27017, "rabbitmq-main": 5672, "mysql-main": 3306, "kite360-api": 8001, "nyasahub-api": 8090} {
		if have[name].Port != port {
			t.Errorf("%s should be on port %d, got %d", name, port, have[name].Port)
		}
	}
	if have["gotenberg"].Params["url"] != "http://127.0.0.1:3025/health" {
		t.Errorf("gotenberg url: %v", have["gotenberg"].Params)
	}
	for _, bad := range []string{"services:\n  - {name: x, kind: nosuch, port: 1}\n", "services:\n  - {name: x, kind: tcp, port: 0}\n", "services:\n  - {name: x, kind: tcp, port: 1}\n  - {name: x, kind: tcp, port: 2}\n"} {
		if _, err := parseServices([]byte(bad)); err == nil {
			t.Errorf("should reject: %s", bad)
		}
	}
}
