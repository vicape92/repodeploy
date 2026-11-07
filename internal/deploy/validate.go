package deploy

import (
	"errors"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// FieldError is a validation failure attributed to one project field, named
// as in the JSON API and the browser form, so the panel can highlight it.
type FieldError struct {
	Field   string
	Message string
}

func (e *FieldError) Error() string { return e.Message }

func fieldErr(field, message string) error { return &FieldError{Field: field, Message: message} }

// ErrorField returns the field a validation error refers to, if any.
func ErrorField(err error) string {
	var fe *FieldError
	if errors.As(err, &fe) {
		return fe.Field
	}
	return ""
}

func ValidateProject(p Project) error {
	if p.Connection != "" && p.Connection != "github" && p.Connection != "bitbucket" {
		return fieldErr("connection", "unsupported repository connection")
	}
	if p.Connection != "" && p.RepositoryID == "" {
		return fieldErr("repo", "select a repository from the connected provider")
	}
	if p.CITimeout < 0 || (p.CITimeout > 0 && p.CITimeout < 30) || p.CITimeout > 86400 {
		return fieldErr("ci_timeout", "CI/CD timeout must be 30–86400 seconds (zero selects 1800)")
	}
	if p.WaitCI && ((p.CIProvider != "github" && p.CIProvider != "bitbucket") || p.RepositoryID == "") {
		return fieldErr("wait_ci", "CI/CD requires browser authorization for the repository provider")
	}
	if strings.TrimSpace(p.Name) == "" || len(p.Name) > 100 {
		return fieldErr("name", "a project name of at most 100 characters is required")
	}
	if strings.TrimSpace(p.Repo) == "" || len(p.Repo) > 2048 {
		return fieldErr("repo", "a Git URL of at most 2048 characters is required")
	}
	if err := gitInputs(p); err != nil {
		return err
	}
	u, err := url.Parse(p.Repo)
	scp := strings.HasPrefix(p.Repo, "git@") && strings.Contains(p.Repo, ":")
	if !scp && (err != nil || (u.Scheme != "https" && u.Scheme != "ssh") || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || (u.User != nil && u.Scheme != "ssh")) {
		return fieldErr("repo", "use an HTTPS or SSH Git URL without embedded credentials")
	}
	if err := exec.Command("git", "check-ref-format", "--branch", p.Branch).Run(); err != nil {
		return fieldErr("branch", "invalid deployment branch")
	}
	if p.Mode != "manual" && p.Mode != "poll" && p.Mode != "webhook" && p.Mode != "both" {
		return fieldErr("mode", "mode must be manual, poll, webhook or both")
	}
	if (p.Mode == "webhook" || p.Mode == "both") && githubRepo(p.Repo) == "" {
		return fieldErr("mode", "webhook mode currently supports github.com repositories only")
	}
	if (p.Dockerfile == "") == (p.ComposeFile == "") {
		return fieldErr("build_type", "configure exactly one Dockerfile or Compose file")
	}
	for field, path := range map[string]string{"dockerfile": p.Dockerfile, "compose_file": p.ComposeFile} {
		if path != "" && (filepath.IsAbs(path) || filepath.Clean(path) == ".." || strings.HasPrefix(filepath.Clean(path), ".."+string(filepath.Separator))) {
			return fieldErr(field, "Docker configuration paths must stay inside the repository")
		}
	}
	if p.ComposeFile == "" && (p.Port < 1 || p.Port > 65535) {
		return fieldErr("port", "the host port must be between 1 and 65535")
	}
	if p.ComposeFile == "" && (p.ContainerPort < 1 || p.ContainerPort > 65535) {
		return fieldErr("container_port", "the container port must be between 1 and 65535")
	}
	if p.BindAddress != "" {
		ip := net.ParseIP(p.BindAddress)
		if ip == nil {
			return fieldErr("bind_address", "bind address must be an IP address such as 127.0.0.1 or 0.0.0.0")
		}
		if !ip.IsLoopback() && !ip.IsUnspecified() {
			return fieldErr("bind_address", "bind address must be a loopback address or 0.0.0.0/:: so the loopback health check can reach the application")
		}
	}
	if p.ComposeFile == "" && p.HealthURL == "" {
		return fieldErr("health_url", "Dockerfile mode requires an HTTP health URL")
	}
	if p.HealthURL == "" && p.HealthService == "" {
		return fieldErr("health_service", "an HTTP health URL or Compose health service is required")
	}
	if p.HealthURL != "" {
		u, err := url.Parse(p.HealthURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || (u.Hostname() != "localhost" && (net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback())) {
			return fieldErr("health_url", "health URL must use HTTP(S) on a loopback address")
		}
	}
	if p.HealthTimeout < 0 || p.HealthTimeout > 300 {
		return fieldErr("health_timeout", "health timeout must be 1–300 seconds (zero selects 60)")
	}
	if p.PollSeconds < 0 || (p.PollSeconds > 0 && p.PollSeconds < 10) || p.PollSeconds > 86400 {
		return fieldErr("poll_seconds", "polling interval must be 10–86400 seconds (zero selects 60)")
	}
	if p.Retain < 0 || p.Retain > 50 {
		return fieldErr("retain", "versions to retain must be 1–50 (zero selects 5)")
	}
	if p.SSHKey != "" {
		for field, path := range map[string]string{"ssh_key": p.SSHKey, "known_hosts": p.KnownHosts} {
			if !filepath.IsAbs(path) {
				return fieldErr(field, "SSH paths must be absolute")
			}
			stat, err := os.Stat(path)
			if err != nil || !stat.Mode().IsRegular() {
				return fieldErr(field, "SSH key and known-hosts file must exist and be regular files")
			}
		}
		stat, _ := os.Stat(p.SSHKey)
		if stat.Mode().Perm()&0077 != 0 {
			return fieldErr("ssh_key", "SSH private key must not be accessible to group or others")
		}
	}
	return nil
}
