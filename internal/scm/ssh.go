package scm

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type SSHKey struct {
	ID             string `json:"id"`
	Repo           string `json:"repo"`
	PrivatePath    string `json:"private_path"`
	KnownHostsPath string `json:"known_hosts_path"`
	PublicKey      string `json:"public_key"`
	Created        string `json:"created,omitempty"`
}

var sshKeyIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func (m *Manager) NewSSH(ctx context.Context, repo string) (SSHKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, provider, err := CanonicalRepo(repo)
	if err != nil {
		return SSHKey{}, err
	}
	knownHosts, err := m.sshKnownHosts(ctx, provider)
	if err != nil {
		return SSHKey{}, err
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return SSHKey{}, err
	}
	privateBlock, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		return SSHKey{}, err
	}
	publicKey, err := ssh.NewPublicKey(public)
	if err != nil {
		return SSHKey{}, err
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return SSHKey{}, err
	}
	id := hex.EncodeToString(idBytes)
	parent := filepath.Join(m.root, "ssh")
	if err := os.MkdirAll(parent, 0700); err != nil {
		return SSHKey{}, err
	}
	if err := os.Chmod(parent, 0700); err != nil {
		return SSHKey{}, err
	}
	dir := filepath.Join(parent, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		return SSHKey{}, err
	}
	complete := false
	defer func() {
		if !complete {
			os.RemoveAll(dir)
		}
	}()
	key := SSHKey{
		ID:             id,
		Repo:           repo,
		PrivatePath:    filepath.Join(dir, "key"),
		KnownHostsPath: filepath.Join(dir, "known_hosts"),
		PublicKey:      strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey))),
		Created:        time.Now().UTC().Format(time.RFC3339),
	}
	metadata, err := json.Marshal(key)
	if err != nil {
		return SSHKey{}, err
	}
	for _, file := range []struct {
		path string
		data []byte
	}{
		{key.PrivatePath, pem.EncodeToMemory(privateBlock)},
		{key.KnownHostsPath, []byte(knownHosts)},
		{filepath.Join(dir, "metadata.json"), metadata},
	} {
		if err := os.WriteFile(file.path, file.data, 0600); err != nil {
			return SSHKey{}, err
		}
	}
	complete = true
	return key, nil
}

func (m *Manager) SSHKey(id string) (SSHKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sshKey(id)
}

// SSHKeys lists generated keys, oldest first. Keys created before creation
// times were recorded fall back to their directory modification time.
func (m *Manager) SSHKeys() ([]SSHKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(m.root, "ssh"))
	if os.IsNotExist(err) {
		return []SSHKey{}, nil
	}
	if err != nil {
		return nil, errors.New("cannot list SSH keys")
	}
	keys := []SSHKey{}
	for _, entry := range entries {
		if !entry.IsDir() || !sshKeyIDPattern.MatchString(entry.Name()) {
			continue
		}
		key, err := m.sshKey(entry.Name())
		if err != nil {
			continue
		}
		if key.Created == "" {
			if info, err := entry.Info(); err == nil {
				key.Created = info.ModTime().UTC().Format(time.RFC3339)
			}
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Created < keys[j].Created })
	return keys, nil
}

// DeleteSSH removes a generated key pair. Callers must check it is unused.
func (m *Manager) DeleteSSH(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !sshKeyIDPattern.MatchString(id) {
		return errors.New("invalid SSH key ID")
	}
	dir := filepath.Join(m.root, "ssh", id)
	if _, err := os.Stat(filepath.Join(dir, "metadata.json")); err != nil {
		return errors.New("SSH key not found")
	}
	return os.RemoveAll(dir)
}

func (m *Manager) sshKey(id string) (SSHKey, error) {
	if !sshKeyIDPattern.MatchString(id) {
		return SSHKey{}, errors.New("invalid SSH key ID")
	}
	dir := filepath.Join(m.root, "ssh", id)
	data, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		return SSHKey{}, errors.New("cannot read SSH key metadata")
	}
	var key SSHKey
	if err := json.Unmarshal(data, &key); err != nil || key.ID != id {
		return SSHKey{}, errors.New("invalid SSH key metadata")
	}
	if _, _, err := CanonicalRepo(key.Repo); err != nil {
		return SSHKey{}, errors.New("invalid SSH key repository")
	}
	key.PrivatePath = filepath.Join(dir, "key")
	key.KnownHostsPath = filepath.Join(dir, "known_hosts")
	return key, nil
}

func (m *Manager) sshKnownHosts(ctx context.Context, provider string) (string, error) {
	host := "github.com"
	var keys []string
	if provider == "github" {
		var metadata struct {
			Keys []string `json:"ssh_keys"`
		}
		if _, err := m.request(ctx, http.MethodGet, "https://api.github.com/meta", "", nil, "", &metadata); err != nil {
			return "", err
		}
		keys = metadata.Keys
	} else {
		host = "bitbucket.org"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://bitbucket.org/site/ssh", nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", "RepoDeploy")
		client := *m.Client
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		if client.Timeout == 0 {
			client.Timeout = 20 * time.Second
		}
		response, err := client.Do(req)
		if err != nil {
			return "", errors.New("SSH host key request failed; check network connectivity")
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return "", fmt.Errorf("SSH host key source returned HTTP %d", response.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
		if err != nil || len(body) > 4<<20 {
			return "", errors.New("invalid SSH host key response")
		}
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) >= 3 && fields[0] == "ssh.bitbucket.org" {
				continue
			}
			if len(fields) < 3 || fields[0] != host {
				return "", errors.New("invalid SSH host key hostname")
			}
			keys = append(keys, strings.TrimSpace(strings.TrimPrefix(line, host)))
		}
	}
	if len(keys) == 0 {
		return "", errors.New("SSH host key source returned no keys")
	}
	var knownHosts strings.Builder
	for _, raw := range keys {
		key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(raw))
		if err != nil || len(options) != 0 || len(strings.TrimSpace(string(rest))) != 0 {
			return "", errors.New("invalid SSH host key")
		}
		knownHosts.WriteString(host + " " + string(ssh.MarshalAuthorizedKey(key)))
	}
	return knownHosts.String(), nil
}
