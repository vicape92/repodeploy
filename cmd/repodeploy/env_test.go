package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vicape92/repodeploy/internal/deploy"
	"github.com/vicape92/repodeploy/internal/envvars"
	"github.com/vicape92/repodeploy/internal/web"
)

func cliFixture(t *testing.T) (string, *deploy.Store, *httptest.Server) {
	t.Helper()
	root := t.TempDir()
	if err := web.Initialize(root, "correct-horse-battery-staple"); err != nil {
		t.Fatal(err)
	}
	s, err := deploy.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	server, err := web.New(s, false)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(server.Handler())
	t.Cleanup(h.Close)
	return root, s, h
}

func cli(t *testing.T, root, url, stdin string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(context.Background(), append([]string{"--state", root, "--url", url}, args...), strings.NewReader(stdin), &out)
	return out.String(), err
}

func TestCLIManagesEnvironmentVariables(t *testing.T) {
	root, s, h := cliFixture(t)
	p, err := s.SaveProject(deploy.Project{Name: "app", Repo: "https://github.com/team/app.git", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli(t, root, h.URL, "s3cr3t-value\n", "env", "set", p.ID, "API_KEY", "--build"); err != nil {
		t.Fatal(err)
	}
	dotenv := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(dotenv, []byte("A=1\nB='two'\n"), 0600)
	if _, err := cli(t, root, h.URL, "", "env", "import", p.ID, dotenv); err != nil {
		t.Fatal(err)
	}
	vars, _ := s.Env.List(p.ID)
	want := []envvars.Var{{Name: "A", Value: "1"}, {Name: "API_KEY", Value: "s3cr3t-value", Build: true}, {Name: "B", Value: "two"}}
	if !reflect.DeepEqual(vars, want) {
		t.Fatalf("stored: %+v", vars)
	}
	out, err := cli(t, root, h.URL, "", "env", "list", p.ID)
	if err != nil || strings.Contains(out, "s3cr3t") || !strings.Contains(out, "API_KEY") {
		t.Fatalf("list: %v %s", err, out)
	}
	if out, _ = cli(t, root, h.URL, "", "env", "list", p.ID, "--reveal"); !strings.Contains(out, "s3cr3t-value") {
		t.Fatalf("reveal: %s", out)
	}
	if _, err := cli(t, root, h.URL, "", "env", "unset", p.ID, "A"); err != nil {
		t.Fatal(err)
	}
	if _, err := cli(t, root, h.URL, "", "env", "unset", p.ID, "A"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("second unset: %v", err)
	}
	if _, err := cli(t, root, h.URL, "", "env", "frobnicate", p.ID); err == nil {
		t.Fatal("unknown env command accepted")
	}
	if _, err := cli(t, root, h.URL, "", "deploy-changes", p.ID); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("deploy-changes without release: %v", err)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"200MB": 200 << 20, "1gb": 1 << 30, "2048KB": 2 << 20, "1048576": 1 << 20} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("%s: %d %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "abc", "-1MB", "512KB", "10TB"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
