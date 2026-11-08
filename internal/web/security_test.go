package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRejectsUnexpectedHostHeaders(t *testing.T) {
	server, h, token := webFixture(t)
	server.BaseURL = "https://deploy.example.org"
	server.AllowedHosts = []string{"panel.internal"}
	for host, want := range map[string]int{
		"attacker.example":        http.StatusMisdirectedRequest,
		"attacker.example:8081":   http.StatusMisdirectedRequest,
		"localhost:8081":          200,
		"127.0.0.1:8081":          200,
		"[::1]:8081":              200,
		"deploy.example.org":      200,
		"DEPLOY.example.org:443":  200,
		"panel.internal":          200,
		"deploy.example.org.evil": http.StatusMisdirectedRequest,
	} {
		req, _ := http.NewRequest("GET", h.URL+"/api/projects", nil)
		req.Host = host
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("Host %q: status %d, want %d", host, resp.StatusCode, want)
		}
	}
}

func TestLoginThrottlingIsPerClientBehindTrustedProxy(t *testing.T) {
	server, h, _ := webFixture(t)
	server.TrustProxy = true
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	login := func(ip, password string) *http.Response {
		req, _ := http.NewRequest("POST", h.URL+"/login", strings.NewReader(url.Values{"password": {password}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		// A spoofed leftmost entry must not matter: the proxy appends the peer.
		req.Header.Set("X-Forwarded-For", "203.0.113.250, "+ip)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	for i := 0; i < loginPerIPWindow; i++ {
		if resp := login("198.51.100.7", "wrong-password-guess"); resp.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, resp.StatusCode)
		}
	}
	if resp := login("198.51.100.7", "correct-horse-battery-staple"); resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("attacker not throttled: %d", resp.StatusCode)
	}
	if resp := login("192.0.2.10", "correct-horse-battery-staple"); resp.StatusCode != 303 {
		t.Fatalf("administrator locked out by another client: %d", resp.StatusCode)
	}
}

func TestClientIPIgnoresForwardingHeadersUnlessTrustedLoopbackPeer(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("POST", "/login", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	if got := s.clientIP(r); got != "127.0.0.1" {
		t.Fatalf("untrusted header honoured: %s", got)
	}
	s.TrustProxy = true
	if got := s.clientIP(r); got != "198.51.100.7" {
		t.Fatalf("trusted proxy header ignored: %s", got)
	}
	r.RemoteAddr = "203.0.113.9:5000"
	if got := s.clientIP(r); got != "203.0.113.9" {
		t.Fatalf("non-loopback peer could spoof its address: %s", got)
	}
}

func TestLoginLimiterEscalatesAndResetsOnSuccess(t *testing.T) {
	l := newLoginLimiter()
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	for i := 0; i < loginLockThreshold; i++ {
		if _, ok := l.allow("a"); !ok {
			t.Fatalf("attempt %d refused", i)
		}
		l.result("a", false)
		now = now.Add(20 * time.Second)
	}
	wait, ok := l.allow("a")
	if ok || wait <= 0 || wait > loginWindow {
		t.Fatalf("first lockout: ok=%v wait=%v", ok, wait)
	}
	now = now.Add(loginWindow)
	if _, ok = l.allow("a"); !ok {
		t.Fatal("lockout did not expire")
	}
	l.result("a", false)
	if wait, ok = l.allow("a"); ok || wait <= loginWindow {
		t.Fatalf("lockout did not escalate: %v", wait)
	}
	now = now.Add(loginMaxLock)
	if _, ok = l.allow("a"); !ok {
		t.Fatal("escalated lockout exceeded maximum")
	}
	l.result("a", true)
	if _, ok = l.allow("a"); !ok {
		t.Fatal("success did not reset the client")
	}
}

func TestLoginLimiterDoesNotLetOtherNetworksLockOutFreshClients(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i < 40; i++ {
		client := fmt.Sprintf("198.51.100.%d", i)
		l.allow(client)
		l.result(client, false)
	}
	if _, ok := l.allow("203.0.113.100"); !ok {
		t.Fatal("other networks exhausted the administrator's login budget")
	}
}

func TestLoginLimiterSharesIPv6SubnetBudget(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i < loginPerIPWindow; i++ {
		if _, ok := l.allow(fmt.Sprintf("2001:db8:1::%x", i)); !ok {
			t.Fatal("attempt rejected early")
		}
	}
	if _, ok := l.allow("2001:db8:1::ffff"); ok {
		t.Fatal("IPv6 address rotation bypassed the subnet limit")
	}
	if _, ok := l.allow("2001:db8:2::1"); !ok {
		t.Fatal("another subnet was blocked")
	}
}

func TestLoginLimiterFullTableDoesNotRejectAllNewClients(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i < loginMaxClients; i++ {
		l.allow(fmt.Sprintf("client-%d", i))
	}
	if _, ok := l.allow("administrator"); !ok {
		t.Fatal("full client table blocks every new client")
	}
	if len(l.clients) > loginMaxClients {
		t.Fatal("client tracking is unbounded")
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (r *deadlineRecorder) SetReadDeadline(at time.Time) error {
	r.deadlines = append(r.deadlines, at)
	return nil
}
func TestBodylessRequestsDoNotInstallReadDeadline(t *testing.T) {
	s, _, _ := webFixture(t)
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/login", nil))
	if len(w.deadlines) != 0 {
		t.Fatal("bodyless request inherits a deadline that can cancel long-lived responses")
	}
}
func TestFormReadClearsBodyDeadlineBeforeVerification(t *testing.T) {
	s, _, _ := webFixture(t)
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest("POST", "http://localhost/login", strings.NewReader("password=incorrect-password"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.Handler().ServeHTTP(w, r)
	if len(w.deadlines) < 2 || w.deadlines[0].IsZero() || !w.deadlines[1].IsZero() {
		t.Fatal("body deadline missing or not cleared on EOF")
	}
}
