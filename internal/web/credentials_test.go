package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vicape92/repodeploy/internal/deploy"
)

const fixturePassword = "correct-horse-battery-staple"

func sessionCookie(t *testing.T, base, password string) (*http.Cookie, int) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(base+"/login", url.Values{"password": {password}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == "repodeploy_session" {
			return c, resp.StatusCode
		}
	}
	return nil, resp.StatusCode
}

func withCookie(t *testing.T, method, target string, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, target, nil)
	req.AddCookie(cookie)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestNewCredentialsRecordKDFParameters(t *testing.T) {
	root := t.TempDir()
	if err := Initialize(root, fixturePassword); err != nil {
		t.Fatal(err)
	}
	c, err := loadCredentials(root)
	if err != nil {
		t.Fatal(err)
	}
	if c.KDF == nil || *c.KDF != currentKDF || c.Epoch == "" || !c.verify(fixturePassword) || c.verify("wrong-password-123") {
		t.Fatalf("unexpected credentials: %+v", c)
	}
}

func TestLegacyHashUpgradesOnLoginWithoutSigningOut(t *testing.T) {
	root := t.TempDir()
	if err := Initialize(root, fixturePassword); err != nil {
		t.Fatal(err)
	}
	c, _ := loadCredentials(root)
	legacy := credentials{Salt: c.Salt, Password: hashPassword(fixturePassword, c.Salt, legacyKDF), TokenHash: c.TokenHash}
	b, _ := json.Marshal(legacy)
	os.WriteFile(credentialsPath(root), b, 0600)

	store, err := deploy.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, false)
	if err != nil {
		t.Fatal(err)
	}
	h := newTestServer(t, server)
	cookie, status := sessionCookie(t, h, fixturePassword)
	if status != 303 || cookie == nil {
		t.Fatalf("legacy login failed: %d", status)
	}
	upgraded, _ := loadCredentials(root)
	if upgraded.KDF == nil || *upgraded.KDF != currentKDF || !upgraded.verify(fixturePassword) || upgraded.Epoch != legacy.Epoch {
		t.Fatalf("hash not upgraded safely: %+v", upgraded)
	}
	resp := withCookie(t, "GET", h+"/", cookie)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("upgrade signed the administrator out: %d", resp.StatusCode)
	}
}

func TestExternalPasswordChangeSignsOutSessionsAndTakesEffect(t *testing.T) {
	server, h, _ := webFixture(t)
	cookie, _ := sessionCookie(t, h.URL, fixturePassword)
	if err := ChangePassword(server.Store.Root, "a-brand-new-password"); err != nil {
		t.Fatal(err)
	}
	resp := withCookie(t, "GET", h.URL+"/", cookie)
	resp.Body.Close()
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/login" {
		t.Fatalf("old session survived a password change: %d", resp.StatusCode)
	}
	if _, status := sessionCookie(t, h.URL, fixturePassword); status != 401 {
		t.Fatalf("old password still accepted: %d", status)
	}
	if _, status := sessionCookie(t, h.URL, "a-brand-new-password"); status != 303 {
		t.Fatalf("new password rejected: %d", status)
	}
	if err := ChangePassword(server.Store.Root, "short"); err == nil {
		t.Fatal("weak password accepted")
	}
}

func TestRotateTokenRevokesOldTokenAndKeepsSessions(t *testing.T) {
	server, h, oldToken := webFixture(t)
	cookie, _ := sessionCookie(t, h.URL, fixturePassword)
	newToken, err := RotateToken(server.Store.Root)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := os.ReadFile(filepath.Join(server.Store.Root, "admin.token"))
	if strings.TrimSpace(string(stored)) != newToken || newToken == oldToken {
		t.Fatal("token file not replaced")
	}
	if info, _ := os.Stat(filepath.Join(server.Store.Root, "admin.token")); info.Mode().Perm() != 0600 {
		t.Fatalf("token file mode %v", info.Mode().Perm())
	}
	for token, want := range map[string]int{oldToken: 401, newToken: 200} {
		resp := request(t, "GET", h.URL+"/api/projects", token, nil)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("token status %d, want %d", resp.StatusCode, want)
		}
	}
	resp := withCookie(t, "GET", h.URL+"/", cookie)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("token rotation signed the browser out: %d", resp.StatusCode)
	}
}

func TestBrowserPasswordChangeRequiresCurrentPassword(t *testing.T) {
	_, h, _ := webFixture(t)
	client, csrf := loggedBrowser(t, h.URL)
	for _, tc := range []struct {
		current, next, confirm string
		want                   int
	}{
		{"wrong-current-password", "another-good-password", "another-good-password", 401},
		{fixturePassword, "another-good-password", "different-good-password", 400},
		{fixturePassword, "short", "short", 400},
	} {
		resp := postForm(t, client, h.URL+"/settings/password", url.Values{"_csrf": {csrf}, "current_password": {tc.current}, "new_password": {tc.next}, "confirm_password": {tc.confirm}})
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.want || !strings.Contains(string(body), `role="alert"`) {
			t.Fatalf("status %d, want %d with an inline error", resp.StatusCode, tc.want)
		}
	}
	resp := postForm(t, client, h.URL+"/settings/password", url.Values{"_csrf": {csrf}, "current_password": {fixturePassword}, "new_password": {"another-good-password"}, "confirm_password": {"another-good-password"}})
	resp.Body.Close()
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/login?changed=1" {
		t.Fatalf("change failed: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = client.Get(h.URL + "/")
	resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatal("session survived the browser password change")
	}
	if _, status := sessionCookie(t, h.URL, "another-good-password"); status != 303 {
		t.Fatalf("new password rejected: %d", status)
	}
}

