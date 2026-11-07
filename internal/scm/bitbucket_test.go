package scm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestBitbucketAuthorizationExchangeAndAutomaticRotatingRefresh(t *testing.T) {
	exchanges := 0
	var mu sync.Mutex
	m := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/2.0/user/workspaces" {
			fmt.Fprint(w, `{"values":[]}`)
			return
		}
		if r.URL.Path != "/site/oauth2/access_token" {
			http.NotFound(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "client" || pass != "consumer-secret" {
			t.Error("missing confidential client authentication")
		}
		r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		exchanges++
		if exchanges == 1 {
			if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "browser-code" || r.Form.Get("redirect_uri") != "http://localhost:18081/connections/bitbucket/callback" {
				t.Errorf("wrong code exchange: %v", r.Form)
			}
		} else if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != fmt.Sprintf("refresh-%d", exchanges-1) {
			t.Errorf("refresh rotation lost: %v", r.Form)
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("access-%d", exchanges), "refresh_token": fmt.Sprintf("refresh-%d", exchanges), "expires_in": 3600, "scopes": "account repository"})
	})
	if err := m.SaveBitbucket("client", "consumer-secret"); err != nil {
		t.Fatal(err)
	}
	auth, err := m.BitbucketAuthorizeURL("csrf-state", "http://localhost:18081/connections/bitbucket/callback")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(auth)
	if u.Host != "bitbucket.org" || u.Query().Get("state") != "csrf-state" || u.Query().Get("response_type") != "code" {
		t.Fatalf("authorization URL: %s", auth)
	}
	if err = m.ConnectBitbucket(context.Background(), "browser-code", "http://localhost:18081/connections/bitbucket/callback"); err != nil {
		t.Fatal(err)
	}
	reopened := New(m.root)
	reopened.Client = m.Client
	token, err := reopened.bitbucketToken(context.Background())
	if err != nil || token != "access-2" {
		t.Fatalf("restart refresh: %s %v", token, err)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if _, err := reopened.Workspaces(context.Background()); err != nil {
				t.Errorf("cached token: %v", err)
			}
		})
	}
	wg.Wait()
	if exchanges != 2 {
		t.Fatalf("concurrent refreshes: %d", exchanges)
	}
}

func TestBitbucketListsWorkspaceRepositoriesBranchesAndBindsCredentials(t *testing.T) {
	m := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/site/oauth2/access_token" {
			json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "refresh_token": "refresh", "expires_in": 3600, "scopes": "account repository"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer access" {
			t.Error("API not authenticated")
		}
		switch r.URL.Path {
		case "/2.0/user/workspaces":
			fmt.Fprint(w, `{"values":[{"workspace":{"slug":"team","name":"Team"}}]}`)
		case "/2.0/repositories/team":
			if r.URL.Query().Get("page") == "2" {
				fmt.Fprint(w, `{"values":[]}`)
				return
			}
			fmt.Fprint(w, `{"values":[{"full_name":"team/app","name":"app","mainbranch":{"name":"main"}}],"next":"https://api.bitbucket.org/2.0/repositories/team?page=2"}`)
		case "/2.0/repositories/team/app":
			fmt.Fprint(w, `{"full_name":"team/app","mainbranch":{"name":"main"}}`)
		case "/2.0/repositories/team/app/refs/branches":
			fmt.Fprint(w, `{"values":[{"name":"main"},{"name":"release/one"}]}`)
		default:
			http.NotFound(w, r)
		}
	})
	m.SaveBitbucket("client", "secret")
	if err := m.ConnectBitbucket(context.Background(), "code", "http://localhost/callback"); err != nil {
		t.Fatal(err)
	}
	workspaces, err := m.Workspaces(context.Background())
	if err != nil || len(workspaces) != 1 || workspaces[0].Slug != "team" {
		t.Fatalf("workspaces: %v %v", workspaces, err)
	}
	repos, err := m.Repositories(context.Background(), "bitbucket", "team")
	if err != nil || len(repos) == 0 || repos[0].URL != "https://bitbucket.org/team/app.git" {
		t.Fatalf("repositories: %v %v", repos, err)
	}
	branches, err := m.Branches(context.Background(), "bitbucket", "team/app")
	if err != nil || len(branches) != 2 || branches[1] != "release/one" {
		t.Fatalf("branches: %v %v", branches, err)
	}
	credential, err := m.Credential(context.Background(), "bitbucket", "team/app", "https://bitbucket.org/team/app.git")
	if err != nil || credential.Username != "x-token-auth" || credential.Token != "access" {
		t.Fatalf("credential: %+v %v", credential, err)
	}
	for _, repo := range []string{"https://attacker.invalid/team/app.git", "https://bitbucket.org/team/other.git"} {
		if _, err = m.Credential(context.Background(), "bitbucket", "team/app", repo); err == nil {
			t.Fatalf("credentials disclosed for %s", repo)
		}
	}
}

func TestBitbucketRejectsWriteScopesAndUntrustedPagination(t *testing.T) {
	m := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/site/oauth2/access_token" {
			fmt.Fprint(w, `{"access_token":"access","refresh_token":"refresh","expires_in":3600,"scopes":"account repository repository:write"}`)
			return
		}
		t.Error("unexpected API request")
	})
	m.SaveBitbucket("client", "secret")
	if err := m.ConnectBitbucket(context.Background(), "code", "http://localhost/callback"); err == nil || strings.Contains(err.Error(), "access_token") {
		t.Fatalf("excessive permissions accepted: %v", err)
	}
}

func TestBitbucketCIWaitsForAllResultsAndHandlesFailedBuilds(t *testing.T) {
	statuses := `[]`
	m := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/site/oauth2/access_token":
			fmt.Fprint(w, `{"access_token":"access","refresh_token":"refresh","expires_in":3600,"scopes":"account repository"}`)
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			fmt.Fprintf(w, `{"values":%s}`, statuses)
		default:
			http.NotFound(w, r)
		}
	})
	m.SaveBitbucket("client", "secret")
	m.ConnectBitbucket(context.Background(), "code", "http://localhost/callback")
	for _, tc := range []struct{ body, want string }{{`[]`, "pending"}, {`[{"state":"INPROGRESS"}]`, "pending"}, {`[{"state":"SUCCESSFUL"},{"state":"INPROGRESS"}]`, "pending"}, {`[{"state":"SUCCESSFUL"}]`, "passed"}, {`[{"state":"FAILED"}]`, "failed"}, {`[{"state":"STOPPED"}]`, "failed"}} {
		statuses = tc.body
		got, err := m.CIState(context.Background(), "bitbucket", "team/app", strings.Repeat("a", 40))
		if err != nil || got != tc.want {
			t.Fatalf("CI %s: %s %v", tc.body, got, err)
		}
	}
}
