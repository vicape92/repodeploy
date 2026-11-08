package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vicape92/repodeploy/internal/deploy"
	"github.com/vicape92/repodeploy/internal/scm"
)

type session struct {
	CSRF    string
	Expires time.Time
}
type Server struct {
	Store    *deploy.Store
	auth     credentials
	credInfo os.FileInfo
	secure   bool
	mu       sync.Mutex
	sessions map[string]session
	limiter  *loginLimiter
	// webhookSlots bounds concurrent webhook bodies held in memory.
	webhookSlots chan struct{}
	loginSlots   chan struct{}
	BaseURL      string
	flows        map[string]connectionFlow
	// TrustProxy honours X-Forwarded-For/X-Real-IP from a loopback reverse
	// proxy when identifying clients for login throttling.
	TrustProxy bool
	// AllowedHosts lists extra Host names accepted besides loopback names and
	// the public URL host.
	AllowedHosts []string
	// Deleter removes projects; serve wires it to the deployment engine.
	Deleter ProjectDeleter
}

func New(s *deploy.Store, secure bool) (*Server, error) {
	info, err := os.Stat(credentialsPath(s.Root))
	if err != nil {
		return nil, errors.New("initialize administrator credentials before serving")
	}
	c, err := loadCredentials(s.Root)
	if err != nil {
		return nil, err
	}
	return &Server{Store: s, auth: c, credInfo: info, secure: secure, sessions: make(map[string]session), limiter: newLoginLimiter(), webhookSlots: make(chan struct{}, 4), loginSlots: make(chan struct{}, 2), flows: make(map[string]connectionFlow)}, nil
}

