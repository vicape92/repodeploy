package scm

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func (m *Manager) RegisterGitHub(ctx context.Context, code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if code == "" || len(code) > 200 {
		return errors.New("invalid GitHub registration code")
	}
	var registration struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
		PEM  string `json:"pem"`
	}
	if _, err := m.request(ctx, "POST", "https://api.github.com/app-manifests/"+url.PathEscape(code)+"/conversions", "", nil, "", &registration); err != nil {
		return err
	}
	if registration.ID <= 0 || !regexp.MustCompile(`^[a-zA-Z0-9-]+$`).MatchString(registration.Slug) {
		return errors.New("invalid GitHub App registration")
	}
	if _, err := parseRSA(registration.PEM); err != nil {
		return err
	}
	state, err := m.load()
	if err != nil {
		return err
	}
	if state.GitHub.AppID > 0 {
		return errors.New("a GitHub App is already configured")
	}
	state.GitHub = githubConfig{AppID: registration.ID, Slug: registration.Slug, PrivateKey: registration.PEM}
	if err = m.save(state); err != nil {
		return err
	}
	m.githubAccess = make(map[string]accessToken)
	return nil
}
func parseRSA(encoded string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(encoded))
	if block == nil {
		return nil, errors.New("invalid GitHub App private key")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		parsed, parseErr := x509.ParsePKCS8PrivateKey(block.Bytes)
		if parseErr != nil {
			return nil, errors.New("invalid GitHub App private key")
		}
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("GitHub App requires an RSA key")
		}
	}
	if key.N.BitLen() < 2048 {
		return nil, errors.New("GitHub App RSA key must be at least 2048 bits")
	}
	return key, nil
}
func appJWT(config githubConfig) (string, error) {
	key, err := parseRSA(config.PrivateKey)
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iss": strconv.FormatInt(config.AppID, 10), "iat": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(9 * time.Minute).Unix()})
	message := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(message))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return message + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
func (m *Manager) GitHubInstallURL(state string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	config, err := m.load()
	if err != nil {
		return "", err
	}
	if config.GitHub.AppID <= 0 {
		return "", ErrDisconnected
	}
	return "https://github.com/apps/" + config.GitHub.Slug + "/installations/new?" + url.Values{"state": {state}}.Encode(), nil
}
func (m *Manager) ConnectGitHub(ctx context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.load()
	if err != nil {
		return err
	}
	if state.GitHub.AppID <= 0 || id <= 0 {
		return ErrDisconnected
	}
	jwt, err := appJWT(state.GitHub)
	if err != nil {
		return err
	}
	var installation struct {
		ID          int64             `json:"id"`
		AppID       int64             `json:"app_id"`
		SuspendedAt *string           `json:"suspended_at"`
		Permissions map[string]string `json:"permissions"`
	}
	if _, err = m.request(ctx, "GET", fmt.Sprintf("https://api.github.com/app/installations/%d", id), "Bearer "+jwt, nil, "", &installation); err != nil {
		return err
	}
	if installation.ID != id || installation.AppID != state.GitHub.AppID || installation.SuspendedAt != nil || installation.Permissions["contents"] != "read" {
		return errors.New("installation is not an active read-only installation of this GitHub App")
	}
	for permission, value := range installation.Permissions {
		if value != "read" || (permission != "contents" && permission != "metadata" && permission != "checks" && permission != "statuses") {
			return errors.New("GitHub App must request only read access to contents, metadata, checks and statuses")
		}
	}
	state.GitHub.InstallationID = id
	if err = m.save(state); err != nil {
		return err
	}
	m.githubAccess = make(map[string]accessToken)
	return nil
}
func (m *Manager) githubToken(ctx context.Context, repoID string) (string, error) {
	state, err := m.load()
	if err != nil {
		return "", err
	}
	if state.GitHub.InstallationID <= 0 {
		return "", ErrDisconnected
	}
	if cached := m.githubAccess[repoID]; cached.Value != "" && cached.Expires.After(time.Now().Add(time.Minute)) {
		return cached.Value, nil
	}
	request := map[string]any{"permissions": map[string]string{"contents": "read", "checks": "read", "statuses": "read"}}
	if repoID != "" {
		id, err := strconv.ParseInt(repoID, 10, 64)
		if err != nil || id <= 0 {
			return "", errors.New("invalid GitHub repository ID")
		}
		request["repository_ids"] = []int64{id}
	}
	jwt, err := appJWT(state.GitHub)
	if err != nil {
		return "", err
	}
	var token struct {
		Value   string    `json:"token"`
		Expires time.Time `json:"expires_at"`
	}
	if _, err = m.request(ctx, "POST", fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", state.GitHub.InstallationID), "Bearer "+jwt, jsonBody(request), "application/json", &token); err != nil {
		return "", err
	}
	if token.Value == "" || !token.Expires.After(time.Now()) {
		return "", errors.New("invalid GitHub installation token")
	}
	m.githubAccess[repoID] = accessToken{Value: token.Value, Expires: token.Expires}
	return token.Value, nil
}

