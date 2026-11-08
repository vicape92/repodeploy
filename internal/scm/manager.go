package scm

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
	"regexp"
	"strings"
	"sync"
	"time"
)

var ErrDisconnected = errors.New("provider is not connected; reconnect from the repository connections page")

type Repository struct {
	ID            string `json:"id"`
	FullName      string `json:"full_name"`
	URL           string `json:"url"`
	DefaultBranch string `json:"default_branch"`
}
type Workspace struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}
type Credential struct {
	Username string
	Token    string
}
type Status struct {
	Provider   string `json:"provider"`
	Configured bool   `json:"configured"`
	Connected  bool   `json:"connected"`
}
type githubConfig struct {
	AppID          int64  `json:"app_id"`
	Slug           string `json:"slug"`
	PrivateKey     string `json:"private_key"`
	InstallationID int64  `json:"installation_id"`
}
type bitbucketConfig struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
}
type connections struct {
	GitHub    githubConfig    `json:"github"`
	Bitbucket bitbucketConfig `json:"bitbucket"`
}
type accessToken struct {
	Value   string
	Expires time.Time
}
type binding struct {
	repo    Repository
	expires time.Time
	version uint64
}

type Manager struct {
	root                            string
	mu                              sync.Mutex
	Client                          *http.Client
	githubAccess                    map[string]accessToken
	bitbucketAccess                 accessToken
	githubVersion, bitbucketVersion uint64
	githubGate, bitbucketGate       chan struct{}
	bindings                        map[string]binding
}

