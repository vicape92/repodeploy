package web

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vicape92/repodeploy/internal/deploy"
	"golang.org/x/crypto/ssh"
)

type providerTransport struct{ base *url.URL }

func (p providerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copy := req.Clone(req.Context())
	u := *req.URL
	copy.URL = &u
	copy.URL.Scheme = p.base.Scheme
	copy.URL.Host = p.base.Host
	return http.DefaultTransport.RoundTrip(copy)
}
func loggedBrowser(t *testing.T, hURL string) (*http.Client, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(hURL+"/login", url.Values{"password": {"correct-horse-battery-staple"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp, err = client.Get(hURL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	match := regexp.MustCompile(`name="_csrf" value="([^"]+)"`).FindStringSubmatch(string(b))
	if len(match) != 2 {
		t.Fatal("no authenticated browser CSRF field")
	}
	return client, match[1]
}
func postForm(t *testing.T, client *http.Client, target string, values url.Values) *http.Response {
	t.Helper()
	response, err := client.PostForm(target, values)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
func callback(t *testing.T, target string, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", target, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestConnectionsPageRequiresLoginAndContainsBothBrowserProvidersAndSSH(t *testing.T) {
	_, h, _ := webFixture(t)
	client, _ := loggedBrowser(t, h.URL)
	response, err := client.Get(h.URL + "/connections")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(b), "Connect GitHub") || !strings.Contains(string(b), "Connect Bitbucket") || !strings.Contains(string(b), "SSH") {
		t.Fatalf("missing connection options: %d %s", response.StatusCode, b)
	}
	response = postForm(t, client, h.URL+"/connections/github/start", url.Values{})
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatalf("start bypasses CSRF: %d", response.StatusCode)
	}
}

func TestGitHubManifestCallbackIsBrowserBoundSingleUseAndWorksWithoutStrictSessionCookie(t *testing.T) {
	server, h, _ := webFixture(t)
	private, _ := rsa.GenerateKey(rand.Reader, 2048)
	encoded := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(private)}))
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app-manifests/provider-code/conversions" {
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "slug": "repodeploy-test", "pem": encoded})
			return
		}
		http.NotFound(w, r)
	}))
	defer provider.Close()
	u, _ := url.Parse(provider.URL)
	server.Store.Sources.Client = &http.Client{Transport: providerTransport{u}}
	client, csrf := loggedBrowser(t, h.URL)
	response := postForm(t, client, h.URL+"/connections/github/start", url.Values{"_csrf": {csrf}})
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(body), "https://github.com/settings/apps/new") || !strings.Contains(response.Header.Get("Content-Security-Policy"), "https://github.com") {
		t.Fatalf("manifest form blocked: %d %s", response.StatusCode, body)
	}
	var flowCookie *http.Cookie
	for _, cookie := range response.Cookies() {
		if cookie.Name == "repodeploy_connect_github" {
			flowCookie = cookie
		}
	}
	if flowCookie == nil || !flowCookie.HttpOnly || flowCookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("missing browser flow cookie")
	}
	match := regexp.MustCompile(`name="manifest" value="([^"]+)"`).FindStringSubmatch(string(body))
	if len(match) != 2 {
		t.Fatal("no GitHub manifest")
	}
	var manifest map[string]any
	if err := json.Unmarshal([]byte(html.UnescapeString(match[1])), &manifest); err != nil {
		t.Fatal(err)
	}
	permissions := manifest["default_permissions"].(map[string]any)
	if !strings.HasPrefix(manifest["name"].(string), "RepoDeploy-") {
		t.Fatalf("incorrect GitHub App name: %v", manifest["name"])
	}
	if manifest["setup_on_update"] != true {
		t.Fatal("GitHub permission changes cannot return to the panel")
	}
	for _, key := range []string{"contents", "metadata", "checks", "statuses"} {
		if permissions[key] != "read" {
			t.Fatalf("wrong app permission: %v", permissions)
		}
	}
	target := h.URL + "/connections/github/callback?code=provider-code&state=" + flowCookie.Value
	forged := callback(t, target, nil)
	forged.Body.Close()
	if forged.StatusCode != 403 {
		t.Fatal("unbound callback accepted")
	}
	valid := callback(t, target, flowCookie)
	valid.Body.Close()
	if valid.StatusCode != 303 || !strings.HasPrefix(valid.Header.Get("Location"), "https://github.com/apps/repodeploy-test/installations/new?") {
		t.Fatalf("manifest callback: %d %s", valid.StatusCode, valid.Header.Get("Location"))
	}
	replay := callback(t, target, flowCookie)
	replay.Body.Close()
	if replay.StatusCode != 403 {
		t.Fatal("manifest code replay accepted")
	}
}

