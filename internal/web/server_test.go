package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vicape92/repodeploy/internal/deploy"
)

func webFixture(t *testing.T) (*Server, *httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	if err := Initialize(root, "correct-horse-battery-staple"); err != nil {
		t.Fatal(err)
	}
	s, err := deploy.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	server, err := New(s, false)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(server.Handler())
	t.Cleanup(h.Close)
	token, _ := os.ReadFile(filepath.Join(root, "admin.token"))
	return server, h, strings.TrimSpace(string(token))
}

func request(t *testing.T, method, path, token string, body any) *http.Response {
	t.Helper()
	var b io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		b = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, path, b)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAPIRequiresAuthenticationAndQueuesWithoutExecuting(t *testing.T) {
	server, h, token := webFixture(t)
	resp := request(t, "GET", h.URL+"/api/projects", "", nil)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unauthorized status=%d", resp.StatusCode)
	}
	p := deploy.Project{Name: "app", Repo: "https://github.com/team/app.git", Branch: "main", Mode: "manual", Dockerfile: "Dockerfile", Port: 18080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:18080/health"}
	resp = request(t, "POST", h.URL+"/api/projects", token, p)
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create %d: %s", resp.StatusCode, b)
	}
	json.NewDecoder(resp.Body).Decode(&p)
	resp = request(t, "POST", h.URL+"/api/projects/"+p.ID+"/deploy", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("deploy status=%d", resp.StatusCode)
	}
	jobs, _ := server.Store.Jobs(p.ID)
	if len(jobs) != 1 || jobs[0].Phase != "queued" {
		t.Fatalf("handler executed deployment: %v", jobs)
	}
}

func TestBrowserSessionRequiresCSRFForWrites(t *testing.T) {
	_, h, _ := webFixture(t)
	form := url.Values{"password": {"correct-horse-battery-staple"}}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(h.URL+"/login", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 303 || len(resp.Cookies()) != 1 {
		t.Fatalf("login failed: %d", resp.StatusCode)
	}
	cookie := resp.Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe session cookie")
	}
	req, _ := http.NewRequest("POST", h.URL+"/api/projects", strings.NewReader(`{}`))
	req.AddCookie(cookie)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("CSRF bypass: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", h.URL+"/", nil)
	req.AddCookie(cookie)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "<title>RepoDeploy</title>") || strings.Contains(string(b), ">Pushvori<") {
		t.Fatalf("dashboard %d", resp.StatusCode)
	}
}

func TestProjectRejectsCredentialURLsUnsafePathsAndMissingHealth(t *testing.T) {
	_, h, token := webFixture(t)
	for _, p := range []deploy.Project{
		{Name: "app", Repo: "https://user:secret@github.com/team/app.git", Branch: "main", Mode: "manual", Dockerfile: "Dockerfile", HealthURL: "http://localhost:18080/health", Port: 18080, ContainerPort: 8080},
		{Name: "app", Repo: "https://github.com/team/app.git", Branch: "main", Mode: "manual", Dockerfile: "../../Dockerfile", HealthURL: "http://localhost:18080/health", Port: 18080, ContainerPort: 8080},
		{Name: "app", Repo: "https://github.com/team/app.git", Branch: "main", Mode: "manual", Dockerfile: "Dockerfile", Port: 18080, ContainerPort: 8080},
	} {
		resp := request(t, "POST", h.URL+"/api/projects", token, p)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("invalid project accepted: %d", resp.StatusCode)
		}
	}
}

func TestInitializationCannotOverwriteExistingCredentials(t *testing.T) {
	root := t.TempDir()
	if err := Initialize(root, "correct-horse-battery-staple"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(root, "admin.token"))
	if err := Initialize(root, "other-password-long-enough"); err == nil {
		t.Fatal("overwrote credentials")
	}
	after, _ := os.ReadFile(filepath.Join(root, "admin.token"))
	if !bytes.Equal(before, after) {
		t.Fatal("token changed")
	}
}
