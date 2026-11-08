package scm

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (m *Manager) SaveBitbucket(clientID, secret string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return errors.New("Bitbucket client ID is required")
	}
	state, err := m.load()
	if err != nil {
		return err
	}
	if secret == "" && clientID == state.Bitbucket.ClientID {
		secret = state.Bitbucket.ClientSecret
	}
	if secret == "" {
		return errors.New("Bitbucket client secret is required")
	}
	if clientID != state.Bitbucket.ClientID {
		state.Bitbucket.RefreshToken = ""
	}
	state.Bitbucket.ClientID = clientID
	state.Bitbucket.ClientSecret = secret
	if err = m.save(state); err != nil {
		return err
	}
	m.bitbucketAccess = accessToken{}
	m.bitbucketVersion++
	m.bindings = make(map[string]binding)
	return nil
}

func (m *Manager) BitbucketAuthorizeURL(state, redirectURI string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	config, err := m.load()
	if err != nil {
		return "", err
	}
	if config.Bitbucket.ClientID == "" || config.Bitbucket.ClientSecret == "" {
		return "", ErrDisconnected
	}
	if state == "" || redirectURI == "" {
		return "", errors.New("Bitbucket authorization state and redirect URI are required")
	}
	query := url.Values{"client_id": {config.Bitbucket.ClientID}, "response_type": {"code"}, "state": {state}, "redirect_uri": {redirectURI}}
	return "https://bitbucket.org/site/oauth2/authorize?" + query.Encode(), nil
}

func (m *Manager) ConnectBitbucket(ctx context.Context, code, redirectURI string) error {
	select {
	case m.bitbucketGate <- struct{}{}:
		defer func() { <-m.bitbucketGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if code == "" || redirectURI == "" {
		return errors.New("Bitbucket authorization code and redirect URI are required")
	}
	m.mu.Lock()
	version := m.bitbucketVersion
	state, err := m.load()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = m.bitbucketExchange(ctx, state, version, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI}})
	return err
}

func (m *Manager) bitbucketToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	cached := m.bitbucketAccess
	m.mu.Unlock()
	if cached.Value != "" && time.Now().Add(30*time.Second).Before(cached.Expires) {
		return cached.Value, nil
	}
	select {
	case m.bitbucketGate <- struct{}{}:
		defer func() { <-m.bitbucketGate }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	m.mu.Lock()
	if m.bitbucketAccess.Value != "" && time.Now().Add(30*time.Second).Before(m.bitbucketAccess.Expires) {
		value := m.bitbucketAccess.Value
		m.mu.Unlock()
		return value, nil
	}
	state, err := m.load()
	version := m.bitbucketVersion
	m.mu.Unlock()
	if err != nil {
		return "", err
	}
	if state.Bitbucket.RefreshToken == "" {
		return "", ErrDisconnected
	}
	return m.bitbucketExchange(ctx, state, version, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {state.Bitbucket.RefreshToken}})
}

func (m *Manager) bitbucketExchange(ctx context.Context, state connections, version uint64, form url.Values) (string, error) {
	config := state.Bitbucket
	if config.ClientID == "" || config.ClientSecret == "" {
		return "", ErrDisconnected
	}
	var response struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scopes       string `json:"scopes"`
	}
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(config.ClientID+":"+config.ClientSecret))
	_, err := m.request(ctx, http.MethodPost, "https://bitbucket.org/site/oauth2/access_token", auth, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", &response)
	if err != nil {
		return "", err
	}
	scopes := make(map[string]bool)
	for _, scope := range strings.Fields(response.Scopes) {
		if scope != "account" && scope != "repository" {
			return "", errors.New("Bitbucket consumer must grant only account and repository read permissions")
		}
		scopes[scope] = true
	}
	if !scopes["account"] || !scopes["repository"] {
		return "", errors.New("Bitbucket consumer requires account and repository read permissions")
	}
	if response.AccessToken == "" || response.RefreshToken == "" || response.ExpiresIn <= 0 || response.ExpiresIn > int64((1<<63-1)/time.Second) {
		return "", errors.New("invalid Bitbucket authorization response")
	}
	expires := time.Now().Add(time.Duration(response.ExpiresIn) * time.Second)
	m.mu.Lock()
	defer m.mu.Unlock()
	if version != m.bitbucketVersion {
		return "", ErrDisconnected
	}
	state, err = m.load()
	if err != nil {
		return "", err
	}
	state.Bitbucket.RefreshToken = response.RefreshToken
	if err = m.save(state); err != nil {
		return "", err
	}
	if form.Get("grant_type") == "authorization_code" {
		m.bitbucketVersion++
		m.bindings = make(map[string]binding)
	}
	m.bitbucketAccess = accessToken{Value: response.AccessToken, Expires: expires}
	return response.AccessToken, nil
}

