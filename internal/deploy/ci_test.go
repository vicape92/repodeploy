package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type ciTransport struct{ base *url.URL }

func (r ciTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copy := req.Clone(req.Context())
	u := *req.URL
	copy.URL = &u
	copy.URL.Scheme = r.base.Scheme
	copy.URL.Host = r.base.Host
	return http.DefaultTransport.RoundTrip(copy)
}
func ciFixture(t *testing.T) (*Engine, *testRuntime, Project, *string) {
	t.Helper()
	e, r, p, _ := engineFixture(t)
	state := new(string)
	*state = "INPROGRESS"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/site/oauth2/access_token":
			json.NewEncoder(w).Encode(map[string]any{"access_token": "ci-token", "refresh_token": "ci-refresh", "scopes": "account repository", "expires_in": 3600})
		case strings.HasSuffix(req.URL.Path, "/statuses"):
			fmt.Fprintf(w, `{"values":[{"state":%q}]}`, *state)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	e.Store.Sources.Client = &http.Client{Transport: ciTransport{u}}
	if err := e.Store.Sources.SaveBitbucket("client", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.Sources.ConnectBitbucket(context.Background(), "code", "http://localhost/callback"); err != nil {
		t.Fatal(err)
	}
	p.WaitCI = true
	p.CIProvider = "bitbucket"
	p.RepositoryID = "team/app"
	p.CITimeout = 1800
	p, _ = e.Store.SaveProject(p)
	return e, r, p, state
}
func TestCIGateParksExactCommitAndDoesNotBlockAnotherProject(t *testing.T) {
	e, r, p, status := ciFixture(t)
	job := runDeployment(t, e, p)
	if job.Phase != "waiting_ci" || job.Commit == "" || r.active != "" {
		t.Fatalf("CI did not prevent build/activation: %+v %s", job, r.active)
	}
	other := testProject(t, e.Store)
	other.Repo = p.Repo
	other, _ = e.Store.SaveProject(other)
	deployed := runDeployment(t, e, other)
	if deployed.Phase != "succeeded" {
		t.Fatalf("CI blocked other project: %+v", deployed)
	}
	*status = "SUCCESSFUL"
	// Recreate the engine to exercise durable waiting state after restart.
	resumed := &Engine{Store: e.Store, Runtime: r}
	if err := resumed.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Store.Job(job.ID)
	if got.Phase != "succeeded" || got.Commit != job.Commit {
		t.Fatalf("CI resume changed commit: %+v", got)
	}
}
func TestCIFailureAndTimeoutNeverActivate(t *testing.T) {
	for _, reason := range []string{"failed", "timeout"} {
		t.Run(reason, func(t *testing.T) {
			e, r, p, status := ciFixture(t)
			job := runDeployment(t, e, p)
			if job.Phase != "waiting_ci" {
				t.Fatal(job.Phase)
			}
			if reason == "failed" {
				*status = "FAILED"
			} else {
				e.Store.db.Exec(`UPDATE jobs SET started=? WHERE id=?`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), job.ID)
			}
			restarted := &Engine{Store: e.Store, Runtime: r}
			restarted.Step(context.Background())
			got, _ := e.Store.Job(job.ID)
			if got.Phase != "failed" || r.active != "" {
				t.Fatalf("CI %s activated app: %+v %s", reason, got, r.active)
			}
		})
	}
}
func TestCIDefaultOffDoesNotReadProviderAndRollbackBypassesGate(t *testing.T) {
	e, r, p, _ := ciFixture(t)
	p.WaitCI = false
	p, _ = e.Store.SaveProject(p)
	first := runDeployment(t, e, p)
	if first.Phase != "succeeded" {
		t.Fatal(first.Phase)
	}
	p.WaitCI = true
	p, _ = e.Store.SaveProject(p)
	rollback, err := e.Store.Enqueue(p.ID, "manual", "", "", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.Step(context.Background())
	got, _ := e.Store.Job(rollback.ID)
	if got.Phase != "succeeded" || r.active != "first" {
		t.Fatalf("rollback waited for CI: %+v", got)
	}
}

func TestRollbackCancelsExistingCIWait(t *testing.T) {
	e, _, p, status := ciFixture(t)
	p.WaitCI = false
	p, _ = e.Store.SaveProject(p)
	first := runDeployment(t, e, p)
	p.WaitCI = true
	p, _ = e.Store.SaveProject(p)
	waiting := runDeployment(t, e, p)
	rollback, _ := e.Store.Enqueue(p.ID, "manual", "", "", first.ID)
	e.Step(context.Background())
	restored, _ := e.Store.Job(rollback.ID)
	old, _ := e.Store.Job(waiting.ID)
	if restored.Phase != "succeeded" || old.Phase != "failed" {
		t.Fatalf("rollback left pending CI deployment: %+v %+v", restored, old)
	}
	*status = "SUCCESSFUL"
	after := &Engine{Store: e.Store, Runtime: e.Runtime}
	after.Step(context.Background())
	current, _ := e.Store.Project(p.ID)
	if current.Active != rollback.ID {
		t.Fatal("old CI job replaced manual rollback")
	}
}

func TestCIFailuresRespectHistoryRetention(t *testing.T) {
	e, _, p, status := ciFixture(t)
	p.Retain = 1
	p, _ = e.Store.SaveProject(p)
	for range 2 {
		*status = "INPROGRESS"
		runDeployment(t, e, p)
		*status = "FAILED"
		e = &Engine{Store: e.Store, Runtime: e.Runtime}
		e.Step(context.Background())
	}
	jobs, _ := e.Store.Jobs(p.ID)
	if len(jobs) != 1 {
		t.Fatalf("CI failures bypass retention: %d", len(jobs))
	}
}

type cancelCITransport struct{ entered chan struct{} }

func (c cancelCITransport) RoundTrip(r *http.Request) (*http.Response, error) {
	close(c.entered)
	<-r.Context().Done()
	return nil, r.Context().Err()
}
func TestShutdownDuringCIRequestPreservesWaitingJob(t *testing.T) {
	e, _, p, _ := ciFixture(t)
	waiting := runDeployment(t, e, p)
	entered := make(chan struct{})
	e.Store.Sources.Client = &http.Client{Transport: cancelCITransport{entered}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	restarted := &Engine{Store: e.Store, Runtime: e.Runtime}
	go func() { _, err := restarted.Step(ctx); done <- err }()
	<-entered
	cancel()
	err := <-done
	job, _ := e.Store.Job(waiting.ID)
	if !errors.Is(err, context.Canceled) || job.Phase != "waiting_ci" {
		t.Fatalf("shutdown failed waiting job: %+v %v", job, err)
	}
}