// credentials returns the administrator credentials, reloading admin.json when
// another process (repodeploy passwd / rotate-token) replaced it. Every write
// is an atomic rename, so a changed inode, size or mtime marks a new version.
// A new epoch signs out all browser sessions. Failures are fatal for auth.
func (s *Server) credentials() (credentials, error) {
	info, err := os.Stat(credentialsPath(s.Store.Root))
	if err != nil {
		return credentials{}, errors.New("administrator credentials unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.credInfo != nil && os.SameFile(info, s.credInfo) && info.Size() == s.credInfo.Size() && info.ModTime().Equal(s.credInfo.ModTime()) {
		return s.auth, nil
	}
	c, err := loadCredentials(s.Store.Root)
	if err != nil {
		return credentials{}, err
	}
	if c.Epoch != s.auth.Epoch {
		s.sessions = make(map[string]session)
		s.flows = make(map[string]connectionFlow)
	}
	s.auth, s.credInfo = c, info
	return c, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		p := page{View: "login"}
		if r.URL.Query().Get("changed") == "1" {
			p.Notice = "Password changed. Sign in with the new password."
		}
		s.render(w, r, p)
	})
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /hooks/github/{id}", s.webhook)
	mux.HandleFunc("GET /api/projects", s.projects)
	mux.HandleFunc("POST /api/projects", s.saveProject)
	mux.HandleFunc("GET /api/projects/{id}", s.getProject)
	mux.HandleFunc("PUT /api/projects/{id}", s.saveProject)
	mux.HandleFunc("POST /api/projects/{id}/deploy", s.deploy)
	mux.HandleFunc("POST /api/projects/{id}/rollback", s.rollback)
	mux.HandleFunc("POST /api/projects/{id}/resume", s.resume)
	mux.HandleFunc("GET /api/projects/{id}/jobs", s.jobs)
	mux.HandleFunc("PUT /api/projects/{id}/secrets/{name}", s.secret)
	mux.HandleFunc("GET /api/projects/{id}/secrets", s.secretsList)
	mux.HandleFunc("DELETE /api/projects/{id}/secrets/{name}", s.deleteSecret)
	mux.HandleFunc("DELETE /api/projects/{id}", s.deleteProject)
	mux.HandleFunc("POST /api/projects/{id}/webhook-secret", s.webhookSecret)
	mux.HandleFunc("GET /api/jobs/{id}", s.job)
	mux.HandleFunc("GET /api/jobs/{id}/logs", s.logs)
	mux.HandleFunc("GET /{$}", s.dashboard)
	mux.HandleFunc("GET /projects/{id}", s.projectPage)
	mux.HandleFunc("GET /jobs/{id}", s.jobPage)
	mux.HandleFunc("POST /projects", s.saveProject)
	mux.HandleFunc("POST /projects/{id}", s.saveProject)
	mux.HandleFunc("POST /projects/{id}/deploy", s.deploy)
	mux.HandleFunc("POST /projects/{id}/rollback", s.rollback)
	mux.HandleFunc("POST /projects/{id}/resume", s.resume)
	mux.HandleFunc("POST /projects/{id}/webhook-secret", s.webhookSecret)
	mux.HandleFunc("GET /projects/{id}/delete", s.deletePage)
	mux.HandleFunc("POST /projects/{id}/delete", s.deleteProject)
	mux.HandleFunc("POST /projects/{id}/secrets", s.saveSecretForm)
	mux.HandleFunc("POST /projects/{id}/secrets/{name}/delete", s.deleteSecret)
	mux.Handle("GET /static/", staticHandler())
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /settings", s.settings)
	mux.HandleFunc("POST /settings/password", s.changePassword)
	mux.HandleFunc("POST /settings/token", s.rotateToken)
	mux.HandleFunc("GET /connections", s.connections)
	mux.HandleFunc("POST /connections/{provider}/start", s.connectionStart)
	mux.HandleFunc("GET /connections/{provider}/callback", s.connectionCallback)
	mux.HandleFunc("POST /connections/{provider}/disconnect", s.disconnectProvider)
	mux.HandleFunc("POST /connections/bitbucket/settings", s.bitbucketSettings)
	mux.HandleFunc("GET /connections/{provider}/repositories", s.repositorySelection)
	mux.HandleFunc("GET /connections/ssh", s.sshAssistant)
	mux.HandleFunc("POST /connections/ssh/key", s.sshGenerate)
	mux.HandleFunc("GET /connections/ssh/keys/{id}", s.sshAssistant)
	mux.HandleFunc("POST /connections/ssh/keys/{id}/test", s.sshTest)
	mux.HandleFunc("POST /connections/ssh/keys/{id}/delete", s.sshDelete)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			http.Error(w, "unexpected Host header; open the panel through localhost or its configured public URL", http.StatusMisdirectedRequest)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if s.secure {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		limit := int64(maxRequestBody)
		if strings.HasPrefix(r.URL.Path, "/hooks/github/") {
			limit = maxWebhookBody
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		if r.URL.Path == "/login" || strings.HasPrefix(r.URL.Path, "/hooks/github/") || (r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/static/")) || (r.Method == "GET" && (r.URL.Path == "/connections/github/callback" || r.URL.Path == "/connections/bitbucket/callback")) {
			mux.ServeHTTP(w, r)
			return
		}
		auth, err := s.credentials()
		if err != nil {
			http.Error(w, "administrator credentials unavailable; check the state directory", 500)
			return
		}
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		tokenOK := strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") && equal(tokenHash(bearer), auth.TokenHash)
		var sess session
		var ok bool
		if cookie, err := r.Cookie(s.sessionCookie()); err == nil {
			s.mu.Lock()
			sess, ok = s.sessions[cookie.Value]
			s.mu.Unlock()
			ok = ok && time.Now().Before(sess.Expires)
		}
		if !tokenOK && !ok {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "authentication required", 401)
			} else {
				http.Redirect(w, r, "/login", 303)
			}
			return
		}
		if !tokenOK && r.Method != "GET" && r.Method != "HEAD" {
			csrf := r.Header.Get("X-CSRF-Token")
			if csrf == "" {
				csrf = r.FormValue("_csrf")
			}
			if !equal(csrf, sess.CSRF) {
				http.Error(w, "invalid CSRF token", 403)
				return
			}
		}
		if ok {
			r = r.WithContext(context.WithValue(r.Context(), csrfKey{}, sess.CSRF))
			cookie, _ := r.Cookie(s.sessionCookie())
			r = r.WithContext(context.WithValue(r.Context(), sessionIDKey{}, cookie.Value))
		}
		mux.ServeHTTP(w, r)
	})
}