func TestBrowserTokenRotationShowsWorkingToken(t *testing.T) {
	_, h, oldToken := webFixture(t)
	client, csrf := loggedBrowser(t, h.URL)
	resp := postForm(t, client, h.URL+"/settings/token", url.Values{"_csrf": {csrf}})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	match := regexp.MustCompile(`<code>([a-f0-9]{64})</code>`).FindStringSubmatch(string(body))
	if resp.StatusCode != 200 || len(match) != 2 || match[1] == oldToken {
		t.Fatalf("token not displayed: %d", resp.StatusCode)
	}
	resp = request(t, "GET", h.URL+"/api/projects", match[1], nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("displayed token rejected: %d", resp.StatusCode)
	}
}

func TestLoginErrorsRenderTheSignInPage(t *testing.T) {
	_, h, _ := webFixture(t)
	resp, err := http.PostForm(h.URL+"/login", url.Values{"password": {"definitely-not-it"}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(string(body), "Incorrect password.") || !strings.Contains(string(body), `name="password"`) {
		t.Fatalf("login error not rendered in page: %d", resp.StatusCode)
	}
}

func newTestServer(t *testing.T, server *Server) string {
	t.Helper()
	h := httptest.NewServer(server.Handler())
	t.Cleanup(h.Close)
	return h.URL
}

type auditGatedReader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	data    io.Reader
}

func (r *auditGatedReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return r.data.Read(p)
}

func TestPasswordChangeRevokesInFlightLogin(t *testing.T) {
	s, _, _ := webFixture(t)
	body := &auditGatedReader{
		started: make(chan struct{}), release: make(chan struct{}),
		data: strings.NewReader(url.Values{"password": {fixturePassword}}.Encode()),
	}
	req := httptest.NewRequest("POST", "http://localhost/login", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); s.Handler().ServeHTTP(response, req) }()
	<-body.started
	if err := ChangePassword(s.Store.Root, "new-password-after-revocation"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.credentials(); err != nil {
		t.Fatal(err)
	}
	close(body.release)
	<-done
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name != s.sessionCookie() {
			continue
		}
		check := httptest.NewRequest("GET", "http://localhost/api/projects", nil)
		check.AddCookie(cookie)
		result := httptest.NewRecorder()
		s.Handler().ServeHTTP(result, check)
		if result.Code == http.StatusOK {
			t.Fatal("old password created a valid administrator session after password change and epoch reload")
		}
	}
}

func TestConcurrentPasswordChangePreservesTokenRotation(t *testing.T) {
	root := t.TempDir()
	if err := Initialize(root, fixturePassword); err != nil {
		t.Fatal(err)
	}
	old, _ := loadCredentials(root)
	done := make(chan error, 1)
	go func() { done <- ChangePassword(root, "new-password-after-rotation") }()
	time.Sleep(20 * time.Millisecond)
	token, err := RotateToken(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current, err := loadCredentials(root)
	if err != nil {
		t.Fatal(err)
	}
	if current.TokenHash != tokenHash(token) {
		t.Fatalf("successful rotation was lost; previous token restored=%v", current.TokenHash == old.TokenHash)
	}
}

func TestSlowLoginBodyDoesNotOccupyPasswordSlot(t *testing.T) {
	server, _, _ := webFixture(t)
	body := &auditGatedReader{started: make(chan struct{}), release: make(chan struct{}), data: strings.NewReader("password=incorrect")}
	req := httptest.NewRequest("POST", "http://localhost/login", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	done := make(chan struct{})
	go func() { defer close(done); server.Handler().ServeHTTP(httptest.NewRecorder(), req) }()
	<-body.started
	occupied := len(server.loginSlots)
	close(body.release)
	<-done
	if occupied != 0 {
		t.Fatalf("slow request holds %d password verification slots", occupied)
	}
}

func TestTokenFileIsAuthoritativeAfterInterruptedRotation(t *testing.T) {
	root := t.TempDir()
	if err := Initialize(root, fixturePassword); err != nil {
		t.Fatal(err)
	}
	token := randomToken()
	if err := writeAtomic(tokenPath(root), []byte(token+"\n")); err != nil {
		t.Fatal(err)
	}
	c, err := loadCredentials(root)
	if err != nil {
		t.Fatal(err)
	}
	if c.TokenHash != tokenHash(token) {
		t.Fatal("atomic token file replacement did not revoke the old token")
	}
}
