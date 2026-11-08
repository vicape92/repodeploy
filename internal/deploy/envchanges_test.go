package deploy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/vicape92/repodeploy/internal/envvars"
)

func stepOnce(t *testing.T, e *Engine, j Job) Job {
	t.Helper()
	did, err := e.Step(context.Background())
	if err != nil || !did {
		t.Fatalf("step=%v err=%v", did, err)
	}
	j, err = e.Store.Job(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestEngineBuildsWithVariablesAndRecordsTheirDigest(t *testing.T) {
	e, r, p, _ := engineFixture(t)
	if err := e.Store.Env.Set(p.ID, []envvars.Update{{Name: "API_KEY", Value: "very-secret-value"}}); err != nil {
		t.Fatal(err)
	}
	j := runDeployment(t, e, p)
	vars, _ := e.Store.Env.List(p.ID)
	if j.Phase != "succeeded" || !reflect.DeepEqual(r.env, vars) || j.Project.EnvDigest != envvars.Digest(vars) {
		t.Fatalf("job=%+v runtime env=%+v", j, r.env)
	}
	var snapshot string
	if err := e.Store.db.QueryRow(`SELECT snapshot FROM jobs WHERE id=?`, j.ID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snapshot, "very-secret-value") {
		t.Fatal("variable value persisted in the job snapshot")
	}
}

func TestConfigChangeRedeploysActiveCommitAndRollbackRestoresDigest(t *testing.T) {
	e, r, p, repo := engineFixture(t)
	if _, err := e.Store.EnqueueConfigChange(p.ID); !errors.Is(err, ErrNoActive) {
		t.Fatalf("config change without release: %v", err)
	}
	first := runDeployment(t, e, p)
	current, _ := e.Store.Project(p.ID)
	if pending, err := e.Store.EnvPending(current); err != nil || pending {
		t.Fatalf("pending without changes: %v %v", pending, err)
	}
	e.Store.Env.Set(p.ID, []envvars.Update{{Name: "MODE", Value: "blue"}})
	if pending, _ := e.Store.EnvPending(current); !pending {
		t.Fatal("variable change not detected")
	}
	secondCommit(t, repo) // the branch moves; deploy changes must keep the active commit
	j, err := e.Store.EnqueueConfigChange(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	j = stepOnce(t, e, j)
	if j.Phase != "succeeded" || j.Commit != first.Commit || r.active != "first" || len(r.env) != 1 {
		t.Fatalf("config change: %+v active=%s env=%v", j, r.active, r.env)
	}
	current, _ = e.Store.Project(p.ID)
	if pending, _ := e.Store.EnvPending(current); pending {
		t.Fatal("still pending after deploying changes")
	}
	rollback, err := e.Store.Enqueue(p.ID, "manual", "", "", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	rollback = stepOnce(t, e, rollback)
	current, _ = e.Store.Project(p.ID)
	if rollback.Phase != "succeeded" || rollback.Project.EnvDigest != envvars.Digest(nil) {
		t.Fatalf("rollback: %+v", rollback)
	}
	if pending, _ := e.Store.EnvPending(current); !pending {
		t.Fatal("restoring the old release should restore its variables")
	}
}

func TestLogWriterRedactsEnvironmentValues(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	s.Env.Set(p.ID, []envvars.Update{{Name: "TOKEN", Value: "tok-123456"}})
	j, err := s.Enqueue(p.ID, "manual", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	w := s.LogWriter(j.ID)
	fmt.Fprintln(w, "using tok-123456 now")
	w.Close()
	logs, _ := s.Logs(j.ID)
	if strings.Contains(logs, "tok-123456") || !strings.Contains(logs, "[REDACTED]") {
		t.Fatalf("value not redacted: %q", logs)
	}
}

func TestDeleteProjectRemovesOnlyItsVariables(t *testing.T) {
	e, _, p, _ := engineFixture(t)
	e.Runtime = &purgeRuntime{testRuntime: &testRuntime{}}
	keep := testProject(t, e.Store)
	e.Store.Env.Set(p.ID, []envvars.Update{{Name: "A", Value: "1"}})
	e.Store.Env.Set(keep.ID, []envvars.Update{{Name: "B", Value: "2"}})
	if err := e.DeleteProject(context.Background(), p.ID, false); err != nil {
		t.Fatal(err)
	}
	if vars, _ := e.Store.Env.List(p.ID); len(vars) != 0 {
		t.Fatal("deleted project's variables remain")
	}
	if vars, _ := e.Store.Env.List(keep.ID); len(vars) != 1 {
		t.Fatal("another project's variables were deleted")
	}
}

func TestLogWriterRedactsEachLineOfMultiLineValues(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	pem := "-----BEGIN KEY-----\nMIIEvQIBADANBgkqhkiG9w0B\nAQEFAASCBKcwggSjAgEAAoIBAQC7\n-----END KEY-----"
	if err := s.Env.Set(p.ID, []envvars.Update{{Name: "PRIVATE_KEY", Value: pem}}); err != nil {
		t.Fatal(err)
	}
	j, err := s.Enqueue(p.ID, "manual", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	w := s.LogWriter(j.ID)
	fmt.Fprintln(w, pem)
	w.Close()
	logs, _ := s.Logs(j.ID)
	for _, line := range strings.Split(pem, "\n") {
		if strings.Contains(logs, line) {
			t.Fatalf("line %q not redacted: %q", line, logs)
		}
	}
	if !strings.Contains(logs, "[REDACTED]") {
		t.Fatalf("nothing redacted: %q", logs)
	}
}
