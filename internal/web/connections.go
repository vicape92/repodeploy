package web

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vicape92/repodeploy/internal/deploy"
	"github.com/vicape92/repodeploy/internal/scm"
)

type connectionFlow struct {
	SessionID string
	Kind      string
	Origin    string
	Expires   time.Time
}

type sessionIDKey struct{}

func (s *Server) connectionOrigin(r *http.Request) (string, error) {
	raw := s.BaseURL
	if raw == "" {
		raw = "http://" + r.Host
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("invalid panel address")
	}
	loopback := u.Hostname() == "localhost" || (net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return "", errors.New("open the panel on localhost or configure its public HTTPS URL")
	}
	return u.Scheme + "://" + u.Host, nil
}
func (s *Server) newConnectionFlow(w http.ResponseWriter, r *http.Request, provider, kind, origin string) (string, error) {
	sessionID, _ := r.Context().Value(sessionIDKey{}).(string)
	if sessionID == "" {
		return "", errors.New("sign in through the browser before connecting a provider")
	}
	state := randomToken()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, flow := range s.flows {
		if time.Now().After(flow.Expires) {
			delete(s.flows, key)
		}
	}
	s.flows[state] = connectionFlow{SessionID: sessionID, Kind: kind, Origin: origin, Expires: time.Now().Add(15 * time.Minute)}
	http.SetCookie(w, &http.Cookie{Name: s.flowCookie(provider), Value: state, Path: "/connections/", HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode, MaxAge: 900})
	return state, nil
}
func (s *Server) consumeConnectionFlow(r *http.Request, provider string) (connectionFlow, error) {
	state := r.URL.Query().Get("state")
	cookie, err := r.Cookie(s.flowCookie(provider))
	if err != nil || state == "" || !equal(cookie.Value, state) {
		return connectionFlow{}, errors.New("connection could not be verified; start again from this panel")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	flow, ok := s.flows[state]
	if !ok {
		return flow, errors.New("connection expired or already used; start again")
	}
	session, sessionOK := s.sessions[flow.SessionID]
	origin, _ := url.Parse(flow.Origin)
	if !strings.HasPrefix(flow.Kind, provider+"-") || time.Now().After(flow.Expires) || !sessionOK || time.Now().After(session.Expires) || origin.Host != r.Host {
		return connectionFlow{}, errors.New("connection expired or belongs to another browser; start again")
	}
	delete(s.flows, state)
	return flow, nil
}
func (s *Server) connectionRender(w http.ResponseWriter, r *http.Request, p page) {
	p.ConnectionView = true
	s.render(w, r, p)
}

type sshKeyRow struct {
	Key   scm.SSHKey
	Users []string
}

func (s *Server) connectionsPage(r *http.Request) (page, int, error) {
	origin, err := s.connectionOrigin(r)
	if err != nil {
		return page{}, 400, err
	}
	states := []scm.Status{}
	for _, provider := range []string{"github", "bitbucket"} {
		state, err := s.Store.Sources.Status(provider)
		if err != nil {
			return page{}, 500, err
		}
		states = append(states, state)
	}
	keys, err := s.Store.Sources.SSHKeys()
	if err != nil {
		return page{}, 500, err
	}
	usage, err := s.Store.SSHKeyUsage()
	if err != nil {
		return page{}, 500, errors.New("cannot read projects")
	}
	rows := make([]sshKeyRow, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, sshKeyRow{Key: key, Users: usage[filepath.Clean(key.PrivatePath)]})
	}
	return page{View: "connections", Connections: states, Origin: origin, SSHKeys: rows}, 200, nil
}
func (s *Server) connections(w http.ResponseWriter, r *http.Request) {
	p, status, err := s.connectionsPage(r)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	s.connectionRender(w, r, p)
}

