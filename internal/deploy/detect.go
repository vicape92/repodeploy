package deploy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
)

var ErrSignature = errors.New("invalid webhook signature")

func githubRepo(raw string) string {
	raw = strings.TrimSuffix(raw, ".git")
	if strings.HasPrefix(raw, "git@github.com:") {
		return strings.TrimPrefix(raw, "git@github.com:")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "github.com" {
		return ""
	}
	return strings.Trim(u.Path, "/")
}

func AcceptGitHub(s *Store, project, secret, event, delivery, signature string, body []byte) (Job, error) {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	digest, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if secret == "" || !strings.HasPrefix(signature, "sha256=") || err != nil || !hmac.Equal(digest, mac.Sum(nil)) {
		return Job{}, ErrSignature
	}
	p, err := s.Project(project)
	if err != nil {
		return Job{}, err
	}
	if p.Mode != "webhook" && p.Mode != "both" {
		return Job{}, nil
	}
	if event != "push" {
		return Job{}, nil
	}
	if delivery == "" || len(delivery) > 200 {
		return Job{}, errors.New("missing or invalid delivery ID")
	}
	var payload struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Deleted    bool   `json:"deleted"`
		Repository struct {
			FullName string `json:"full_name"`
			HTMLURL  string `json:"html_url"`
		} `json:"repository"`
	}
	if err = json.Unmarshal(body, &payload); err != nil {
		return Job{}, errors.New("invalid webhook JSON")
	}
	expected := githubRepo(p.Repo)
	if payload.Deleted || payload.Ref != "refs/heads/"+p.Branch || expected == "" || payload.Repository.FullName != expected || githubRepo(payload.Repository.HTMLURL) != expected {
		return Job{}, nil
	}
	if !shaPattern.MatchString(payload.After) || strings.Trim(payload.After, "0") == "" {
		return Job{}, errors.New("invalid push commit")
	}
	return s.Enqueue(p.ID, "github", delivery, payload.After, "")
}

type pollSchedule struct {
	next     time.Time
	failures int
}
type Poller struct {
	store    *Store
	schedule map[string]pollSchedule
}

func NewPoller(s *Store) *Poller { return &Poller{store: s, schedule: make(map[string]pollSchedule)} }

func (p *Poller) Tick(ctx context.Context, at time.Time) {
	projects, err := p.store.Projects()
	if err != nil {
		return
	}
	for _, project := range projects {
		if ctx.Err() != nil {
			return
		}
		if project.Paused || project.Blocked || (project.Mode != "poll" && project.Mode != "both") {
			continue
		}
		state := p.schedule[project.ID]
		if at.Before(state.next) {
			continue
		}
		interval := time.Duration(project.PollSeconds) * time.Second
		if interval <= 0 {
			interval = time.Minute
		}
		pollCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		sha, err := (Git{Sources: p.store.Sources}).Resolve(pollCtx, project)
		cancel()
		if err == nil {
			_, err = p.store.Enqueue(project.ID, "poll", "", sha, "")
		}
		if err != nil {
			state.failures++
			backoff := interval * time.Duration(1<<min(state.failures, 6))
			if backoff > 15*time.Minute {
				backoff = 15 * time.Minute
			}
			state.next = at.Add(backoff)
			message := "Repository polling failed: " + err.Error()
			if len(message) > 600 {
				message = message[:600] + "…"
			}
			p.store.PollError(project.ID, message)
		} else {
			state.failures = 0
			state.next = at.Add(interval)
			p.store.PollError(project.ID, "")
		}
		p.schedule[project.ID] = state
	}
}

// UnusedSSHKeyAge is how long a generated SSH key may stay unused by any
// project before the hourly sweep deletes it.
const UnusedSSHKeyAge = 24 * time.Hour

func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	sweep := time.NewTicker(time.Hour)
	defer sweep.Stop()
	p.store.PruneSSHKeys(UnusedSSHKeyAge, time.Now())
	p.Tick(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case at := <-ticker.C:
			p.Tick(ctx, at)
		case at := <-sweep.C:
			p.store.PruneSSHKeys(UnusedSSHKeyAge, at)
		}
	}
}
