package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vicape92/repodeploy/internal/envvars"
)

type testRuntime struct {
	active       string
	failBuild    bool
	failStart    bool
	failHealth   bool
	failRecovery bool
	beforeBuild  func()
	env          []envvars.Var
}

func (r *testRuntime) Forget(context.Context, Project, string, io.Writer) error { return nil }

func (r *testRuntime) Build(ctx context.Context, p Project, checkout, dir string, w io.Writer) (string, error) {
	r.env = p.Env
	if r.beforeBuild != nil {
		r.beforeBuild()
	}
	if r.failBuild {
		return "", errors.New("build failure")
	}
	b, err := os.ReadFile(filepath.Join(checkout, "version.txt"))
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "compose.json")
	return path, os.WriteFile(path, b, 0600)
}
func (r *testRuntime) Activate(ctx context.Context, p Project, path string, w io.Writer) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	r.active = string(b)
	if r.failStart && r.active == "second" {
		return errors.New("startup failure")
	}
	if r.failRecovery && r.active == "first" {
		return errors.New("recovery failure")
	}
	return nil
}
func (r *testRuntime) Healthy(ctx context.Context, p Project, path string, w io.Writer) error {
	if r.failHealth && r.active == "second" {
		return errors.New("health failure")
	}
	return nil
}
func (r *testRuntime) Remove(ctx context.Context, p Project, path string, w io.Writer) error {
	r.active = ""
	return nil
}

func engineFixture(t *testing.T) (*Engine, *testRuntime, Project, string) {
	t.Helper()
	s := testStore(t)
	repo, _ := sourceRepo(t)
	p := testProject(t, s)
	p.Repo = repo
	p, err := s.SaveProject(p)
	if err != nil {
		t.Fatal(err)
	}
	r := &testRuntime{}
	return &Engine{Store: s, Runtime: r}, r, p, repo
}