// sshDelete removes a generated key that no project or pending deployment uses.
func (s *Server) sshDelete(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.Sources.SSHKey(r.PathValue("id")); err != nil {
		http.NotFound(w, r)
		return
	}
	err := s.Store.DeleteSSHKey(r.PathValue("id"))
	if err != nil {
		p, status, pageErr := s.connectionsPage(r)
		if pageErr != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		p.Error = err.Error()
		if status == 200 {
			status = 409
		}
		p.ConnectionView = true
		s.renderStatus(w, r, status, p)
		return
	}
	http.Redirect(w, r, "/connections#ssh", 303)
}
func (s *Server) connectionStart(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if provider != "github" && provider != "bitbucket" {
		http.NotFound(w, r)
		return
	}
	origin, err := s.connectionOrigin(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	status, err := s.Store.Sources.Status(provider)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if provider == "github" && !status.Configured {
		organization := r.FormValue("organization")
		if organization != "" && !regexp.MustCompile(`^[A-Za-z0-9-]+$`).MatchString(organization) {
			http.Error(w, "invalid GitHub organization", 400)
			return
		}
		state, err := s.newConnectionFlow(w, r, provider, "github-manifest", origin)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		manifest := map[string]any{"name": "RepoDeploy-" + state[:8], "url": origin, "redirect_url": origin + "/connections/github/callback", "setup_url": origin + "/connections/github/callback", "setup_on_update": true, "public": false, "hook_attributes": map[string]any{"url": origin + "/hooks/github/app", "active": false}, "default_permissions": map[string]string{"contents": "read", "metadata": "read", "checks": "read", "statuses": "read"}, "request_oauth_on_install": false}
		b, _ := json.Marshal(manifest)
		action := "https://github.com/settings/apps/new"
		if organization != "" {
			action = "https://github.com/organizations/" + organization + "/settings/apps/new"
		}
		action += "?state=" + state
		w.Header().Set("Content-Security-Policy", strings.Replace(w.Header().Get("Content-Security-Policy"), "form-action 'self'", "form-action 'self' https://github.com", 1))
		s.connectionRender(w, r, page{View: "manifest", Manifest: string(b), ManifestAction: action})
		return
	}
	if !status.Configured {
		http.Redirect(w, r, "/connections#bitbucket", 303)
		return
	}
	kind := provider + "-authorize"
	if provider == "github" {
		kind = "github-install"
	}
	state, err := s.newConnectionFlow(w, r, provider, kind, origin)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	target := ""
	if provider == "github" {
		target, err = s.Store.Sources.GitHubInstallURL(state)
	} else {
		target, err = s.Store.Sources.BitbucketAuthorizeURL(state, origin+"/connections/bitbucket/callback")
	}
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, target, 303)
}
func (s *Server) bitbucketSettings(w http.ResponseWriter, r *http.Request) {
	if _, err := s.connectionOrigin(r); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := s.Store.Sources.SaveBitbucket(r.FormValue("client_id"), r.FormValue("client_secret")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/connections#bitbucket", 303)
}
func (s *Server) connectionCallback(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	flow, err := s.consumeConnectionFlow(r, provider)
	if err != nil {
		http.Error(w, err.Error(), 403)
		return
	}
	s.mu.Lock()
	session := s.sessions[flow.SessionID]
	s.mu.Unlock()
	r = r.WithContext(context.WithValue(context.WithValue(r.Context(), sessionIDKey{}, flow.SessionID), csrfKey{}, session.CSRF))
	if r.URL.Query().Get("error") != "" {
		s.connectionRender(w, r, page{View: "connected", Error: "Authorization was cancelled. Start again from repository connections."})
		return
	}
	switch flow.Kind {
	case "github-manifest":
		if err = s.Store.Sources.RegisterGitHub(r.Context(), r.URL.Query().Get("code")); err == nil {
			var next string
			next, err = s.newConnectionFlow(w, r, "github", "github-install", flow.Origin)
			if err == nil {
				var target string
				target, err = s.Store.Sources.GitHubInstallURL(next)
				if err == nil {
					http.Redirect(w, r, target, 303)
					return
				}
			}
		}
	case "github-install":
		id, parseErr := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
		if parseErr != nil {
			err = errors.New("GitHub installation was not completed; retry from connections")
		} else {
			err = s.Store.Sources.ConnectGitHub(r.Context(), id)
		}
	case "bitbucket-authorize":
		err = s.Store.Sources.ConnectBitbucket(r.Context(), r.URL.Query().Get("code"), flow.Origin+"/connections/bitbucket/callback")
	default:
		err = errors.New("invalid connection step")
	}
	if err != nil {
		s.connectionRender(w, r, page{View: "connected", Provider: provider, Error: err.Error()})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: s.flowCookie(provider), Path: "/connections/", HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: s.sessionCookie(), Value: flow.SessionID, Path: "/", HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteStrictMode, MaxAge: max(1, int(time.Until(session.Expires).Seconds()))})
	s.connectionRender(w, r, page{View: "connected", Provider: provider})
}
func (s *Server) disconnectProvider(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Sources.Disconnect(r.PathValue("provider")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/connections", 303)
}
func wizardProject() deploy.Project {
	return deploy.Project{Mode: "poll", Branch: "main", Dockerfile: "Dockerfile", Port: 8080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:8080/health", HealthTimeout: 60, PollSeconds: 60, Retain: 5, CITimeout: 1800}
}
func (s *Server) repositorySelection(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	status, err := s.Store.Sources.Status(provider)
	if err != nil || !status.Connected {
		http.Redirect(w, r, "/connections", 303)
		return
	}
	p := page{View: "selection", Provider: provider, Workspace: r.URL.Query().Get("workspace"), Wizard: true, Project: wizardProject()}
	if provider == "bitbucket" {
		p.Workspaces, err = s.Store.Sources.Workspaces(r.Context())
		if err != nil {
			p.Error = err.Error()
			s.connectionRender(w, r, p)
			return
		}
		if p.Workspace == "" && len(p.Workspaces) > 0 {
			p.Workspace = p.Workspaces[0].Slug
		}
	}
	if provider == "github" || p.Workspace != "" {
		p.Repositories, err = s.Store.Sources.Repositories(r.Context(), provider, p.Workspace)
		if err != nil {
			p.Error = err.Error()
			s.connectionRender(w, r, p)
			return
		}
	}
	if id := r.URL.Query().Get("repository"); id != "" {
		repo, err := s.Store.Sources.Repository(r.Context(), provider, id)
		if err != nil {
			p.Error = err.Error()
			s.connectionRender(w, r, p)
			return
		}
		p.Branches, err = s.Store.Sources.Branches(r.Context(), provider, id)
		if err != nil {
			p.Error = err.Error()
			s.connectionRender(w, r, p)
			return
		}
		p.Project.Repo = repo.URL
		p.Project.Connection = provider
		p.Project.RepositoryID = id
		p.Project.CIProvider = provider
		p.Project.Name = repo.FullName
		p.Project.Branch = repo.DefaultBranch
		if len(p.Branches) == 0 {
			p.Error = "This repository has no branches. Push its first commit and reload."
		}
	}
	s.connectionRender(w, r, p)
}
func (s *Server) sshAssistant(w http.ResponseWriter, r *http.Request) {
	p := page{View: "ssh"}
	if id := r.PathValue("id"); id != "" {
		key, err := s.Store.Sources.SSHKey(id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		p.SSH = key
		p.AccessKeyURL = accessKeyURL(key.Repo)
	}
	s.connectionRender(w, r, p)
}
func accessKeyURL(repo string) string {
	canonical, provider, err := scm.CanonicalRepo(repo)
	if err != nil {
		return ""
	}
	base := strings.TrimSuffix(canonical, ".git")
	if provider == "github" {
		return base + "/settings/keys"
	}
	return base + "/admin/access-keys/"
}
func (s *Server) sshGenerate(w http.ResponseWriter, r *http.Request) {
	canonical, _, err := scm.CanonicalRepo(r.FormValue("repo"))
	if err != nil {
		s.connectionRender(w, r, page{View: "ssh", Error: err.Error()})
		return
	}
	u, _ := url.Parse(canonical)
	repo := "git@" + u.Host + ":" + strings.TrimPrefix(u.Path, "/")
	key, err := s.Store.Sources.NewSSH(r.Context(), repo)
	if err != nil {
		s.connectionRender(w, r, page{View: "ssh", Error: err.Error()})
		return
	}
	http.Redirect(w, r, "/connections/ssh/keys/"+key.ID, 303)
}
func (s *Server) sshTest(w http.ResponseWriter, r *http.Request) {
	key, err := s.Store.Sources.SSHKey(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p := wizardProject()
	p.Repo = key.Repo
	p.SSHKey = key.PrivatePath
	p.KnownHosts = key.KnownHostsPath
	p.Name = strings.TrimSuffix(strings.TrimPrefix(key.Repo, "git@"), ".git")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	branches, err := (deploy.Git{}).Branches(ctx, p)
	view := page{View: "ssh", SSH: key, AccessKeyURL: accessKeyURL(key.Repo), Branches: branches, Project: p, Wizard: true}
	if err != nil {
		view.Error = err.Error()
	} else if len(branches) == 0 {
		view.Error = "The repository has no branches. Push its first commit and retry."
	} else {
		view.Project.Branch = branches[0]
		for _, branch := range branches {
			if branch == "main" {
				view.Project.Branch = branch
			}
		}
	}
	s.connectionRender(w, r, view)
}
