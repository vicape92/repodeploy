package web

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, page{View: "settings"})
}

// changePassword requires the current password, applies the same throttling
// as sign-in, and signs out every browser session on success.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, message string) {
		s.renderStatus(w, r, status, page{View: "settings", Error: message})
	}
	if err := r.ParseForm(); err != nil {
		fail(400, "Invalid form.")
		return
	}
	auth, err := s.credentials()
	if err != nil {
		fail(500, "Administrator credentials are unavailable.")
		return
	}
	client := s.clientIP(r)
	if wait, ok := s.limiter.allow(client); !ok {
		seconds := max(1, int(wait.Round(time.Second).Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		fail(429, fmt.Sprintf("Too many attempts. Try again in %d seconds.", seconds))
		return
	}
	if !s.acquireLoginSlot(r) {
		fail(429, "The server is busy verifying another password. Try again in a moment.")
		return
	}
	defer func() { <-s.loginSlots }()
	if !auth.verify(r.FormValue("current_password")) {
		s.limiter.result(client, false)
		fail(401, "The current password is incorrect.")
		return
	}
	s.limiter.result(client, true)
	next := r.FormValue("new_password")
	if next != r.FormValue("confirm_password") {
		fail(400, "The new passwords do not match.")
		return
	}
	if err = validatePassword(next); err != nil {
		fail(400, err.Error())
		return
	}
	f, err := lockCredentials(s.Store.Root)
	if err != nil {
		fail(500, "Administrator credentials are unavailable.")
		return
	}
	current, err := readCredentials(s.Store.Root)
	if epoch, ok := r.Context().Value(sessionEpochKey{}).(string); err != nil || current.Epoch != auth.Epoch || current.Password != auth.Password || current.Salt != auth.Salt || (ok && epoch != current.Epoch) {
		unlockCredentials(f)
		fail(401, "The current password is incorrect.")
		return
	}
	err = saveCredentials(s.Store.Root, newPasswordCredentials(next, current.TokenHash))
	unlockCredentials(f)
	if err != nil {
		fail(400, err.Error())
		return
	}
	s.mu.Lock()
	s.sessions = make(map[string]session)
	s.flows = make(map[string]connectionFlow)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: s.sessionCookie(), Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login?changed=1", 303)
}

func (s *Server) rotateToken(w http.ResponseWriter, r *http.Request) {
	token, err := RotateToken(s.Store.Root)
	if err != nil {
		s.renderStatus(w, r, 500, page{View: "settings", Error: "Cannot rotate the CLI token: " + err.Error()})
		return
	}
	s.render(w, r, page{View: "token", Secret: token})
}