func TestBitbucketAuthorizationAndCallbackKeepCredentialsOutOfHTML(t *testing.T) {
	server, h, _ := webFixture(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/site/oauth2/access_token" {
			fmt.Fprint(w, `{"access_token":"private-access","refresh_token":"private-refresh","expires_in":3600,"scopes":"account repository"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer provider.Close()
	u, _ := url.Parse(provider.URL)
	server.Store.Sources.Client = &http.Client{Transport: providerTransport{u}}
	client, csrf := loggedBrowser(t, h.URL)
	response := postForm(t, client, h.URL+"/connections/bitbucket/settings", url.Values{"_csrf": {csrf}, "client_id": {"client"}, "client_secret": {"private-consumer"}})
	response.Body.Close()
	if response.StatusCode != 303 {
		t.Fatal(response.StatusCode)
	}
	response = postForm(t, client, h.URL+"/connections/bitbucket/start", url.Values{"_csrf": {csrf}})
	response.Body.Close()
	location, _ := url.Parse(response.Header.Get("Location"))
	if response.StatusCode != 303 || location.Host != "bitbucket.org" {
		t.Fatalf("authorization redirect %d %v", response.StatusCode, location)
	}
	var cookie *http.Cookie
	for _, c := range response.Cookies() {
		if c.Name == "repodeploy_connect_bitbucket" {
			cookie = c
		}
	}
	response = callback(t, h.URL+"/connections/bitbucket/callback?code=code&state="+location.Query().Get("state"), cookie)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || strings.Contains(string(body), "private-access") || strings.Contains(string(body), "private-refresh") || strings.Contains(string(body), "private-consumer") {
		t.Fatalf("callback failure/disclosure: %d %s", response.StatusCode, body)
	}
	status, err := server.Store.Sources.Status("bitbucket")
	if err != nil || !status.Connected {
		t.Fatal("not connected")
	}
}

func TestAuthorizationStateExpiresAndRejectsUntrustedCallbackOrigin(t *testing.T) {
	server, h, _ := webFixture(t)
	client, csrf := loggedBrowser(t, h.URL)
	response := postForm(t, client, h.URL+"/connections/github/start", url.Values{"_csrf": {csrf}})
	response.Body.Close()
	var cookie *http.Cookie
	for _, c := range response.Cookies() {
		if c.Name == "repodeploy_connect_github" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("missing flow cookie")
	}
	server.mu.Lock()
	flow := server.flows[cookie.Value]
	flow.Expires = time.Now().Add(-time.Second)
	server.flows[cookie.Value] = flow
	server.mu.Unlock()
	expired := callback(t, h.URL+"/connections/github/callback?code=bad&state="+cookie.Value, cookie)
	expired.Body.Close()
	if expired.StatusCode != 403 {
		t.Fatal("expired flow accepted")
	}
	req, _ := http.NewRequest("POST", h.URL+"/connections/github/start", strings.NewReader(url.Values{"_csrf": {csrf}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "attacker.invalid"
	u, _ := url.Parse(h.URL)
	for _, cookie := range client.Jar.Cookies(u) {
		req.AddCookie(cookie)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	// Rejected by Host validation before the connection origin is computed.
	if response.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("Host header poisoned registration: %d", response.StatusCode)
	}
}

func TestRepositoryPickerCreatesDeploymentWithSelectedBranchAndCIPolicy(t *testing.T) {
	server, h, token := webFixture(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/site/oauth2/access_token":
			fmt.Fprint(w, `{"access_token":"access","refresh_token":"refresh","expires_in":3600,"scopes":"account repository"}`)
		case "/2.0/user/workspaces":
			fmt.Fprint(w, `{"values":[{"workspace":{"slug":"team","name":"Team"}}]}`)
		case "/2.0/repositories/team":
			fmt.Fprint(w, `{"values":[{"full_name":"team/app","mainbranch":{"name":"main"}}]}`)
		case "/2.0/repositories/team/app":
			fmt.Fprint(w, `{"full_name":"team/app","mainbranch":{"name":"main"}}`)
		case "/2.0/repositories/team/app/refs/branches":
			fmt.Fprint(w, `{"values":[{"name":"main"},{"name":"release"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	u, _ := url.Parse(provider.URL)
	server.Store.Sources.Client = &http.Client{Transport: providerTransport{u}}
	server.Store.Sources.SaveBitbucket("client", "secret")
	if err := server.Store.Sources.ConnectBitbucket(context.Background(), "code", "http://localhost/callback"); err != nil {
		t.Fatal(err)
	}
	client, csrf := loggedBrowser(t, h.URL)
	response, err := client.Get(h.URL + "/connections/bitbucket/repositories?repository=team%2Fapp")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(body), `name="connection" value="bitbucket"`) || !strings.Contains(string(body), `value="release"`) || !strings.Contains(string(body), "Wait for CI/CD") {
		t.Fatalf("incomplete picker: %d %s", response.StatusCode, body)
	}
	values := url.Values{"_csrf": {csrf}, "connection": {"bitbucket"}, "repository_id": {"team/app"}, "repo": {"https://bitbucket.org/team/app.git"}, "name": {"Team app"}, "branch": {"release"}, "mode": {"poll"}, "dockerfile": {"Dockerfile"}, "port": {"18082"}, "container_port": {"8080"}, "health_url": {"http://127.0.0.1:18082/health"}, "wait_ci": {"on"}, "ci_timeout": {"60"}, "deploy": {"1"}}
	response = postForm(t, client, h.URL+"/projects", values)
	response.Body.Close()
	if response.StatusCode != 303 || !strings.HasPrefix(response.Header.Get("Location"), "/jobs/") {
		t.Fatalf("save/deploy: %d %s", response.StatusCode, response.Header.Get("Location"))
	}
	projects, _ := server.Store.Projects()
	if len(projects) != 1 || !projects[0].WaitCI || projects[0].CIProvider != "bitbucket" || projects[0].Branch != "release" {
		t.Fatalf("settings lost: %+v", projects)
	}
	jobs, _ := server.Store.Jobs(projects[0].ID)
	if len(jobs) != 1 || jobs[0].Phase != "queued" || !jobs[0].Project.WaitCI {
		t.Fatalf("not queued with CI policy: %+v", jobs)
	}
	forged := projects[0]
	forged.Repo = "https://bitbucket.org/team/other.git"
	response = request(t, "PUT", h.URL+"/api/projects/"+forged.ID, token, forged)
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal("provider identity can be reassigned to unauthorized repository")
	}
}

func TestWaitCIRequiresProviderAccessAndValidTimeout(t *testing.T) {
	_, h, token := webFixture(t)
	p := deploy.Project{Name: "app", Repo: "https://github.com/team/app.git", Branch: "main", Mode: "manual", Dockerfile: "Dockerfile", Port: 18082, ContainerPort: 8080, HealthURL: "http://127.0.0.1:18082/health", WaitCI: true, CITimeout: 1800}
	response := request(t, "POST", h.URL+"/api/projects", token, p)
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatalf("CI access silently bypassed: %d", response.StatusCode)
	}
	p.WaitCI = false
	p.CITimeout = -1
	response = request(t, "POST", h.URL+"/api/projects", token, p)
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal("invalid CI timeout accepted")
	}
}

func TestSSHWizardCreatesKeyInPanelWithoutExposingPrivateKey(t *testing.T) {
	server, h, _ := webFixture(t)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	host, _ := ssh.NewPublicKey(pub)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/meta" {
			t.Error("unexpected key source")
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ssh_keys": []string{strings.TrimSpace(string(ssh.MarshalAuthorizedKey(host)))}})
	}))
	defer provider.Close()
	u, _ := url.Parse(provider.URL)
	server.Store.Sources.Client = &http.Client{Transport: providerTransport{u}}
	client, csrf := loggedBrowser(t, h.URL)
	response := postForm(t, client, h.URL+"/connections/ssh/key", url.Values{"_csrf": {csrf}, "repo": {"https://github.com/team/app.git"}})
	response.Body.Close()
	if response.StatusCode != 303 {
		t.Fatal(response.StatusCode)
	}
	response, err := client.Get(h.URL + response.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(body), "ssh-ed25519 ") || !strings.Contains(string(body), "Copy public key") || !strings.Contains(string(body), "https://github.com/team/app/settings/keys") || strings.Contains(string(body), "PRIVATE KEY") {
		t.Fatalf("incomplete or unsafe SSH wizard: %d", response.StatusCode)
	}
}

