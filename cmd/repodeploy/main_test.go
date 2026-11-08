package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vicape92/repodeploy/internal/deploy"
	"github.com/vicape92/repodeploy/internal/web"
)

func TestCLIUsesRepoDeployName(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"help"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "RepoDeploy") || !strings.Contains(out.String(), "Usage: repodeploy ") || !strings.Contains(out.String(), "/var/lib/repodeploy") || strings.Contains(out.String(), "Pushvori") {
		t.Fatalf("unexpected CLI identity: %s", out.String())
	}
}

func TestDefaultStateDirectoryPreservesExistingInstallation(t *testing.T) {
	base := t.TempDir()
	fresh, legacy := filepath.Join(base, "repodeploy"), filepath.Join(base, "pushvori")
	if got := defaultStateDirectory(base); got != fresh {
		t.Fatalf("fresh state: %s", got)
	}
	os.Mkdir(legacy, 0700)
	if got := defaultStateDirectory(base); got != legacy {
		t.Fatalf("legacy state lost: %s", got)
	}
	os.Mkdir(fresh, 0700)
	if got := defaultStateDirectory(base); got != fresh {
		t.Fatalf("new state must take priority: %s", got)
	}
}

func TestInstallerFreshLegacyAndRunningServices(t *testing.T) {
	for _, tc := range []struct {
		name, active string
		legacy       bool
	}{
		{name: "fresh"},
		{name: "legacy", legacy: true},
		{name: "running new service", active: "repodeploy"},
		{name: "running legacy service", active: "pushvori", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range []string{"scripts", "packaging", "tools", "var/lib", "usr/local/bin", "etc/systemd/system"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
					t.Fatal(err)
				}
			}
			script, err := os.ReadFile("../../scripts/install.sh")
			if err != nil {
				t.Fatal(err)
			}
			unit, err := filepath.Glob("../../packaging/*.service")
			if err != nil || len(unit) != 1 {
				t.Fatalf("service fixture: %v %v", unit, err)
			}
			body, err := os.ReadFile(unit[0])
			if err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{"/var/lib/", "/usr/local/bin/", "/etc/systemd/system/"} {
				script = bytes.ReplaceAll(script, []byte(dir), []byte(root+dir))
				body = bytes.ReplaceAll(body, []byte(dir), []byte(root+dir))
			}
			os.WriteFile(filepath.Join(root, "scripts/install.sh"), script, 0600)
			os.WriteFile(filepath.Join(root, "packaging", filepath.Base(unit[0])), body, 0600)
			binary := filepath.Join(root, "binary")
			os.WriteFile(binary, []byte("binary-fixture"), 0600)
			legacyState := filepath.Join(root, "var/lib/pushvori")
			if tc.legacy {
				os.MkdirAll(legacyState, 0700)
				os.WriteFile(filepath.Join(legacyState, "credentials"), []byte("keep-this"), 0600)
				os.WriteFile(filepath.Join(root, "etc/systemd/system/pushvori.service"), []byte("old-unit"), 0600)
			}
			stub := `#!/bin/sh
name=${0##*/}
printf '%s %s\n' "$name" "$*" >> "$TEST_CALLS"
case "$name" in
  uname) echo Linux ;;
  id) if [ "$1" = -u ]; then echo 0; else exit 1; fi ;;
  systemctl) if [ "$1" = is-active ]; then [ "$3" = "$TEST_ACTIVE" ] && [ -n "$TEST_ACTIVE" ]; fi ;;
  install)
    directory=no
    while [ "$#" -gt 0 ]; do
      case "$1" in
        -d) directory=yes; shift ;;
        -o|-g|-m) shift 2 ;;
        *) break ;;
      esac
    done
    if [ "$directory" = yes ]; then mkdir -p "$1"; else cp "$1" "$2"; fi ;;
esac
`
			for _, tool := range []string{"uname", "id", "systemctl", "install", "git", "ssh", "docker", "getent", "useradd", "usermod"} {
				os.WriteFile(filepath.Join(root, "tools", tool), []byte(stub), 0700)
			}
			calls := filepath.Join(root, "calls")
			command := exec.Command("sh", filepath.Join(root, "scripts/install.sh"), binary)
			command.Env = append(os.Environ(), "PATH="+filepath.Join(root, "tools")+":"+os.Getenv("PATH"), "TEST_CALLS="+calls, "TEST_ACTIVE="+tc.active)
			output, err := command.CombinedOutput()
			installed, readErr := os.ReadFile(filepath.Join(root, "etc/systemd/system/repodeploy.service"))
			if tc.active != "" {
				if err == nil || readErr == nil {
					t.Fatalf("replaced running service: %s %v", output, err)
				}
				return
			}
			if err != nil || readErr != nil {
				t.Fatalf("install: %s %v; unit: %v", output, err, readErr)
			}
			account, state := "repodeploy", filepath.Join(root, "var/lib/repodeploy")
			if tc.legacy {
				account, state = "pushvori", legacyState
			}
			if !strings.Contains(string(installed), "User="+account+"\n") || !strings.Contains(string(installed), "--state "+state+" serve") || !strings.Contains(string(installed), "/usr/local/bin/repodeploy ") {
				t.Fatalf("incorrect service identity/state: %s", installed)
			}
			if _, err := os.Stat(filepath.Join(root, "usr/local/bin/repodeploy")); err != nil {
				t.Fatal(err)
			}
			if tc.legacy {
				credentials, _ := os.ReadFile(filepath.Join(legacyState, "credentials"))
				log, _ := os.ReadFile(calls)
				if string(credentials) != "keep-this" || !strings.Contains(string(log), "systemctl disable pushvori") {
					t.Fatalf("legacy state/service not preserved: %s", log)
				}
			}
		})
	}
}

