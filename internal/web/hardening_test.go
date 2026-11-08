package web

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

func TestSecureServerUsesPrefixedCookiesAndHSTS(t *testing.T) {
	root := t.TempDir()
	if err := Initialize(root, fixturePassword); err != nil {
		t.Fatal(err)
	}
	store, err := deploy.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, true)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(server.Handler())
	defer h.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(h.URL+"/login", url.Values{"password": {fixturePassword}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Strict-Transport-Security") != "max-age=31536000" {
		t.Fatalf("missing HSTS: %q", resp.Header.Get("Strict-Transport-Security"))
	}
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "__Host-repodeploy_session" {
			session = c
		}
	}
	if session == nil || !session.Secure || session.Path != "/" || session.Domain != "" {
		t.Fatalf("session cookie lacks __Host- guarantees: %+v", resp.Cookies())
	}
	// The browser would send it back over HTTPS; the server must accept it.
	req, _ := http.NewRequest("GET", h.URL+"/", nil)
	req.AddCookie(&http.Cookie{Name: session.Name, Value: session.Value})
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("prefixed session rejected: %d", resp.StatusCode)
	}
	if server.flowCookie("github") != "__Secure-repodeploy_connect_github" {
		t.Fatal("flow cookie not prefixed")
	}
}

func TestInsecureLoopbackServerOmitsHSTS(t *testing.T) {
	_, h, _ := webFixture(t)
	resp, err := http.Get(h.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Strict-Transport-Security") != "" {
		t.Fatal("HSTS sent over plain HTTP")
	}
}

func signedWebhook(t *testing.T, target, secret string, body []byte) *http.Response {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req, _ := http.NewRequest("POST", target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("delivery-%d", len(body)))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestLargeWebhookPayloadsAreAcceptedUpToGitHubLimit(t *testing.T) {
	server, h, _ := webFixture(t)
	p, err := server.Store.SaveProject(deploy.Project{Name: "app", Repo: "https://github.com/team/app.git", Branch: "main", Mode: "webhook", Dockerfile: "Dockerfile", Port: 18080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:18080/health"})
	if err != nil {
		t.Fatal(err)
	}
	secret := "webhook-secret-value"
	os.WriteFile(filepath.Join(server.Store.Root, "secrets", p.ID+".webhook"), []byte(secret), 0600)
	payload := func(padding int) []byte {
		return []byte(`{"ref":"refs/heads/main","after":"` + strings.Repeat("a", 40) + `","repository":{"full_name":"team/app","html_url":"https://github.com/team/app"},"commits":"` + strings.Repeat("x", padding) + `"}`)
	}
	resp := signedWebhook(t, h.URL+"/hooks/github/"+p.ID, secret, payload(3<<20))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 202 || !strings.Contains(string(body), "job_id") {
		t.Fatalf("3 MB push rejected: %d %s", resp.StatusCode, body)
	}
	resp = signedWebhook(t, h.URL+"/hooks/github/"+p.ID, secret, payload(26<<20))
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatalf("oversized webhook: %d", resp.StatusCode)
	}
	// Other endpoints keep the 1 MiB limit.
	resp, err = http.Post(h.URL+"/login", "application/x-www-form-urlencoded", strings.NewReader("password="+strings.Repeat("x", 2<<20)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 303 || resp.StatusCode == 200 {
		t.Fatalf("oversized form accepted: %d", resp.StatusCode)
	}
}
