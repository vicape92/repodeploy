package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vicape92/repodeploy/internal/deploy"
)

func TestParseUpArgs(t *testing.T) {
	got, err := parseUpArgs([]string{"site", "--project=abc", "--force", "--detach"})
	if err != nil || !reflect.DeepEqual(got, upOptions{project: "abc", dir: "site", force: true, detach: true}) {
		t.Fatalf("%+v %v", got, err)
	}
	if got, _ = parseUpArgs([]string{"--project", "abc"}); got.dir != "." || got.project != "abc" {
		t.Fatalf("defaults: %+v", got)
	}
	for _, bad := range [][]string{{"--project"}, {"--unknown"}, {"a", "b"}} {
		if _, err := parseUpArgs(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestUpUploadsDirectoryAndRemembersProject(t *testing.T) {
	root, s, h := cliFixture(t)
	p, err := s.SaveProject(deploy.Project{Name: "site", Source: "upload", Dockerfile: "Dockerfile", Port: 18080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:18080/health"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch"), 0644)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1"), 0644)
	out, err := cli(t, root, h.URL, "", "up", "--project", p.ID, "--detach", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Skipped .env") || !strings.Contains(out, "queued") {
		t.Fatalf("output: %s", out)
	}
	if saved, _ := os.ReadFile(filepath.Join(dir, ".repodeploy", "project")); strings.TrimSpace(string(saved)) != p.ID {
		t.Fatalf("project ID not saved: %q", saved)
	}
	jobs, _ := s.Jobs(p.ID)
	if len(jobs) != 1 || jobs[0].Source != "upload" {
		t.Fatalf("jobs: %+v", jobs)
	}
	dest := filepath.Join(t.TempDir(), "src")
	if err := s.Uploads.Prepare(p.ID, jobs[0].Commit, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, ".env")); !os.IsNotExist(err) {
		t.Fatal(".env was uploaded")
	}
	if _, err := cli(t, root, h.URL, "", "up", "--detach", dir); err != nil {
		t.Fatalf("saved project ID not used: %v", err)
	}
	if jobs, _ = s.Jobs(p.ID); len(jobs) != 2 {
		t.Fatalf("second upload not queued: %d", len(jobs))
	}
}

func TestUpFollowsLogsAndFailsWhenTheDeploymentFails(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "admin.token"), []byte("secret-token"), 0600)
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/uploads"):
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(202)
			w.Write([]byte(`{"id":"job1","phase":"queued"}`))
		case strings.HasSuffix(r.URL.Path, "/logs"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"log\":\"Building\\n\"}\n\nevent: done\ndata: {}\n\n"))
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"job1","phase":"failed","error":"Docker build failed"}`))
		}
	}))
	defer h.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch"), 0644)
	out, err := cli(t, root, h.URL, "", "up", "--project", "p1", dir)
	if err == nil || !strings.Contains(err.Error(), "Docker build failed") || !strings.Contains(out, "Building") {
		t.Fatalf("err=%v out=%s", err, out)
	}
}