type csrfKey struct{}

const (
	maxRequestBody = 1 << 20
	// maxWebhookBody matches GitHub's 25 MB payload cap; large pushes would
	// otherwise be rejected and silently miss deployments.
	maxWebhookBody = 25 << 20
)

// sessionCookie names the session cookie. Behind HTTPS the __Host- prefix makes
// browsers insist on Secure, Path=/ and no Domain, so a sibling subdomain or a
// plain-HTTP response cannot plant or overwrite it.
func (s *Server) sessionCookie() string {
	if s.secure {
		return "__Host-repodeploy_session"
	}
	return "repodeploy_session"
}

// flowCookie names the provider-connection cookie. It is scoped to
// /connections/, which __Host- forbids, so HTTPS uses the __Secure- prefix.
func (s *Server) flowCookie(provider string) string {
	if s.secure {
		return "__Secure-repodeploy_connect_" + provider
	}
	return "repodeploy_connect_" + provider
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			http.Error(w, "invalid origin", 403)
			return
		}
	}
	fail := func(status int, message string) {
		s.renderStatus(w, r, status, page{View: "login", Error: message})
	}
	auth, err := s.credentials()
	if err != nil {
		fail(500, "Administrator credentials are unavailable. Check the state directory on the host.")
		return
	}
	client := s.clientIP(r)
	if wait, ok := s.limiter.allow(client); !ok {
		seconds := max(1, int(wait.Round(time.Second).Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		fail(429, fmt.Sprintf("Too many sign-in attempts from this address. Try again in %d seconds.", seconds))
		return
	}
	if !s.acquireLoginSlot(r) {
		fail(429, "The server is busy verifying another sign-in. Try again in a moment.")
		return
	}
	defer func() { <-s.loginSlots }()
	password := r.FormValue("password")
	if !auth.verify(password) {
		s.limiter.result(client, false)
		fail(401, "Incorrect password.")
		return
	}
	s.limiter.result(client, true)
	if auth.needsRehash() {
		s.upgradeHash(auth, password)
	}
	token := randomToken()
	s.mu.Lock()
	for key, sess := range s.sessions {
		if time.Now().After(sess.Expires) {
			delete(s.sessions, key)
		}
	}
	s.sessions[token] = session{CSRF: randomToken(), Expires: time.Now().Add(8 * time.Hour)}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: s.sessionCookie(), Value: token, Path: "/", HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteStrictMode, MaxAge: 8 * 3600})
	http.Redirect(w, r, "/", 303)
}

// acquireLoginSlot bounds concurrent Argon2 work (64 MiB each), waiting briefly
// instead of failing immediately when another verification is in progress.
func (s *Server) acquireLoginSlot(r *http.Request) bool {
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case s.loginSlots <- struct{}{}:
		return true
	case <-timer.C:
		return false
	case <-r.Context().Done():
		return false
	}
}

// upgradeHash re-derives the stored hash with the current parameters after a
// successful sign-in. It keeps the epoch, so sessions survive, and aborts if
// the credentials changed since they were verified.
func (s *Server) upgradeHash(verified credentials, password string) {
	current, err := loadCredentials(s.Store.Root)
	if err != nil || current.Salt != verified.Salt || current.Password != verified.Password {
		return
	}
	kdf := currentKDF
	current.Salt = randomToken()
	current.Password = hashPassword(password, current.Salt, kdf)
	current.KDF = &kdf
	saveCredentials(s.Store.Root, current)
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.sessionCookie()); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: s.sessionCookie(), Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", 303)
}
func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func decode(r *http.Request, v any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("only one JSON document is allowed")
	}
	return nil
}
func api(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/api/") }
func respondJob(w http.ResponseWriter, r *http.Request, j deploy.Job, err error) {
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	if api(r) {
		jsonResponse(w, 202, j)
	} else {
		http.Redirect(w, r, "/jobs/"+j.ID, 303)
	}
}

func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Projects()
	if err != nil {
		http.Error(w, "cannot read projects", 500)
		return
	}
	jsonResponse(w, 200, p)
}
func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	jsonResponse(w, 200, p)
}

