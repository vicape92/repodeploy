package scm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type localTransport struct{ base *url.URL }

func (r localTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	u := *req.URL
	clone.URL = &u
	clone.Host = req.URL.Host
	clone.URL.Scheme = r.base.Scheme
	clone.URL.Host = r.base.Host
	return http.DefaultTransport.RoundTrip(clone)
}
func testManager(t *testing.T, h http.HandlerFunc) *Manager {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	m := New(t.TempDir())
	m.Client = &http.Client{Transport: localTransport{u}, Timeout: 5 * time.Second}
	return m
}
func TestConnectionStatusDoesNotExposeSecretsAndRejectsUnknownProvider(t *testing.T) {
	m := New(t.TempDir())
	status, err := m.Status("github")
	if err != nil || status.Connected || status.Configured {
		t.Fatalf("empty status: %+v %v", status, err)
	}
	if _, err = m.Status("unknown"); err == nil {
		t.Fatal("unknown provider accepted")
	}
	if err = m.SaveBitbucket("client-key", "consumer-secret"); err != nil {
		t.Fatal(err)
	}
	status, err = m.Status("bitbucket")
	b, _ := json.Marshal(status)
	if err != nil || !status.Configured || status.Connected || strings.Contains(string(b), "consumer-secret") {
		t.Fatalf("status: %s %v", b, err)
	}
	stat, err := os.Stat(filepath.Join(m.root, "connections.json"))
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatalf("credential permissions: %v %v", stat, err)
	}
	reopened := New(m.root)
	status, err = reopened.Status("bitbucket")
	if err != nil || !status.Configured {
		t.Fatal("settings lost after restart")
	}
}
func TestProviderRequestRejectsRedirectAndSuppressesErrorBody(t *testing.T) {
	m := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://attacker.invalid/")
		w.WriteHeader(302)
		w.Write([]byte("secret-token"))
	})
	var result any
	_, err := m.request(context.Background(), "GET", "https://api.github.com/app", "Bearer secret-token", nil, "", &result)
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("unsafe redirect/error: %v", err)
	}
}

func TestCanonicalRepositoryRejectsEmbeddedSSHPassword(t *testing.T) {
	if _, _, err := CanonicalRepo("ssh://git:private-password@github.com/team/app.git"); err == nil {
		t.Fatal("SSH URL with embedded password accepted")
	}
}
func TestProviderPaginationNeverForwardsTokensToAnotherHost(t *testing.T) {
	for _, provider := range []string{"github", "bitbucket"} {
		t.Run(provider, func(t *testing.T) {
			calls := 0
			m := testManager(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if provider == "github" {
					w.Header().Set("Link", `<https://attacker.invalid/page>; rel="next"`)
					fmt.Fprint(w, `{"repositories":[]}`)
				} else {
					fmt.Fprint(w, `{"values":[],"next":"https://attacker.invalid/page"}`)
				}
			})
			if provider == "github" {
				fixture, _ := githubFixture(t)
				state, _ := fixture.load()
				m.save(state)
				m.githubAccess[""] = accessToken{Value: "token", Expires: time.Now().Add(time.Hour)}
			} else {
				m.SaveBitbucket("client", "secret")
				m.bitbucketAccess = accessToken{Value: "token", Expires: time.Now().Add(time.Hour)}
			}
			var err error
			if provider == "github" {
				_, err = m.Repositories(context.Background(), provider, "")
			} else {
				_, err = m.Workspaces(context.Background())
			}
			if err == nil || calls != 1 {
				t.Fatalf("untrusted pagination followed: %d %v", calls, err)
			}
		})
	}
}
