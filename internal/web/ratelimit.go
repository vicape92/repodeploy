package web

import (
	"container/list"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	loginWindow        = time.Minute
	loginPerIPWindow   = 5 // attempts per client per window
	loginLockThreshold = 5 // consecutive failures before an escalating lockout
	loginMaxLock       = 15 * time.Minute
	loginMaxClients    = 10000
)

type clientAttempts struct {
	recent      []time.Time
	failures    int
	lockedUntil time.Time
	element     *list.Element
}

// loginLimiter throttles password attempts per client or IPv6 subnet.
type loginLimiter struct {
	mu      sync.Mutex
	clients map[string]*clientAttempts
	lru     list.List
	now     func() time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{clients: make(map[string]*clientAttempts), now: time.Now}
}

func recentSince(times []time.Time, cutoff time.Time) []time.Time {
	kept := times[:0]
	for _, at := range times {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	return kept
}

// allow records an attempt from client. When refused, it reports how long the
// client should wait before retrying.
func (l *loginLimiter) allow(client string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-loginWindow)
	client = subnetClient(client)
	c := l.clients[client]
	if c == nil {
		if len(l.clients) >= loginMaxClients {
			oldest := l.lru.Back()
			delete(l.clients, oldest.Value.(string))
			l.lru.Remove(oldest)
		}
		c = &clientAttempts{}
		c.element = l.lru.PushFront(client)
		l.clients[client] = c
	} else {
		l.lru.MoveToFront(c.element)
	}
	if now.Before(c.lockedUntil) {
		return c.lockedUntil.Sub(now), false
	}
	c.recent = recentSince(c.recent, cutoff)
	if len(c.recent) >= loginPerIPWindow {
		return c.recent[0].Add(loginWindow).Sub(now), false
	}
	c.recent = append(c.recent, now)
	return 0, true
}

// result records the outcome of an allowed attempt.
func (l *loginLimiter) result(client string, success bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	client = subnetClient(client)
	if success {
		if c := l.clients[client]; c != nil {
			l.lru.Remove(c.element)
		}
		delete(l.clients, client)
		return
	}
	c := l.clients[client]
	if c == nil {
		return
	}
	c.failures++
	if c.failures >= loginLockThreshold {
		lock := loginWindow << min(c.failures-loginLockThreshold, 4)
		c.lockedUntil = l.now().Add(min(lock, loginMaxLock))
	}
}

func subnetClient(client string) string {
	ip := net.ParseIP(client)
	if ip == nil || ip.To4() != nil {
		return client
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// clientIP returns the address used for throttling. Forwarding headers are
// honoured only with TrustProxy and only from a loopback peer, i.e. a reverse
// proxy on the same host. The rightmost X-Forwarded-For entry is the address
// that proxy observed; earlier entries are client-controlled.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if !s.TrustProxy || peer == nil || !peer.IsLoopback() {
		return host
	}
	if forwarded := r.Header.Values("X-Forwarded-For"); len(forwarded) > 0 {
		parts := strings.Split(strings.Join(forwarded, ","), ",")
		if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
			return ip.String()
		}
	}
	if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ip != nil {
		return ip.String()
	}
	return host
}

// hostAllowed defends against DNS rebinding: a browser tricked into sending
// requests to the loopback panel still carries the attacker's host name.
func (s *Server) hostAllowed(host string) bool {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.TrimSuffix(strings.Trim(name, "[]"), ".")
	if name == "" {
		return false
	}
	if strings.EqualFold(name, "localhost") {
		return true
	}
	if ip := net.ParseIP(name); ip != nil && ip.IsLoopback() {
		return true
	}
	if u, err := url.Parse(s.BaseURL); err == nil && u.Hostname() != "" && strings.EqualFold(name, u.Hostname()) {
		return true
	}
	for _, allowed := range s.AllowedHosts {
		if strings.EqualFold(name, strings.TrimSuffix(allowed, ".")) {
			return true
		}
	}
	return false
}