// projectFromForm reads the browser project form. build_type selects which
// mode-specific fields apply; hidden fields of the other mode are discarded.
func projectFromForm(r *http.Request) deploy.Project {
	integer := func(key string) int { n, _ := strconv.Atoi(strings.TrimSpace(r.FormValue(key))); return n }
	text := func(key string) string { return strings.TrimSpace(r.FormValue(key)) }
	p := deploy.Project{Name: text("name"), Repo: text("repo"), Branch: text("branch"), Mode: r.FormValue("mode"), SSHKey: text("ssh_key"), KnownHosts: text("known_hosts"), Dockerfile: text("dockerfile"), ComposeFile: text("compose_file"), Port: integer("port"), ContainerPort: integer("container_port"), BindAddress: text("bind_address"), HealthURL: text("health_url"), HealthService: text("health_service"), HealthTimeout: integer("health_timeout"), PollSeconds: integer("poll_seconds"), Retain: integer("retain")}
	p.Connection = r.FormValue("connection")
	p.RepositoryID = r.FormValue("repository_id")
	p.WaitCI = r.FormValue("wait_ci") == "on"
	p.CITimeout = integer("ci_timeout")
	p.AllowPrivilegeEscalation = r.FormValue("privilege_escalation") == "allow"
	p.PullImages = r.FormValue("pull_images") == "on"
	switch r.FormValue("build_type") {
	case "compose":
		p.Dockerfile, p.Port, p.ContainerPort, p.BindAddress = "", 0, 0, ""
		p.HealthURL = text("compose_health_url")
	case "dockerfile":
		p.ComposeFile, p.HealthService = "", ""
	}
	return p
}

// renderProjectForm shows the submitted values again with the error next to
// the offending field instead of a bare error page that loses the input.
func (s *Server) renderProjectForm(w http.ResponseWriter, r *http.Request, status int, p deploy.Project, keyID string, err error) {
	field := deploy.ErrorField(err)
	if p.ComposeFile != "" && field == "health_url" {
		field = "compose_health_url"
	}
	view := page{View: "edit", Project: p, FormError: err.Error(), ErrorField: field, Wizard: r.FormValue("deploy") == "1", Provider: p.Connection}
	if keyID != "" {
		view.SSH = scm.SSHKey{ID: keyID}
	}
	s.renderStatus(w, r, status, view)
}

