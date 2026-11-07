package deploy

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDockerfileModeBindsLoopbackUnlessConfigured(t *testing.T) {
	source := t.TempDir()
	os.WriteFile(filepath.Join(source, "Dockerfile"), []byte("FROM scratch"), 0600)
	for _, tc := range []struct{ bind, want string }{
		{"", "127.0.0.1:18080:8080"},
		{"0.0.0.0", "0.0.0.0:18080:8080"},
		{"::1", "[::1]:18080:8080"},
	} {
		p := Project{ID: strings.Repeat("a", 32), Dockerfile: "Dockerfile", Port: 18080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:18080/health", BindAddress: tc.bind}
		cfg, err := loadCompose(p, source, t.TempDir(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		ports := cfg["services"].(map[string]any)["app"].(map[string]any)["ports"].([]any)
		if len(ports) != 1 || ports[0] != tc.want {
			t.Fatalf("bind %q published %v, want %s", tc.bind, ports, tc.want)
		}
	}
}

func TestBindAddressValidation(t *testing.T) {
	base := Project{Name: "app", Repo: "https://github.com/team/app.git", Branch: "main", Mode: "manual", Dockerfile: "Dockerfile", Port: 18080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:18080/health"}
	for bind, ok := range map[string]bool{"": true, "127.0.0.1": true, "0.0.0.0": true, "::": true, "::1": true, "10.0.0.5": false, "localhost": false, "not-an-ip": false} {
		p := base
		p.BindAddress = bind
		if err := ValidateProject(p); (err == nil) != ok {
			t.Fatalf("bind %q: err=%v want ok=%v", bind, err, ok)
		}
	}
}

func TestComposeServicesGetNoNewPrivilegesAndLogRotation(t *testing.T) {
	source := t.TempDir()
	os.WriteFile(filepath.Join(source, "compose.yaml"), []byte("services:\n  app:\n    image: alpine\n    healthcheck:\n      test: ['CMD', 'true']\n  worker:\n    image: alpine\n"), 0600)
	p := Project{ID: strings.Repeat("a", 32), ComposeFile: "compose.yaml", HealthService: "app"}
	cfg, err := loadCompose(p, source, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range cfg["services"].(map[string]any) {
		service := v.(map[string]any)
		if !reflect.DeepEqual(service["security_opt"], []any{"no-new-privileges:true"}) {
			t.Fatalf("%s lacks no-new-privileges: %v", name, service["security_opt"])
		}
		logging, _ := service["logging"].(map[string]any)
		options, _ := logging["options"].(map[string]any)
		if logging["driver"] != "json-file" || options["max-size"] != "10m" || options["max-file"] != "3" {
			t.Fatalf("%s lacks log rotation: %v", name, service["logging"])
		}
	}
	p.AllowPrivilegeEscalation = true
	cfg, err = loadCompose(p, source, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["services"].(map[string]any)["app"].(map[string]any)["security_opt"]; ok {
		t.Fatal("explicit opt-out still injected no-new-privileges")
	}
}

func TestComposeRejectsUserSuppliedSecurityOptions(t *testing.T) {
	source := t.TempDir()
	os.WriteFile(filepath.Join(source, "compose.yaml"), []byte("services:\n  app:\n    image: alpine\n    security_opt: ['seccomp:unconfined']\n"), 0600)
	p := Project{ID: strings.Repeat("a", 32), ComposeFile: "compose.yaml", HealthURL: "http://127.0.0.1:1/health"}
	if _, err := loadCompose(p, source, t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("repository overrode security options")
	}
}

func TestAllInterfacePortsReportsOnlyImplicitBindings(t *testing.T) {
	services := map[string]any{
		"app": map[string]any{"ports": []any{"18080:8080", "127.0.0.1:18081:8080", "0.0.0.0:80:80", "[::1]:81:81", 9000, map[string]any{"target": 1, "host_ip": "127.0.0.1"}, map[string]any{"target": 2}}},
	}
	got := allInterfacePorts(services)
	want := []string{"service app port 18080:8080", "service app port 9000", "service app port map[target:2]"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestProductionGitRefusesFileTransport(t *testing.T) {
	if productionGitProtocols != "https:ssh" {
		t.Fatalf("shipped Git protocols widened: %q", productionGitProtocols)
	}
	repo, _ := sourceRepo(t)
	gitAllowProtocol = productionGitProtocols
	defer func() { gitAllowProtocol = "https:ssh:file" }()
	if _, err := (Git{}).Resolve(context.Background(), Project{Repo: repo, Branch: "main"}); err == nil {
		t.Fatal("local file transport accepted")
	}
}

func TestValidationErrorsNameTheirField(t *testing.T) {
	base := Project{Name: "app", Repo: "https://github.com/team/app.git", Branch: "main", Mode: "manual", Dockerfile: "Dockerfile", Port: 18080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:18080/health"}
	for field, mutate := range map[string]func(*Project){
		"name":           func(p *Project) { p.Name = "" },
		"repo":           func(p *Project) { p.Repo = "https://user:pw@github.com/team/app.git" },
		"branch":         func(p *Project) { p.Branch = "bad..branch" },
		"mode":           func(p *Project) { p.Mode = "sometimes" },
		"build_type":     func(p *Project) { p.ComposeFile = "compose.yaml" },
		"port":           func(p *Project) { p.Port = 0 },
		"container_port": func(p *Project) { p.ContainerPort = 70000 },
		"health_url":     func(p *Project) { p.HealthURL = "http://example.org/health" },
		"health_timeout": func(p *Project) { p.HealthTimeout = 301 },
		"poll_seconds":   func(p *Project) { p.PollSeconds = 5 },
		"retain":         func(p *Project) { p.Retain = 51 },
		"bind_address":   func(p *Project) { p.BindAddress = "10.1.2.3" },
	} {
		p := base
		mutate(&p)
		if got := ErrorField(ValidateProject(p)); got != field {
			t.Fatalf("mutating %s reported field %q", field, got)
		}
	}
}

func TestGitFailuresIncludeSanitizedDiagnostics(t *testing.T) {
	_, err := (Git{}).Resolve(context.Background(), Project{Repo: t.TempDir(), Branch: "main"})
	if err == nil || !strings.Contains(err.Error(), "(git: ") {
		t.Fatalf("Git diagnostics missing: %v", err)
	}
	got := sanitizeGitOutput("remote: hello\n\x1b[31mfatal: unable to access 'https://x-access-token:ghs_SECRET@github.com/a/b.git/'\n", "ghs_SECRET")
	if strings.Contains(got, "ghs_SECRET") || strings.Contains(got, "\x1b") || !strings.Contains(got, "fatal: unable to access") || !strings.Contains(got, "remote: hello | ") {
		t.Fatalf("unsafe or unhelpful diagnostics: %q", got)
	}
	long := sanitizeGitOutput(strings.Repeat("a", 2000), "")
	if len(long) > 510 {
		t.Fatalf("diagnostics not capped: %d", len(long))
	}
}

func TestComposeAllowsEscapedDollarOnly(t *testing.T) {
	for body, ok := range map[string]bool{
		"services:\n  app:\n    image: alpine\n    healthcheck:\n      test: ['CMD-SHELL', 'test \"$$HOME\" = /root']\n": true,
		"services:\n  app:\n    image: alpine\n    command: ['sh', '-c', 'echo $$$$']\n":                                 true,
		"services:\n  app:\n    image: alpine\n    command: ['sh', '-c', 'echo $HOME']\n":                                false,
		"services:\n  app:\n    image: alpine\n    command: ['sh', '-c', 'echo ${HOME}']\n":                              false,
		"services:\n  app:\n    image: alpine\n    command: ['sh', '-c', 'echo $$$HOME']\n":                              false,
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(body), 0600)
		p := Project{ID: strings.Repeat("a", 32), ComposeFile: "compose.yaml", HealthURL: "http://127.0.0.1:1/health"}
		if _, err := loadCompose(p, dir, t.TempDir(), t.TempDir()); (err == nil) != ok {
			t.Fatalf("%q: err=%v want ok=%v", body, err, ok)
		}
	}
}

func TestPullImagesOption(t *testing.T) {
	if !reflect.DeepEqual(buildArgs(Project{}), []string{"build"}) || !reflect.DeepEqual(buildArgs(Project{PullImages: true}), []string{"build", "--pull"}) {
		t.Fatal("unexpected build arguments")
	}
	for _, tc := range []struct {
		pull, present, want bool
	}{{false, true, false}, {false, false, true}, {true, true, true}, {true, false, true}} {
		if got := shouldPull(Project{PullImages: tc.pull}, tc.present); got != tc.want {
			t.Fatalf("pull=%v present=%v: got %v", tc.pull, tc.present, got)
		}
	}
}

func TestLogMaskingIgnoresShortFragments(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	j, _ := s.Enqueue(p.ID, "manual", "", "", "")
	os.WriteFile(filepath.Join(s.Root, "secrets", p.ID+"-env"), []byte("DEBUG=true\nPORT=8080\nAPI_KEY=k3y-v4lue-long\n"), 0600)
	os.WriteFile(filepath.Join(s.Root, "secrets", p.ID+"-pin"), []byte("4821\n"), 0600)
	os.WriteFile(filepath.Join(s.Root, "secrets", p.ID+"-flag"), []byte("1\n"), 0600)
	w := s.LogWriter(j.ID)
	w.Write([]byte("step 1 of 3: true port 8080 pin 4821 key k3y-v4lue-long\n"))
	w.Close()
	logs, _ := s.Logs(j.ID)
	if logs != "step 1 of 3: true port 8080 pin [REDACTED] key [REDACTED]\n" {
		t.Fatalf("unexpected masking: %q", logs)
	}
}