type githubRepo struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

func (r githubRepo) repository() (Repository, error) {
	if r.ID <= 0 {
		return Repository{}, errors.New("invalid GitHub repository")
	}
	if _, _, err := repoParts(r.FullName); err != nil {
		return Repository{}, err
	}
	return Repository{ID: strconv.FormatInt(r.ID, 10), FullName: r.FullName, URL: "https://github.com/" + r.FullName + ".git", DefaultBranch: r.DefaultBranch}, nil
}
func githubNext(headers http.Header) string {
	for _, part := range strings.Split(headers.Get("Link"), ",") {
		if strings.Contains(part, `rel="next"`) {
			left := strings.Index(part, "<")
			right := strings.Index(part, ">")
			if left >= 0 && right > left {
				return part[left+1 : right]
			}
		}
	}
	return ""
}
func githubPageAllowed(target string, seen map[string]bool) error {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Host != "api.github.com" || u.User != nil || u.Fragment != "" {
		return errors.New("untrusted GitHub pagination URL")
	}
	if seen[target] || len(seen) >= 100 {
		return errors.New("GitHub pagination limit exceeded")
	}
	seen[target] = true
	return nil
}
func (m *Manager) githubRepositories(ctx context.Context) ([]Repository, error) {
	token, err := m.githubToken(ctx, "")
	if err != nil {
		return nil, err
	}
	target := "https://api.github.com/installation/repositories?per_page=100"
	seen := map[string]bool{}
	repos := []Repository{}
	for target != "" {
		if err = githubPageAllowed(target, seen); err != nil {
			return nil, err
		}
		var page struct {
			Repositories []githubRepo `json:"repositories"`
		}
		headers, err := m.request(ctx, "GET", target, "Bearer "+token, nil, "", &page)
		if err != nil {
			return nil, err
		}
		for _, v := range page.Repositories {
			repo, err := v.repository()
			if err != nil {
				return nil, err
			}
			repos = append(repos, repo)
		}
		target = githubNext(headers)
	}
	return repos, nil
}
func (m *Manager) githubRepository(ctx context.Context, id string) (Repository, error) {
	token, err := m.githubToken(ctx, id)
	if err != nil {
		return Repository{}, err
	}
	var repo githubRepo
	if _, err = m.request(ctx, "GET", "https://api.github.com/repositories/"+id, "Bearer "+token, nil, "", &repo); err != nil {
		return Repository{}, err
	}
	value, err := repo.repository()
	if err != nil || value.ID != id {
		return Repository{}, errors.New("GitHub repository does not match the authorized selection")
	}
	return value, nil
}
func (m *Manager) githubBranches(ctx context.Context, id string) ([]string, error) {
	repo, err := m.githubRepository(ctx, id)
	if err != nil {
		return nil, err
	}
	token, err := m.githubToken(ctx, id)
	if err != nil {
		return nil, err
	}
	target := "https://api.github.com/repos/" + repo.FullName + "/branches?per_page=100"
	seen := map[string]bool{}
	branches := []string{}
	for target != "" {
		if err = githubPageAllowed(target, seen); err != nil {
			return nil, err
		}
		var page []struct {
			Name string `json:"name"`
		}
		headers, err := m.request(ctx, "GET", target, "Bearer "+token, nil, "", &page)
		if err != nil {
			return nil, err
		}
		for _, branch := range page {
			branches = append(branches, branch.Name)
		}
		target = githubNext(headers)
	}
	return branches, nil
}
func (m *Manager) githubCI(ctx context.Context, id, sha string) (string, error) {
	repo, err := m.githubRepository(ctx, id)
	if err != nil {
		return "", err
	}
	token, err := m.githubToken(ctx, id)
	if err != nil {
		return "", err
	}
	base := "https://api.github.com/repos/" + repo.FullName + "/commits/" + sha
	var status struct {
		State string `json:"state"`
		Total int    `json:"total_count"`
	}
	if _, err = m.request(ctx, "GET", base+"/status", "Bearer "+token, nil, "", &status); err != nil {
		return "", err
	}
	if status.State == "failure" || status.State == "error" {
		return "failed", nil
	}
	total := status.Total
	pending := total > 0 && status.State != "success"
	target := base + "/check-runs?per_page=100&filter=latest"
	seen := map[string]bool{}
	for target != "" {
		if err = githubPageAllowed(target, seen); err != nil {
			return "", err
		}
		var checks struct {
			Runs []struct {
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"check_runs"`
		}
		headers, err := m.request(ctx, "GET", target, "Bearer "+token, nil, "", &checks)
		if err != nil {
			return "", err
		}
		for _, check := range checks.Runs {
			total++
			if check.Status != "completed" {
				pending = true
				continue
			}
			switch check.Conclusion {
			case "success", "neutral", "skipped":
			case "failure", "cancelled", "timed_out", "action_required", "startup_failure", "stale":
				return "failed", nil
			default:
				pending = true
			}
		}
		target = githubNext(headers)
	}
	if total == 0 || pending {
		return "pending", nil
	}
	return "passed", nil
}