func (s *Server) saveProject(w http.ResponseWriter, r *http.Request) {
	var p deploy.Project
	browser := !api(r)
	keyID := ""
	if browser {
		p = projectFromForm(r)
		keyID = r.FormValue("ssh_key_id")
	} else if err := decode(r, &p); err != nil {
		http.Error(w, "invalid project JSON", 400)
		return
	}
	fail := func(status int, err error) {
		if browser {
			s.renderProjectForm(w, r, status, p, keyID, err)
		} else {
			http.Error(w, err.Error(), status)
		}
	}
	if keyID != "" {
		key, err := s.Store.Sources.SSHKey(keyID)
		if err != nil {
			fail(400, errors.New("SSH key no longer available; restart the SSH assistant"))
			return
		}
		p.Repo = key.Repo
		p.SSHKey = key.PrivatePath
		p.KnownHosts = key.KnownHostsPath
		p.Connection = ""
	}
	p.ID = r.PathValue("id")
	if p.ID != "" {
		if _, err := s.Store.Project(p.ID); err != nil {
			http.NotFound(w, r)
			return
		}
	}
	if p.Connection != "" {
		repo, err := s.Store.Sources.Repository(r.Context(), p.Connection, p.RepositoryID)
		canonical, provider, parseErr := scm.CanonicalRepo(p.Repo)
		if err != nil || parseErr != nil || provider != p.Connection || !strings.EqualFold(canonical, repo.URL) {
			fail(400, &deploy.FieldError{Field: "repo", Message: "repository does not match the connected provider selection; choose it again from repository connections"})
			return
		}
		if p.SSHKey == "" {
			p.Repo = repo.URL
		}
	}
	if p.WaitCI {
		repo, provider, err := s.Store.Sources.FindRepository(r.Context(), p.Repo)
		if err != nil {
			fail(400, &deploy.FieldError{Field: "wait_ci", Message: "CI/CD requires browser authorization for this repository; connect its provider first"})
			return
		}
		p.CIProvider = provider
		p.RepositoryID = repo.ID
	} else {
		p.CIProvider = ""
	}
	if err := deploy.ValidateProject(p); err != nil {
		fail(400, err)
		return
	}
	created := p.ID == ""
	saved, err := s.Store.SaveProject(p)
	if err != nil {
		fail(500, errors.New("cannot persist project"))
		return
	}
	p = saved
	if !browser {
		status := 200
		if created {
			status = 201
		}
		jsonResponse(w, status, p)
	} else if created && r.FormValue("deploy") == "1" {
		job, err := s.Store.Enqueue(p.ID, "manual", "", "", "")
		respondJob(w, r, job, err)
	} else {
		http.Redirect(w, r, "/projects/"+p.ID, 303)
	}
}
func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	j, err := s.Store.Enqueue(r.PathValue("id"), "manual", "", "", "")
	respondJob(w, r, j, err)
}
func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	target := r.FormValue("release_id")
	if api(r) {
		var body struct {
			Release string `json:"release_id"`
		}
		if err := decode(r, &body); err != nil {
			http.Error(w, "invalid rollback request", 400)
			return
		}
		target = body.Release
	}
	old, err := s.Store.Job(target)
	if err != nil || old.ProjectID != r.PathValue("id") || old.Phase != "succeeded" {
		http.Error(w, "select a retained successful release of this project", 400)
		return
	}
	if _, err = os.Stat(old.Manifest); err != nil {
		http.Error(w, "release artifact unavailable", 409)
		return
	}
	if !api(r) && r.FormValue("confirm") != "1" {
		p, err := s.Store.Project(old.ProjectID)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		s.render(w, r, page{View: "confirm-rollback", Project: p, Job: old})
		return
	}
	j, err := s.Store.Enqueue(r.PathValue("id"), "manual", "", "", target)
	respondJob(w, r, j, err)
}
func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if p.Blocked {
		http.Error(w, "restore a healthy release before resuming automation", 409)
		return
	}
	if err = s.Store.Resume(p.ID); err != nil {
		http.Error(w, "cannot resume project", 500)
		return
	}
	if api(r) {
		jsonResponse(w, 200, map[string]bool{"resumed": true})
	} else {
		http.Redirect(w, r, "/projects/"+p.ID, 303)
	}
}
func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.Store.Jobs(r.PathValue("id"))
	if err != nil {
		http.Error(w, "cannot read history", 500)
		return
	}
	jsonResponse(w, 200, jobs)
}
func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	j, err := s.Store.Job(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	jsonResponse(w, 200, j)
}
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.Job(id); err != nil {
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Get("follow") != "1" {
		log, err := s.Store.Logs(id)
		if err != nil {
			http.Error(w, "cannot read logs", 500)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, log)
		return
	}
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	offset := 0
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		log, err := s.Store.Logs(id)
		if err != nil {
			return
		}
		j, err := s.Store.Job(id)
		if err != nil {
			return
		}
		if offset > len(log) {
			offset = 0
		}
		b, _ := json.Marshal(map[string]string{"log": log[offset:], "phase": j.Phase})
		offset = len(log)
		if _, err = fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return
		}
		f.Flush()
		if j.Phase == "succeeded" || j.Phase == "failed" {
			fmt.Fprint(w, "event: done\ndata: {}\n\n")
			f.Flush()
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Server) secret(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "secret must contain 1–1048576 bytes", 400)
		return
	}
	if err = s.Store.SaveSecret(p.ID, r.PathValue("name"), b); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	jsonResponse(w, 200, map[string]bool{"saved": true})
}
func (s *Server) webhookSecret(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	secret := randomToken()
	if err = deploy.WritePrivate(filepath.Join(s.Store.Root, "secrets", p.ID+".webhook"), []byte(secret)); err != nil {
		http.Error(w, "cannot rotate webhook secret", 500)
		return
	}
	if api(r) {
		jsonResponse(w, 200, map[string]string{"secret": secret})
	} else {
		s.render(w, r, page{View: "secret", Project: p, Secret: secret})
	}
}
func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, err := s.Store.Project(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	secret, err := os.ReadFile(filepath.Join(s.Store.Root, "secrets", p.ID+".webhook"))
	if err != nil {
		http.Error(w, "webhook not configured", 403)
		return
	}
	// Only requests for a configured project (an unguessable ID) reach this
	// point, and at most a few large bodies are buffered at once.
	select {
	case s.webhookSlots <- struct{}{}:
		defer func() { <-s.webhookSlots }()
	default:
		w.Header().Set("Retry-After", "5")
		http.Error(w, "too many concurrent webhook deliveries", 503)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "payload too large", 413)
		return
	}
	j, err := deploy.AcceptGitHub(s.Store, p.ID, string(secret), r.Header.Get("X-GitHub-Event"), r.Header.Get("X-GitHub-Delivery"), r.Header.Get("X-Hub-Signature-256"), body)
	if errors.Is(err, deploy.ErrSignature) {
		http.Error(w, "invalid signature", 403)
		return
	}
	if errors.Is(err, deploy.ErrPaused) {
		jsonResponse(w, 202, map[string]string{"status": "paused"})
		return
	}
	if err != nil {
		http.Error(w, "invalid webhook event", 400)
		return
	}
	jsonResponse(w, 202, map[string]string{"job_id": j.ID})
}

