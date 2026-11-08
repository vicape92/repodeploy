package deploy

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/vicape92/repodeploy/internal/scm"
)

type Git struct{ Sources *scm.Manager }

var shaPattern = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)

func (g Git) Resolve(ctx context.Context, p Project) (string, error) {
	if err := gitInputs(p); err != nil {
		return "", err
	}
	credential, err := g.credential(ctx, p)
	if err != nil {
		return "", err
	}
	b, err := gitRunCredential(ctx, p, "", credential, "ls-remote", "--exit-code", p.Repo, "refs/heads/"+p.Branch)
	if err != nil {
		return "", fmt.Errorf("Git branch lookup failed; check URL, branch, credentials and known hosts%s", gitDetail(err))
	}
	fields := strings.Fields(string(b))
	if len(fields) != 2 || !shaPattern.MatchString(fields[0]) {
		return "", errors.New("Git returned an invalid branch commit")
	}
	return fields[0], nil
}

func (g Git) Prepare(ctx context.Context, p Project, sha, dest string) error {
	if err := gitInputs(p); err != nil {
		return err
	}
	if !shaPattern.MatchString(sha) {
		return errors.New("invalid commit SHA")
	}
	credential, err := g.credential(ctx, p)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0700); err != nil {
		return err
	}
	steps := [][]string{{"init", "--quiet"}, {"remote", "add", "origin", p.Repo}, {"fetch", "--quiet", "--no-tags", "origin", "refs/heads/" + p.Branch}, {"cat-file", "-e", sha + "^{commit}"}, {"checkout", "--quiet", "--detach", sha}}
	for _, args := range steps {
		if _, err := gitRunCredential(ctx, p, dest, credential, args...); err != nil {
			return fmt.Errorf("Git preparation failed at %s; check commit availability and repository access%s", args[0], gitDetail(err))
		}
	}
	return nil
}

func (g Git) credential(ctx context.Context, p Project) (scm.Credential, error) {
	if p.Connection == "" || p.SSHKey != "" {
		return scm.Credential{}, nil
	}
	if g.Sources == nil {
		return scm.Credential{}, scm.ErrDisconnected
	}
	return g.Sources.Credential(ctx, p.Connection, p.RepositoryID, p.Repo)
}
func (g Git) Branches(ctx context.Context, p Project) ([]string, error) {
	if p.Branch == "" {
		p.Branch = "main"
	}
	if err := gitInputs(p); err != nil {
		return nil, err
	}
	credential, err := g.credential(ctx, p)
	if err != nil {
		return nil, err
	}
	b, err := gitRunCredential(ctx, p, "", credential, "ls-remote", "--heads", p.Repo)
	if err != nil {
		return nil, fmt.Errorf("repository access failed; register the displayed public key as a repository access key and retry%s", gitDetail(err))
	}
	branches := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.HasPrefix(fields[1], "refs/heads/") {
			branches = append(branches, strings.TrimPrefix(fields[1], "refs/heads/"))
		}
	}
	sort.Strings(branches)
	return branches, nil
}

func gitInputs(p Project) error {
	if strings.HasPrefix(p.Repo, "ssh://") {
		u, err := url.Parse(p.Repo)
		if err != nil {
			return fieldErr("repo", "invalid SSH repository URL")
		}
		if u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword {
				return fieldErr("repo", "Git URLs must not contain passwords")
			}
		}
	}
	if strings.HasPrefix(p.Repo, "-") || strings.HasPrefix(p.Repo, "ext:") || strings.ContainsAny(p.Repo, "\r\n\x00") {
		return fieldErr("repo", "unsupported Git transport")
	}
	for _, prefix := range []string{"http://", "https://"} {
		if strings.HasPrefix(p.Repo, prefix) && strings.Contains(strings.TrimPrefix(p.Repo, prefix), "@") {
			return fieldErr("repo", "Git URLs must not contain credentials")
		}
	}
	if p.Branch == "" || strings.HasPrefix(p.Branch, "-") || strings.ContainsAny(p.Branch, "\r\n\x00 :?*[\\~^") || strings.Contains(p.Branch, "..") || strings.Contains(p.Branch, "@{") {
		return fieldErr("branch", "invalid deployment branch")
	}
	if (p.SSHKey == "") != (p.KnownHosts == "") {
		return fieldErr("ssh_key", "SSH key and known-hosts file must be configured together")
	}
	return nil
}

// gitAllowProtocol restricts Git transports. Project validation only accepts
// HTTPS and SSH URLs; tests widen this to use local fixture repositories.
var gitAllowProtocol = "https:ssh"

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func gitRun(ctx context.Context, p Project, dir string, args ...string) ([]byte, error) {
	return gitRunCredential(ctx, p, dir, scm.Credential{}, args...)
}
func gitRunCredential(ctx context.Context, p Project, dir string, credential scm.Credential, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") && !strings.HasPrefix(entry, "SSH_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_ALLOW_PROTOCOL="+gitAllowProtocol, "GIT_SSH_VARIANT=ssh")
	if credential.Token != "" {
		header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(credential.Username+":"+credential.Token))
		cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=http."+p.Repo+".extraHeader", "GIT_CONFIG_VALUE_0="+header, "GIT_CONFIG_KEY_1=http.followRedirects", "GIT_CONFIG_VALUE_1=false")
	}
	ssh := "ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o IdentityAgent=none -o IdentitiesOnly=yes -o ConnectTimeout=10"
	if p.SSHKey != "" {
		ssh += " -i " + shellQuote(p.SSHKey) + " -o UserKnownHostsFile=" + shellQuote(p.KnownHosts)
	}
	cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND="+ssh)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, &gitError{err: err, detail: sanitizeGitOutput(stderr.String(), credential.Token)}
	}
	return out, nil
}

// gitError carries Git's diagnostic output so failures are actionable
// ("Permission denied (publickey)", "Host key verification failed", ...).
type gitError struct {
	err    error
	detail string
}

func (e *gitError) Error() string {
	if e.detail == "" {
		return e.err.Error()
	}
	return e.detail
}
func (e *gitError) Unwrap() error { return e.err }

// gitDetail formats Git's diagnostics as a suffix for a user-facing message.
func gitDetail(err error) string {
	var ge *gitError
	if errors.As(err, &ge) && ge.detail != "" {
		return " (git: " + ge.detail + ")"
	}
	return ""
}

var urlCredentials = regexp.MustCompile(`://[^/\s@]+@`)

// sanitizeGitOutput keeps the last few diagnostic lines, strips control
// characters and removes anything resembling credentials.
func sanitizeGitOutput(raw, token string) string {
	if token != "" {
		raw = strings.ReplaceAll(raw, token, "[REDACTED]")
	}
	raw = urlCredentials.ReplaceAllString(raw, "://[REDACTED]@")
	var lines []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, line)
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	detail := strings.Join(lines, " | ")
	if len(detail) > 500 {
		detail = detail[:500] + "…"
	}
	return detail
}