func bitbucketPages[T any](ctx context.Context, m *Manager, target, token string) ([]T, error) {
	values := make([]T, 0)
	seen := make(map[string]bool)
	for target != "" {
		u, err := url.Parse(target)
		if err != nil || u.Scheme != "https" || u.Host != "api.bitbucket.org" || u.User != nil || !strings.HasPrefix(u.Path, "/2.0/") || u.Fragment != "" {
			return nil, errors.New("untrusted Bitbucket pagination URL")
		}
		if seen[target] || len(seen) >= 100 {
			return nil, errors.New("Bitbucket pagination limit exceeded")
		}
		seen[target] = true
		var page struct {
			Values []T    `json:"values"`
			Next   string `json:"next"`
		}
		if _, err = m.request(ctx, http.MethodGet, target, "Bearer "+token, nil, "", &page); err != nil {
			return nil, err
		}
		values = append(values, page.Values...)
		target = page.Next
	}
	return values, nil
}

func (m *Manager) bitbucketWorkspaces(ctx context.Context) ([]Workspace, error) {
	token, err := m.bitbucketToken(ctx)
	if err != nil {
		return nil, err
	}
	values, err := bitbucketPages[struct {
		Workspace Workspace `json:"workspace"`
	}](ctx, m, "https://api.bitbucket.org/2.0/user/workspaces?pagelen=100", token)
	if err != nil {
		return nil, err
	}
	workspaces := make([]Workspace, 0, len(values))
	for _, value := range values {
		workspaces = append(workspaces, value.Workspace)
	}
	return workspaces, nil
}

type bitbucketRepo struct {
	FullName   string `json:"full_name"`
	MainBranch struct {
		Name string `json:"name"`
	} `json:"mainbranch"`
}

func (repo bitbucketRepo) repository() (Repository, error) {
	if _, _, err := repoParts(repo.FullName); err != nil {
		return Repository{}, err
	}
	return Repository{ID: repo.FullName, FullName: repo.FullName, URL: "https://bitbucket.org/" + repo.FullName + ".git", DefaultBranch: repo.MainBranch.Name}, nil
}

func (m *Manager) bitbucketRepositories(ctx context.Context, workspace string) ([]Repository, error) {
	if _, _, err := repoParts(workspace + "/repository"); err != nil {
		return nil, errors.New("invalid Bitbucket workspace")
	}
	token, err := m.bitbucketToken(ctx)
	if err != nil {
		return nil, err
	}
	values, err := bitbucketPages[bitbucketRepo](ctx, m, "https://api.bitbucket.org/2.0/repositories/"+workspace+"?role=member&pagelen=100", token)
	if err != nil {
		return nil, err
	}
	repos := make([]Repository, 0, len(values))
	for _, value := range values {
		repo, err := value.repository()
		if err != nil {
			return nil, err
		}
		repos = append(repos, repo)
	}
	return repos, nil
}

func (m *Manager) bitbucketRepository(ctx context.Context, id string) (Repository, error) {
	if _, _, err := repoParts(id); err != nil {
		return Repository{}, err
	}
	token, err := m.bitbucketToken(ctx)
	if err != nil {
		return Repository{}, err
	}
	var value bitbucketRepo
	if _, err = m.request(ctx, http.MethodGet, "https://api.bitbucket.org/2.0/repositories/"+id, "Bearer "+token, nil, "", &value); err != nil {
		return Repository{}, err
	}
	if value.FullName != id {
		return Repository{}, errors.New("Bitbucket repository does not match the authorized selection")
	}
	return value.repository()
}

func (m *Manager) bitbucketBranches(ctx context.Context, id string) ([]string, error) {
	if _, _, err := repoParts(id); err != nil {
		return nil, err
	}
	token, err := m.bitbucketToken(ctx)
	if err != nil {
		return nil, err
	}
	values, err := bitbucketPages[struct {
		Name string `json:"name"`
	}](ctx, m, "https://api.bitbucket.org/2.0/repositories/"+id+"/refs/branches?pagelen=100", token)
	if err != nil {
		return nil, err
	}
	branches := make([]string, 0, len(values))
	for _, value := range values {
		branches = append(branches, value.Name)
	}
	return branches, nil
}

func (m *Manager) bitbucketCI(ctx context.Context, id, sha string) (string, error) {
	if _, _, err := repoParts(id); err != nil {
		return "", err
	}
	token, err := m.bitbucketToken(ctx)
	if err != nil {
		return "", err
	}
	values, err := bitbucketPages[struct {
		State string `json:"state"`
	}](ctx, m, "https://api.bitbucket.org/2.0/repositories/"+id+"/commit/"+url.PathEscape(sha)+"/statuses?pagelen=100", token)
	if err != nil {
		return "", err
	}
	passed := len(values) > 0
	for _, value := range values {
		switch value.State {
		case "FAILED", "STOPPED":
			return "failed", nil
		case "SUCCESSFUL":
		default:
			passed = false
		}
	}
	if passed {
		return "passed", nil
	}
	return "pending", nil
}