func New(root string) *Manager {
	return &Manager{root: root, Client: &http.Client{Timeout: 20 * time.Second}, githubAccess: make(map[string]accessToken), githubGate: make(chan struct{}, 1), bitbucketGate: make(chan struct{}, 1), bindings: make(map[string]binding)}
}
func (m *Manager) load() (connections, error) {
	var state connections
	b, err := os.ReadFile(filepath.Join(m.root, "connections.json"))
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, errors.New("cannot read provider credentials")
	}
	if err = json.Unmarshal(b, &state); err != nil {
		return state, errors.New("invalid provider credentials file")
	}
	return state, nil
}
func (m *Manager) save(state connections) error {
	if err := os.MkdirAll(m.root, 0700); err != nil {
		return err
	}
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(m.root, ".connections-")
	if err != nil {
		return errors.New("cannot save provider credentials")
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(m.root, "connections.json")); err != nil {
		return err
	}
	dir, err := os.Open(m.root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func providerOK(provider string) bool { return provider == "github" || provider == "bitbucket" }
func (m *Manager) Status(provider string) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := Status{Provider: provider}
	if !providerOK(provider) {
		return out, errors.New("unsupported provider")
	}
	state, err := m.load()
	if err != nil {
		return out, err
	}
	if provider == "github" {
		out.Configured = state.GitHub.AppID > 0
		out.Connected = state.GitHub.InstallationID > 0
	} else {
		out.Configured = state.Bitbucket.ClientID != "" && state.Bitbucket.ClientSecret != ""
		out.Connected = state.Bitbucket.RefreshToken != ""
	}
	return out, nil
}
func (m *Manager) Disconnect(provider string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.load()
	if err != nil {
		return err
	}
	switch provider {
	case "github":
		state.GitHub.InstallationID = 0
		m.githubAccess = make(map[string]accessToken)
		m.githubVersion++
		m.bindings = make(map[string]binding)
	case "bitbucket":
		state.Bitbucket.RefreshToken = ""
		m.bitbucketAccess = accessToken{}
		m.bitbucketVersion++
		m.bindings = make(map[string]binding)
	default:
		return errors.New("unsupported provider")
	}
	return m.save(state)
}
func (m *Manager) Repositories(ctx context.Context, provider, workspace string) ([]Repository, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch provider {
	case "github":
		return m.githubRepositories(ctx)
	case "bitbucket":
		return m.bitbucketRepositories(ctx, workspace)
	default:
		return nil, errors.New("unsupported provider")
	}
}
func (m *Manager) Repository(ctx context.Context, provider, id string) (Repository, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return m.repository(ctx, provider, id)
}

func (m *Manager) FindRepository(ctx context.Context, raw string) (Repository, string, error) {
	canonical, provider, err := CanonicalRepo(raw)
	if err != nil {
		return Repository{}, "", err
	}
	if provider == "bitbucket" {
		name := strings.TrimSuffix(strings.TrimPrefix(canonical, "https://bitbucket.org/"), ".git")
		repo, err := m.Repository(ctx, provider, name)
		return repo, provider, err
	}
	repos, err := m.Repositories(ctx, provider, "")
	if err != nil {
		return Repository{}, provider, err
	}
	for _, repo := range repos {
		if strings.EqualFold(repo.URL, canonical) {
			return repo, provider, nil
		}
	}
	return Repository{}, provider, errors.New("authorize this repository in the connected provider before enabling CI/CD")
}
func (m *Manager) repository(ctx context.Context, provider, id string) (Repository, error) {
	switch provider {
	case "github":
		return m.githubRepository(ctx, id)
	case "bitbucket":
		return m.bitbucketRepository(ctx, id)
	default:
		return Repository{}, errors.New("unsupported provider")
	}
}
func (m *Manager) Workspaces(ctx context.Context) ([]Workspace, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return m.bitbucketWorkspaces(ctx)
}
func (m *Manager) Branches(ctx context.Context, provider, id string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch provider {
	case "github":
		return m.githubBranches(ctx, id)
	case "bitbucket":
		return m.bitbucketBranches(ctx, id)
	default:
		return nil, errors.New("unsupported provider")
	}
}
func (m *Manager) Credential(ctx context.Context, provider, id, repoURL string) (Credential, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	canonical, urlProvider, err := CanonicalRepo(repoURL)
	if err != nil || urlProvider != provider {
		return Credential{}, errors.New("repository does not match the authorized selection")
	}
	key := provider + "\x00" + id
	m.mu.Lock()
	version := m.githubVersion
	if provider == "bitbucket" {
		version = m.bitbucketVersion
	}
	cached, ok := m.bindings[key]
	m.mu.Unlock()
	if !ok || cached.version != version || time.Now().After(cached.expires) {
		repo, err := m.repository(ctx, provider, id)
		if err != nil {
			return Credential{}, err
		}
		if canonical != repo.URL {
			return Credential{}, errors.New("repository does not match the authorized selection")
		}
		m.mu.Lock()
		current := m.githubVersion
		if provider == "bitbucket" {
			current = m.bitbucketVersion
		}
		if current != version {
			m.mu.Unlock()
			return Credential{}, ErrDisconnected
		}
		m.bindings[key] = binding{repo: repo, expires: time.Now().Add(time.Minute), version: version}
		m.mu.Unlock()
	} else if canonical != cached.repo.URL {
		return Credential{}, errors.New("repository does not match the authorized selection")
	}
	switch provider {
	case "github":
		token, err := m.githubToken(ctx, id)
		return Credential{Username: "x-access-token", Token: token}, err
	case "bitbucket":
		token, err := m.bitbucketToken(ctx)
		return Credential{Username: "x-token-auth", Token: token}, err
	}
	return Credential{}, ErrDisconnected
}
func (m *Manager) CIState(ctx context.Context, provider, id, sha string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !commitPattern.MatchString(sha) {
		return "", errors.New("invalid commit")
	}
	switch provider {
	case "github":
		return m.githubCI(ctx, id, sha)
	case "bitbucket":
		return m.bitbucketCI(ctx, id, sha)
	default:
		return "", errors.New("unsupported CI/CD provider")
	}
}

var commitPattern = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
var repoPartPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func repoParts(name string) (string, string, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 2 || !repoPartPattern.MatchString(parts[0]) || !repoPartPattern.MatchString(parts[1]) || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return "", "", errors.New("invalid repository name")
	}
	return parts[0], parts[1], nil
}
func CanonicalRepo(raw string) (string, string, error) {
	for _, host := range []string{"github.com", "bitbucket.org"} {
		prefix := "git@" + host + ":"
		if strings.HasPrefix(raw, prefix) {
			raw = "https://" + host + "/" + strings.TrimPrefix(raw, prefix)
		}
	}
	u, err := url.Parse(raw)
	if err == nil && u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			return "", "", errors.New("Git URLs must not contain passwords")
		}
	}
	if err != nil || (u.Scheme != "https" && u.Scheme != "ssh") || u.RawQuery != "" || u.Fragment != "" || u.Port() != "" || (u.User != nil && (u.Scheme != "ssh" || u.User.Username() != "git")) {
		return "", "", errors.New("use a GitHub or Bitbucket HTTPS/SSH repository URL")
	}
	provider := ""
	switch u.Hostname() {
	case "github.com":
		provider = "github"
	case "bitbucket.org":
		provider = "bitbucket"
	default:
		return "", "", errors.New("SSH assistant supports github.com and bitbucket.org")
	}
	name := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
	if _, _, err = repoParts(name); err != nil {
		return "", "", err
	}
	return "https://" + u.Hostname() + "/" + name + ".git", provider, nil
}

func (m *Manager) request(ctx context.Context, method, target, auth string, body io.Reader, contentType string, out any) (http.Header, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Host != "api.github.com" && u.Host != "api.bitbucket.org" && u.Host != "bitbucket.org") {
		return nil, errors.New("untrusted provider API URL")
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, errors.New("invalid provider request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "RepoDeploy")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	client := *m.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if client.Timeout == 0 {
		client.Timeout = 20 * time.Second
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("provider request failed; check network connectivity")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("provider returned HTTP %d; reconnect or check app permissions", response.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(b) > 4<<20 {
		return nil, errors.New("invalid provider response")
	}
	if out != nil {
		if err = json.Unmarshal(b, out); err != nil {
			return nil, errors.New("invalid provider JSON response")
		}
	}
	return response.Header, nil
}
func jsonBody(v any) io.Reader { b, _ := json.Marshal(v); return bytes.NewReader(b) }