func TestSSHKeyListAndDeleteProtectsKeysInUse(t *testing.T) {
	server, h, _ := webFixture(t)
	root := server.Store.Root
	makeKey := func(id string) string {
		dir := filepath.Join(root, "ssh", id)
		os.MkdirAll(dir, 0700)
		os.WriteFile(filepath.Join(dir, "key"), []byte("x"), 0600)
		os.WriteFile(filepath.Join(dir, "known_hosts"), []byte("x"), 0600)
		meta, _ := json.Marshal(map[string]string{"id": id, "repo": "git@github.com:team/app.git", "created": time.Now().UTC().Format(time.RFC3339)})
		os.WriteFile(filepath.Join(dir, "metadata.json"), meta, 0600)
		return filepath.Join(dir, "key")
	}
	used, unused := strings.Repeat("1", 32), strings.Repeat("2", 32)
	usedPath := makeKey(used)
	makeKey(unused)
	if _, err := server.Store.SaveProject(deploy.Project{Name: "keyed-app", Repo: "git@github.com:team/app.git", Branch: "main", Mode: "manual", Dockerfile: "Dockerfile", Port: 18080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:18080/health", SSHKey: usedPath, KnownHosts: filepath.Join(filepath.Dir(usedPath), "known_hosts")}); err != nil {
		t.Fatal(err)
	}
	client, csrf := loggedBrowser(t, h.URL)
	resp, _ := client.Get(h.URL + "/connections")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "keyed-app") || !strings.Contains(string(body), "/connections/ssh/keys/"+unused+"/delete") || strings.Contains(string(body), "/connections/ssh/keys/"+used+"/delete") {
		t.Fatal("key list does not show usage and delete actions correctly")
	}
	resp = postForm(t, client, h.URL+"/connections/ssh/keys/"+used+"/delete", url.Values{"_csrf": {csrf}})
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 409 || !strings.Contains(string(body), "keyed-app") {
		t.Fatalf("in-use key deletion: %d", resp.StatusCode)
	}
	if _, err := os.Stat(usedPath); err != nil {
		t.Fatal("in-use key was deleted")
	}
	resp = postForm(t, client, h.URL+"/connections/ssh/keys/"+unused+"/delete", url.Values{"_csrf": {csrf}})
	resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatalf("unused key deletion: %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(root, "ssh", unused)); !os.IsNotExist(err) {
		t.Fatal("unused key remains")
	}
}
