package deploy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	poller.Tick(context.Background(), start.Add(31*time.Second))
	current, _ = s.Project(p.ID)
	if current.PollError == "" {
		t.Fatal("backoff bypassed")
	}
	p.Branch = "main"
	s.SaveProject(p)
	poller.Tick(context.Background(), start.Add(32*time.Second))
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

func TestPollingSlowRepositoryDoesNotDelayAnotherProject(t *testing.T) {
	s := testStore(t)
	a, b := testProject(t, s), testProject(t, s)
	if a.ID > b.ID {
		a, b = b, a
	}
	a.Repo, a.Mode = "https://example.test/slow.git", "poll"
	b.Repo, b.Mode = "https://example.test/fast.git", "poll"
	s.SaveProject(a)
	s.SaveProject(b)
	dir := t.TempDir()
	started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
	script := "#!/bin/sh\ncase \"$*\" in *slow.git*) touch '" + started + "'; while [ ! -f '" + release + "' ]; do sleep 0.01; done;; esac\nprintf '" + strings.Repeat("a", 40) + "\\trefs/heads/main\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	done := make(chan struct{})
	go func() { defer close(done); NewPoller(s).Tick(context.Background(), time.Now()) }()
	defer func() { os.WriteFile(release, nil, 0600); <-done }()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("slow repository not started")
		}
		time.Sleep(time.Millisecond)
	}
	deadline = time.Now().Add(250 * time.Millisecond)
	for {
		jobs, err := s.Jobs(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("slow repository delayed the fast project's poll")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPollingSkipsDatabaseUntilNextDue(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	p.Repo, _ = sourceRepo(t)
	p.Mode = "poll"
	s.SaveProject(p)
	poller := NewPoller(s)
	now := time.Now()
	poller.Tick(context.Background(), now)
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); poller.Tick(context.Background(), now.Add(time.Second)) }()
	select {
	case <-done:
		conn.Close()
	case <-time.After(200 * time.Millisecond):
		conn.Close()
		<-done
		t.Fatal("idle poller queried the database while no project was due")
	}
}
func TestPollingDiscardsTargetWhenSourceChanges(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	p.Mode = "poll"
	s.SaveProject(p)
	dir := t.TempDir()
	started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
	script := "#!/bin/sh\ntouch '" + started + "'\nwhile [ ! -f '" + release + "' ]; do sleep 0.01; done\nprintf '" + strings.Repeat("a", 40) + "\\trefs/heads/main\\n'\n"
	os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	done := make(chan struct{})
	go func() { defer close(done); NewPoller(s).Tick(context.Background(), time.Now()) }()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.WriteFile(release, nil, 0600)
			<-done
			t.Fatal("poll not started")
		}
		time.Sleep(time.Millisecond)
	}
	p.Branch = "different"
	if _, err := s.SaveProject(p); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(release, nil, 0600)
	<-done
	jobs, err := s.Jobs(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatal("commit resolved from obsolete source was queued with the new project configuration")
	}
}

func TestPollingNewSourceDoesNotInheritOldBackoff(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	p.Repo, _ = sourceRepo(t)
	p.Mode = "poll"
	p.Branch = "absent"
	s.SaveProject(p)
	poller := NewPoller(s)
	now := time.Now()
	poller.Tick(context.Background(), now)
	p.Branch = "main"
	s.SaveProject(p)
	poller.Tick(context.Background(), now.Add(time.Second))
	jobs, err := s.Jobs(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatal("new source inherited the previous source's backoff")
	}
}