func runDeployment(t *testing.T, e *Engine, p Project) Job {
	t.Helper()
	j, err := e.Store.Enqueue(p.ID, "manual", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
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

func secondCommit(t *testing.T, repo string) {
	t.Helper()
	os.WriteFile(filepath.Join(repo, "version.txt"), []byte("second"), 0600)
	gitTest(t, repo, "commit", "-am", "second")
}

func TestBuildFailureCannotTouchActiveRelease(t *testing.T) {
	e, r, p, repo := engineFixture(t)
	first := runDeployment(t, e, p)
	if first.Phase != "succeeded" {
		t.Fatalf("first: %+v", first)
	}
	secondCommit(t, repo)
	r.failBuild = true
	r.beforeBuild = func() {
		if r.active != "first" {
			t.Fatal("active version stopped during build")
		}
	}
	failed := runDeployment(t, e, p)
	current, _ := e.Store.Project(p.ID)
	if failed.Phase != "failed" || current.Active != first.ID || r.active != "first" {
		t.Fatalf("build modified active: %+v %+v", failed, current)
	}
}

func TestActivationAndHealthFailureRestorePreviousVersion(t *testing.T) {
	for _, stage := range []string{"startup", "health"} {
		t.Run(stage, func(t *testing.T) {
			e, r, p, repo := engineFixture(t)
			first := runDeployment(t, e, p)
			secondCommit(t, repo)
			r.failStart = stage == "startup"
			r.failHealth = stage == "health"
			failed := runDeployment(t, e, p)
			current, _ := e.Store.Project(p.ID)
			if failed.Phase != "failed" || failed.Recovery != "restored" || current.Active != first.ID || r.active != "first" {
				t.Fatalf("recovery failed: %+v %+v %s", failed, current, r.active)
			}
		})
	}
}

func TestFailedRecoveryBlocksProjectAndDoesNotClaimHealthyActive(t *testing.T) {
	e, r, p, repo := engineFixture(t)
	runDeployment(t, e, p)
	secondCommit(t, repo)
	r.failHealth = true
	r.failRecovery = true
	failed := runDeployment(t, e, p)
	current, _ := e.Store.Project(p.ID)
	if failed.Recovery != "failed" || !current.Blocked || !current.Paused || current.Active != "" {
		t.Fatalf("unsafe recovery state: %+v %+v", failed, current)
	}
	if _, err := e.Store.Enqueue(p.ID, "manual", "", "", ""); !errors.Is(err, ErrPaused) {
		t.Fatalf("blocked project accepts deploy: %v", err)
	}
}

func TestRestartReconcilesInterruptedActivation(t *testing.T) {
	for _, stage := range []string{"activating", "checking"} {
		t.Run(stage, func(t *testing.T) {
			e, r, p, _ := engineFixture(t)
			first := runDeployment(t, e, p)
			j, _ := e.Store.Enqueue(p.ID, "manual", "", "", "")
			e.Store.Claim()
			dir := filepath.Join(e.Store.Root, "releases", j.ID)
			os.MkdirAll(dir, 0700)
			manifest := filepath.Join(dir, "compose.json")
			os.WriteFile(manifest, []byte("second"), 0600)
			e.Store.SetArtifact(j.ID, manifest, first.ID)
			e.Store.SetPhase(j.ID, stage, strings.Repeat("b", 40))
			r.active = "second"
			if err := e.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			got, _ := e.Store.Job(j.ID)
			current, _ := e.Store.Project(p.ID)
			if got.Phase != "failed" || got.Recovery != "restored" || r.active != "first" || current.Active != first.ID {
				t.Fatalf("restart did not restore: %+v %+v", got, current)
			}
		})
	}
}

func TestFirstFailedActivationIsRemoved(t *testing.T) {
	e, r, p, repo := engineFixture(t)
	secondCommit(t, repo)
	r.failHealth = true
	j := runDeployment(t, e, p)
	if j.Phase != "failed" || j.Recovery != "removed" || r.active != "" {
		t.Fatalf("failed first release left running: %+v %s", j, r.active)
	}
}

func TestManualRollbackUsesStoredImageAndPausesAutomation(t *testing.T) {
	e, r, p, repo := engineFixture(t)
	first := runDeployment(t, e, p)
	secondCommit(t, repo)
	runDeployment(t, e, p)
	os.RemoveAll(repo)
	j, err := e.Store.Enqueue(p.ID, "manual", "", "", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Store.Job(j.ID)
	current, _ := e.Store.Project(p.ID)
	if got.Phase != "succeeded" || got.Commit != first.Commit || current.Active != j.ID || !current.Paused || r.active != "first" {
		t.Fatalf("rollback contract: %+v %+v %s", got, current, r.active)
	}
}

func TestRestartRequeuesPreparationWithPinnedSHAAndDiscardsPartialFiles(t *testing.T) {
	e, _, p, repo := engineFixture(t)
	j, _ := e.Store.Enqueue(p.ID, "manual", "", "", "")
	e.Store.Claim()
	sha := gitTest(t, repo, "rev-parse", "HEAD")
	e.Store.SetPhase(j.ID, "building", sha)
	checkout := filepath.Join(e.Store.Root, "releases", j.ID, "source")
	os.MkdirAll(checkout, 0700)
	os.WriteFile(filepath.Join(checkout, "partial"), []byte("bad"), 0600)
	secondCommit(t, repo)
	if err := e.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Store.Job(j.ID)
	if got.Phase != "succeeded" || got.Commit != sha {
		t.Fatalf("lost exact commit: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(checkout, "partial")); !os.IsNotExist(err) {
		t.Fatal("reused partial checkout")
	}
}

func TestRollbackCopiesVersionedSecretsIntoNewRelease(t *testing.T) {
	e, _, p, _ := engineFixture(t)
	first := runDeployment(t, e, p)
	secretDir := filepath.Join(filepath.Dir(first.Manifest), "secrets")
	os.MkdirAll(secretDir, 0700)
	oldPath := filepath.Join(secretDir, "password")
	os.WriteFile(oldPath, []byte("original-secret"), 0600)
	manifest := `{"services":{"app":{"image":"sha256:abc"}},"secrets":{"password":{"file":` + fmt.Sprintf("%q", oldPath) + `}}}`
	os.WriteFile(first.Manifest, []byte(manifest), 0600)
	target := filepath.Join(t.TempDir(), "rollback")
	os.MkdirAll(target, 0700)
	copied, err := copyArtifact(first.Manifest, target)
	if err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Dir(first.Manifest))
	b, err := os.ReadFile(filepath.Join(target, "secrets", "password"))
	if err != nil || string(b) != "original-secret" {
		t.Fatalf("rollback depends on prunable source: %s %v", b, err)
	}
	contents, _ := os.ReadFile(copied)
	if strings.Contains(string(contents), oldPath) {
		t.Fatal("rollback still refers to old release")
	}
}

func TestFailedBuildHistoryIsPrunedWithoutBreakingDeliveryDeduplication(t *testing.T) {
	e, r, p, _ := engineFixture(t)
	p.Retain = 1
	p, _ = e.Store.SaveProject(p)
	r.failBuild = true
	p.Branch = "absent"
	p, _ = e.Store.SaveProject(p)
	first, err := e.Store.Enqueue(p.ID, "poll", "old-delivery", strings.Repeat("a", 40), "")
	if err != nil {
		t.Fatal(err)
	}
	// Preparation failures are also subject to retention.
	e.Step(context.Background())
	p.Branch = "main"
	p, _ = e.Store.SaveProject(p)
	runDeployment(t, e, p)
	runDeployment(t, e, p)
	jobs, _ := e.Store.Jobs(p.ID)
	if len(jobs) != 1 {
		t.Fatalf("failed history grows without limit: %d", len(jobs))
	}
	again, err := e.Store.Enqueue(p.ID, "poll", "old-delivery", strings.Repeat("a", 40), "")
	if err != nil || again.ID != first.ID {
		t.Fatalf("pruned delivery lost deduplication: %v %v", again, err)
	}
}

type crashRuntime struct {
	testRuntime
	path  string
	crash bool
}

func (r *crashRuntime) Activate(ctx context.Context, p Project, path string, w io.Writer) error {
	if err := r.testRuntime.Activate(ctx, p, path, w); err != nil {
		return err
	}
	if err := os.WriteFile(r.path, []byte(r.active), 0600); err != nil {
		return err
	}
	if r.crash {
		os.Exit(91)
	}
	return nil
}

func TestProcessDeathDuringActivationReleasesLockAndRestoresPrevious(t *testing.T) {
	if root := os.Getenv("REPODEPLOY_CRASH_HELPER"); root != "" {
		unlock, err := Lock(root)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		s, err := OpenStore(root)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		runtime := &crashRuntime{path: filepath.Join(root, "active-test"), crash: true}
		e := Engine{Store: s, Runtime: runtime}
		e.Step(context.Background())
		t.Fatal("helper did not crash")
	}
	e, _, p, repo := engineFixture(t)
	first := runDeployment(t, e, p)
	secondCommit(t, repo)
	job, _ := e.Store.Enqueue(p.ID, "manual", "", "", "")
	command := exec.Command(os.Args[0], "-test.run=^TestProcessDeathDuringActivationReleasesLockAndRestoresPrevious$")
	command.Env = append(os.Environ(), "REPODEPLOY_CRASH_HELPER="+e.Store.Root)
	err := command.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 91 {
		t.Fatalf("unexpected helper exit: %v", err)
	}
	unlock, err := Lock(e.Store.Root)
	if err != nil {
		t.Fatal("crashed process kept engine lock:", err)
	}
	defer unlock()
	r := &crashRuntime{path: filepath.Join(e.Store.Root, "active-test")}
	e.Runtime = r
	if err = e.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(r.path)
	got, _ := e.Store.Job(job.ID)
	current, _ := e.Store.Project(p.ID)
	if string(b) != "first" || got.Recovery != "restored" || current.Active != first.ID {
		t.Fatalf("process crash recovery: %s %+v %+v", b, got, current)
	}
}

func TestLogsRedactMultilineSecretsAcrossProcessWrites(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	j, _ := s.Enqueue(p.ID, "manual", "", "", "")
	os.WriteFile(filepath.Join(s.Root, "secrets", p.ID+"-env"), []byte("USER=example\nPASSWORD=supersecret\n"), 0600)
	w := s.LogWriter(j.ID)
	w.Write([]byte("error: super"))
	w.Write([]byte("secret rejected\n"))
	w.Close()
	logs, err := s.Logs(j.ID)
	if err != nil || strings.Contains(logs, "supersecret") || !strings.Contains(logs, "[REDACTED]") {
		t.Fatalf("secret in persisted logs: %q %v", logs, err)
	}
}

func TestBlockedRecoveryKeepsLastHealthyArtifact(t *testing.T) {
	e, r, p, repo := engineFixture(t)
	p.Retain = 1
	p, _ = e.Store.SaveProject(p)
	first := runDeployment(t, e, p)
	secondCommit(t, repo)
	r.failHealth = true
	r.failRecovery = true
	runDeployment(t, e, p)
	if _, err := os.Stat(first.Manifest); err != nil {
		t.Fatal("last recovery artifact was pruned:", err)
	}
}

func TestRollbackPreparationFailureCannotUnblockProject(t *testing.T) {
	e, _, p, _ := engineFixture(t)
	first := runDeployment(t, e, p)
	os.WriteFile(first.Manifest, []byte(`{"secrets":{"password":{"file":"/nonexistent/pushvori-secret"}}}`), 0600)
	e.Store.SetActive(p.ID, "")
	e.Store.Pause(p.ID, true)
	job, err := e.Store.Enqueue(p.ID, "manual", "", "", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.Step(context.Background())
	got, _ := e.Store.Job(job.ID)
	current, _ := e.Store.Project(p.ID)
	if got.Phase != "failed" || !current.Blocked {
		t.Fatalf("failed rollback unblocked project: %+v %+v", got, current)
	}
}

func TestRetentionProtectsQueuedRollbackTarget(t *testing.T) {
	e, _, p, repo := engineFixture(t)
	p.Retain = 2
	p, _ = e.Store.SaveProject(p)
	first := runDeployment(t, e, p)
	secondCommit(t, repo)
	runDeployment(t, e, p)
	e.Store.Enqueue(p.ID, "manual", "", "", "")
	rollback, err := e.Store.Enqueue(p.ID, "manual", "", "", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.Step(context.Background())
	e.Step(context.Background())
	got, _ := e.Store.Job(rollback.ID)
	if got.Phase != "succeeded" {
		t.Fatalf("accepted rollback lost its target: %+v", got)
	}
}

func TestInternalDeploymentTimeoutFailsJobAndAdvancesQueue(t *testing.T) {
	e, _, p, _ := engineFixture(t)
	e.Timeout = time.Nanosecond
	first := runDeployment(t, e, p)
	if first.Phase != "failed" {
		t.Fatalf("timed out job remains in progress: %+v", first)
	}
	e.Timeout = time.Minute
	next := runDeployment(t, e, p)
	if next.Phase != "succeeded" {
		t.Fatalf("queue stuck behind timed out job: %+v", next)
	}
}

func TestLateWebhookCannotDowngradeCurrentBranch(t *testing.T) {
	e, r, p, repo := engineFixture(t)
	firstSHA := gitTest(t, repo, "rev-parse", "HEAD")
	secondCommit(t, repo)
	secondSHA := gitTest(t, repo, "rev-parse", "HEAD")
	e.Store.Enqueue(p.ID, "github", "delivery-B", secondSHA, "")
	e.Step(context.Background())
	late, _ := e.Store.Enqueue(p.ID, "github", "delivery-A", firstSHA, "")
	e.Step(context.Background())
	current, _ := e.Store.Project(p.ID)
	active, _ := e.Store.Job(current.Active)
	lateJob, _ := e.Store.Job(late.ID)
	if r.active != "second" || active.Commit != secondSHA || lateJob.Phase != "failed" {
		t.Fatalf("late webhook downgraded deployment: %+v %+v %s", active, lateJob, r.active)
	}
}

func TestSupersededPollingJobCannotDowngradeCurrentBranch(t *testing.T) {
	e, r, p, repo := engineFixture(t)
	old := gitTest(t, repo, "rev-parse", "HEAD")
	secondCommit(t, repo)
	head := gitTest(t, repo, "rev-parse", "HEAD")
	e.Store.Enqueue(p.ID, "poll", "", head, "")
	e.Step(context.Background())
	stale, _ := e.Store.Enqueue(p.ID, "poll", "", old, "")
	e.Step(context.Background())
	j, _ := e.Store.Job(stale.ID)
	if r.active != "second" || j.Phase != "failed" {
		t.Fatalf("superseded polling target activated: %+v %s", j, r.active)
	}
}