type page struct {
	ConnectionView bool
	Connections    []scm.Status
	Provider       string
	Origin         string
	Error          string
	Manifest       string
	ManifestAction string
	Repositories   []scm.Repository
	Workspaces     []scm.Workspace
	Workspace      string
	Branches       []string
	SSH            scm.SSHKey
	SSHKeys        []sshKeyRow
	AccessKeyURL   string
	Wizard         bool
	View           string
	CSRF           string
	Projects       []deploy.Project
	Project        deploy.Project
	Jobs           []deploy.Job
	Job            deploy.Job
	Logs           string
	Secret         string
	Notice         string
	FormError      string
	ErrorField     string
	Secrets        []deploy.SecretInfo
	SecretError    string
	Latest         map[string]deploy.Job
	Refresh        bool
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, p page) {
	s.renderStatus(w, r, http.StatusOK, p)
}
func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, status int, p page) {
	p.CSRF, _ = r.Context().Value(csrfKey{}).(string)
	var b bytes.Buffer
	if err := pages.Execute(&b, p); err != nil {
		http.Error(w, "cannot render page", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(b.Bytes())
}
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Projects()
	if err != nil {
		http.Error(w, "cannot read projects", 500)
		return
	}
	latest, err := s.Store.LatestJobs()
	if err != nil {
		http.Error(w, "cannot read deployments", 500)
		return
	}
	view := page{View: "home", Projects: p, Latest: latest, Project: deploy.Project{Branch: "main", Mode: "poll", Dockerfile: "Dockerfile", Port: 8080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:8080/health", PollSeconds: 60, Retain: 5, HealthTimeout: 60}}
	for _, j := range latest {
		if j.Phase != "succeeded" && j.Phase != "failed" {
			view.Refresh = true
		}
	}
	if r.URL.Query().Get("deleted") == "1" {
		view.Notice = "Project deleted."
	}
	s.render(w, r, view)
}
func (s *Server) projectPage(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	view, err := s.projectView(p)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.render(w, r, view)
}
func (s *Server) jobPage(w http.ResponseWriter, r *http.Request) {
	j, err := s.Store.Job(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	log, _ := s.Store.Logs(j.ID)
	s.render(w, r, page{View: "job", Job: j, Logs: log})
}