func TestCLIEnqueuesManualDeployThroughAuthenticatedDaemon(t *testing.T) {
	root := t.TempDir()
	if err := web.Initialize(root, "correct-horse-battery-staple"); err != nil {
		t.Fatal(err)
	}
	s, err := deploy.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.SaveProject(deploy.Project{Name: "app", Repo: "https://github.com/team/app.git", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	server, err := web.New(s, false)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(server.Handler())
	defer h.Close()
	var out bytes.Buffer
	if err := run(context.Background(), []string{"--state", root, "--url", h.URL, "deploy", p.ID}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.Jobs(p.ID)
	if len(jobs) != 1 || jobs[0].Phase != "queued" || !strings.Contains(out.String(), jobs[0].ID) {
		t.Fatalf("CLI did not queue: %v %s", jobs, out.String())
	}
}

func TestCLIRefusesSendingTokenOverRemoteHTTP(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "admin.token"), []byte("secret-token"), 0600)
	err := run(context.Background(), []string{"--state", root, "--url", "http://example.org:8081", "projects"}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("insecure remote transport accepted: %v", err)
	}
}

func TestCLIFollowLogsPrintsDecodedLogEvents(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "admin.token"), []byte("secret-token"), 0600)
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"log\":\"building\\n\",\"phase\":\"building\"}\n\nevent: done\ndata: {}\n\n"))
	}))
	defer h.Close()
	var out bytes.Buffer
	if err := run(context.Background(), []string{"--state", root, "--url", h.URL, "logs", "job-id", "--follow"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "building\n" {
		t.Fatalf("raw SSE output: %q", out.String())
	}
}

func TestCLIPasswdAndRotateTokenTakeEffectOnRunningDaemon(t *testing.T) {
	root := t.TempDir()
	if err := web.Initialize(root, "correct-horse-battery-staple"); err != nil {
		t.Fatal(err)
	}
	s, err := deploy.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	server, err := web.New(s, false)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(server.Handler())
	defer h.Close()
	oldToken, _ := os.ReadFile(filepath.Join(root, "admin.token"))
	var out bytes.Buffer
	if err := run(context.Background(), []string{"--state", root, "rotate-token"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	newToken, _ := os.ReadFile(filepath.Join(root, "admin.token"))
	if bytes.Equal(oldToken, newToken) || strings.Contains(out.String(), strings.TrimSpace(string(newToken))) {
		t.Fatalf("token not rotated or printed to the terminal: %s", out.String())
	}
	// The CLI picks up the new token file; the running daemon accepts it.
	if err := run(context.Background(), []string{"--state", root, "--url", h.URL, "projects"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatalf("rotated token rejected: %v", err)
	}
	req, _ := http.NewRequest("GET", h.URL+"/api/projects", nil)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(oldToken)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("old token still accepted: %d", resp.StatusCode)
	}
	if err := run(context.Background(), []string{"--state", root, "passwd", "--password-file", "-"}, strings.NewReader("short\n"), &bytes.Buffer{}); err == nil {
		t.Fatal("weak password accepted")
	}
	if err := run(context.Background(), []string{"--state", root, "passwd", "--password-file", "-"}, strings.NewReader("an-entirely-new-password\n"), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for password, want := range map[string]int{"correct-horse-battery-staple": 401, "an-entirely-new-password": 303} {
		resp, err := client.PostForm(h.URL+"/login", url.Values{"password": {password}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("password %q: status %d, want %d", password[:3], resp.StatusCode, want)
		}
	}
}

func TestCLIDeleteRequiresConfirmationAndBackupNeverOverwrites(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "admin.token"), []byte("secret-token"), 0600)
	err := run(context.Background(), []string{"--state", root, "delete", "project-id"}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("unconfirmed delete: %v", err)
	}
	var method, target string
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, target = r.Method, r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"deleted":true}`))
	}))
	defer h.Close()
	if err := run(context.Background(), []string{"--state", root, "--url", h.URL, "delete", "project-id", "--yes", "--volumes"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if method != "DELETE" || target != "/api/projects/project-id?volumes=1" {
		t.Fatalf("delete request %s %s", method, target)
	}

	state := t.TempDir()
	s, err := deploy.OpenStore(state)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	output := filepath.Join(t.TempDir(), "backup.tar.gz")
	var out bytes.Buffer
	if err := run(context.Background(), []string{"--state", state, "backup", "--output", output}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0600 || info.Size() == 0 {
		t.Fatalf("backup file: %v %v", info, err)
	}
	if err := run(context.Background(), []string{"--state", state, "backup", "--output", output}, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatal("backup overwrote an existing file")
	}
}

func TestCLITokenSourcesAndPermissions(t *testing.T) {
	var seen string
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer h.Close()
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	os.WriteFile(filepath.Join(root, "admin.token"), []byte("state-token"), 0600)
	call := func(args ...string) error {
		return run(context.Background(), append([]string{"--state", root}, args...), strings.NewReader(""), &bytes.Buffer{})
	}
	t.Setenv("REPODEPLOY_URL", h.URL)
	if err := call("projects"); err != nil || seen != "Bearer state-token" {
		t.Fatalf("state token: %v %q", err, seen)
	}
	configDir, _ := os.UserConfigDir()
	os.MkdirAll(filepath.Join(configDir, "repodeploy"), 0700)
	userToken := filepath.Join(configDir, "repodeploy", "token")
	os.WriteFile(userToken, []byte("user-token\n"), 0600)
	if err := call("projects"); err != nil || seen != "Bearer user-token" {
		t.Fatalf("user config token: %v %q", err, seen)
	}
	envToken := filepath.Join(t.TempDir(), "token")
	os.WriteFile(envToken, []byte("env-token"), 0600)
	t.Setenv("REPODEPLOY_TOKEN_FILE", envToken)
	if err := call("projects"); err != nil || seen != "Bearer env-token" {
		t.Fatalf("env token: %v %q", err, seen)
	}
	flagToken := filepath.Join(t.TempDir(), "token")
	os.WriteFile(flagToken, []byte("flag-token"), 0600)
	if err := call("--token-file", flagToken, "projects"); err != nil || seen != "Bearer flag-token" {
		t.Fatalf("flag token: %v %q", err, seen)
	}
	os.Chmod(envToken, 0644)
	seen = ""
	if err := call("projects"); err == nil || !strings.Contains(err.Error(), "chmod 600") || seen != "" {
		t.Fatalf("world-readable token used: %v %q", err, seen)
	}
}
