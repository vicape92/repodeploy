package deploy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

func signed(body []byte, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(body)
	return "sha256=" + hex.EncodeToString(h.Sum(nil))
}

func TestWebhookChecksSignatureRepositoryBranchAndDeduplicates(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	p.Repo = "git@github.com:team/app.git"
	p.Mode = "webhook"
	p, _ = s.SaveProject(p)
	body := []byte(`{"ref":"refs/heads/main","after":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repository":{"full_name":"team/app","html_url":"https://github.com/team/app"}}`)
	if _, err := AcceptGitHub(s, p.ID, "secret", "push", "delivery", "sha256=bad", body); !errors.Is(err, ErrSignature) {
		t.Fatalf("invalid signature: %v", err)
	}
	for _, bad := range []string{
		`{"ref":"refs/heads/other","after":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repository":{"full_name":"team/app","html_url":"https://github.com/team/app"}}`,
		`{"ref":"refs/heads/main","after":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repository":{"full_name":"other/app","html_url":"https://github.com/other/app"}}`,
		`{"ref":"refs/heads/main","deleted":true,"after":"0000000000000000000000000000000000000000","repository":{"full_name":"team/app","html_url":"https://github.com/team/app"}}`,
	} {
		b := []byte(bad)
		j, err := AcceptGitHub(s, p.ID, "secret", "push", "ignored", signed(b, "secret"), b)
		if err != nil || j.ID != "" {
			t.Fatalf("event must be ignored: %+v %v", j, err)
		}
	}
	first, err := AcceptGitHub(s, p.ID, "secret", "push", "delivery", signed(body, "secret"), body)
	if err != nil {
		t.Fatal(err)
	}
	again, err := AcceptGitHub(s, p.ID, "secret", "push", "delivery", signed(body, "secret"), body)
	if err != nil || again.ID != first.ID {
		t.Fatalf("duplicate webhook: %v %v", again, err)
	}
	jobs, _ := s.Jobs(p.ID)
	if len(jobs) != 1 || jobs[0].Phase != "queued" {
		t.Fatalf("webhook executed synchronously: %v", jobs)
	}
}

func TestPollingQueuesOnlyNewObservedCommitsAndBacksOffOnFailure(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	repo, _ := sourceRepo(t)
	p.Repo = repo
	p.Mode = "poll"
	p.PollSeconds = 10
	p, _ = s.SaveProject(p)
	poller := NewPoller(s)
	start := time.Now()
	poller.Tick(context.Background(), start)
	jobs, _ := s.Jobs(p.ID)
	if len(jobs) != 1 {
		t.Fatalf("initial poll: %v", jobs)
	}
	poller.Tick(context.Background(), start.Add(10*time.Second))
	jobs, _ = s.Jobs(p.ID)
	if len(jobs) != 1 {
		t.Fatal("unchanged commit enqueued again")
	}
	secondCommit(t, repo)
	poller.Tick(context.Background(), start.Add(20*time.Second))
	jobs, _ = s.Jobs(p.ID)
	if len(jobs) != 2 {
		t.Fatal("new commit missed")
	}
	p.Branch = "absent"
	s.SaveProject(p)
	poller.Tick(context.Background(), start.Add(30*time.Second))
	current, _ := s.Project(p.ID)
	if current.PollError == "" {
		t.Fatal("poll error invisible")
	}
	p.Branch = "main"
	s.SaveProject(p)
	poller.Tick(context.Background(), start.Add(31*time.Second))
	current, _ = s.Project(p.ID)
	if current.PollError == "" {
		t.Fatal("backoff bypassed")
	}
	poller.Tick(context.Background(), start.Add(50*time.Second))
	current, _ = s.Project(p.ID)
	if current.PollError != "" {
		t.Fatal("poll did not recover")
	}
}

func TestPollingReturningToAnOlderCommitQueuesNewDeployment(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	repo, first := sourceRepo(t)
	p.Repo = repo
	p.Mode = "poll"
	p.PollSeconds = 10
	p, _ = s.SaveProject(p)
	poller := NewPoller(s)
	start := time.Now()
	poller.Tick(context.Background(), start)
	secondCommit(t, repo)
	poller.Tick(context.Background(), start.Add(10*time.Second))
	gitTest(t, repo, "reset", "--hard", first)
	poller.Tick(context.Background(), start.Add(20*time.Second))
	jobs, _ := s.Jobs(p.ID)
	if len(jobs) != 3 || jobs[0].Commit != first {
		t.Fatalf("returning branch target was suppressed: %+v", jobs)
	}
}
