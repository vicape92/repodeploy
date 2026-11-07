package scm

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func githubFixture(t *testing.T) (*Manager, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	m := New(t.TempDir())
	encoded := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if err = m.save(connections{GitHub: githubConfig{AppID: 123, Slug: "repodeploy-test", PrivateKey: encoded, InstallationID: 42}}); err != nil {
		t.Fatal(err)
	}
	return m, key
}
func verifyJWT(t *testing.T, token string, key *rsa.PrivateKey) {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(token, "Bearer "), ".")
	if len(parts) != 3 {
		t.Fatal("missing App JWT")
	}
	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], signature); err != nil {
		t.Fatal("invalid App signature:", err)
	}
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var parsed struct {
		Issuer  string `json:"iss"`
		Expires int64  `json:"exp"`
		Issued  int64  `json:"iat"`
	}
	json.Unmarshal(claims, &parsed)
	if parsed.Issuer != "123" || parsed.Expires <= time.Now().Unix() || parsed.Expires-time.Now().Unix() > 600 || parsed.Issued > time.Now().Unix() {
		t.Fatalf("invalid App claims: %s", claims)
	}
}

func TestGitHubManifestPersistsCredentialsAndVerifiesInstallationOwnership(t *testing.T) {
	template, key := githubFixture(t)
	config, _ := template.load()
	m := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app-manifests/browser-code/conversions":
			json.NewEncoder(w).Encode(map[string]any{"id": 123, "slug": "repodeploy-test", "pem": config.GitHub.PrivateKey})
		case "/app/installations/42":
			verifyJWT(t, r.Header.Get("Authorization"), key)
			fmt.Fprint(w, `{"id":42,"app_id":123,"permissions":{"contents":"read","metadata":"read","checks":"read","statuses":"read"},"suspended_at":null}`)
		case "/app/installations/99":
			fmt.Fprint(w, `{"id":99,"app_id":666}`)
		default:
			http.NotFound(w, r)
		}
	})
	if err := m.RegisterGitHub(context.Background(), "browser-code"); err != nil {
		t.Fatal(err)
	}
	install, err := m.GitHubInstallURL("state")
	u, _ := url.Parse(install)
	if err != nil || u.Host != "github.com" || u.Path != "/apps/repodeploy-test/installations/new" || u.Query().Get("state") != "state" {
		t.Fatalf("installation URL %s %v", install, err)
	}
	if err = m.ConnectGitHub(context.Background(), 99); err == nil {
		t.Fatal("installation of another App accepted")
	}
	if err = m.ConnectGitHub(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	reopened := New(m.root)
	status, err := reopened.Status("github")
	if err != nil || !status.Connected {
		t.Fatalf("connection lost on restart: %+v %v", status, err)
	}
}

func TestGitHubListsRepositoriesAndBranchesWithRepositoryScopedTokens(t *testing.T) {
	m, key := githubFixture(t)
	tokenRequests := 0
	fixture := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/42/access_tokens":
			verifyJWT(t, r.Header.Get("Authorization"), key)
			var request struct {
				RepositoryIDs []int64           `json:"repository_ids"`
				Permissions   map[string]string `json:"permissions"`
			}
			json.NewDecoder(r.Body).Decode(&request)
			tokenRequests++
			token := "list-token"
			if len(request.RepositoryIDs) > 0 {
				if len(request.RepositoryIDs) != 1 || request.RepositoryIDs[0] != 17 {
					t.Fatalf("unscoped token: %+v", request)
				}
				token = "repo-token"
			}
			for _, value := range request.Permissions {
				if value != "read" {
					t.Fatal("requested write permission")
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"token": token, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
		case "/installation/repositories":
			if r.Header.Get("Authorization") != "Bearer list-token" {
				t.Error("wrong list token")
			}
			fmt.Fprint(w, `{"repositories":[{"id":17,"full_name":"team/app","default_branch":"main"}]}`)
		case "/repositories/17":
			if r.Header.Get("Authorization") != "Bearer repo-token" {
				t.Error("repository token not scoped")
			}
			fmt.Fprint(w, `{"id":17,"full_name":"team/app","default_branch":"main"}`)
		case "/repos/team/app/branches":
			fmt.Fprint(w, `[{"name":"main"},{"name":"release/one"}]`)
		default:
			http.NotFound(w, r)
		}
	})
	m.Client = fixture.Client
	repos, err := m.Repositories(context.Background(), "github", "")
	if err != nil || len(repos) != 1 || repos[0].URL != "https://github.com/team/app.git" {
		t.Fatalf("repositories: %+v %v", repos, err)
	}
	branches, err := m.Branches(context.Background(), "github", "17")
	if err != nil || len(branches) != 2 || branches[1] != "release/one" {
		t.Fatalf("branches: %v %v", branches, err)
	}
	cred, err := m.Credential(context.Background(), "github", "17", "https://github.com/team/app.git")
	if err != nil || cred.Username != "x-access-token" || cred.Token != "repo-token" {
		t.Fatalf("credential: %+v %v", cred, err)
	}
	if _, err = m.Credential(context.Background(), "github", "17", "https://github.com/team/other.git"); err == nil {
		t.Fatal("token disclosed for another repository")
	}
	if tokenRequests != 2 {
		t.Fatalf("tokens not cached: %d", tokenRequests)
	}
	m.githubAccess["17"] = accessToken{Value: "expired", Expires: time.Now().Add(-time.Second)}
	if _, err = m.Credential(context.Background(), "github", "17", "https://github.com/team/app.git"); err != nil || tokenRequests != 3 {
		t.Fatalf("token did not renew: %d %v", tokenRequests, err)
	}
}

func TestGitHubCICombinesStatusesAndLatestChecks(t *testing.T) {
	m, _ := githubFixture(t)
	m.githubAccess["17"] = accessToken{Value: "repo-token", Expires: time.Now().Add(time.Hour)}
	state := "pending"
	total := 0
	checks := `[]`
	fixture := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repositories/17":
			fmt.Fprint(w, `{"id":17,"full_name":"team/app","default_branch":"main"}`)
		case strings.HasSuffix(r.URL.Path, "/status"):
			fmt.Fprintf(w, `{"state":%q,"total_count":%d}`, state, total)
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			if r.URL.Query().Get("filter") != "latest" {
				t.Error("not selecting latest check attempts")
			}
			fmt.Fprintf(w, `{"check_runs":%s}`, checks)
		default:
			http.NotFound(w, r)
		}
	})
	m.Client = fixture.Client
	for _, tc := range []struct {
		state        string
		total        int
		checks, want string
	}{
		{"pending", 0, `[]`, "pending"}, {"success", 1, `[]`, "passed"}, {"success", 1, `[{"status":"in_progress"}]`, "pending"},
		{"pending", 0, `[{"status":"completed","conclusion":"success"}]`, "passed"}, {"failure", 1, `[]`, "failed"},
		{"success", 1, `[{"status":"completed","conclusion":"failure"}]`, "failed"}, {"success", 1, `[{"status":"completed","conclusion":"cancelled"}]`, "failed"},
	} {
		state, total, checks = tc.state, tc.total, tc.checks
		got, err := m.CIState(context.Background(), "github", "17", strings.Repeat("a", 40))
		if err != nil || got != tc.want {
			t.Fatalf("CI %s/%s: %s %v", state, checks, got, err)
		}
	}
}
