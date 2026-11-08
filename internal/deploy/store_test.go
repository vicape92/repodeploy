package deploy

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testProject(t *testing.T, s *Store) Project {
	t.Helper()
	p, err := s.SaveProject(Project{Name: "example", Repo: "https://example.org/team/app.git", Branch: "main", Mode: "manual", Dockerfile: "Dockerfile", ContainerPort: 8080, Port: 18080, HealthURL: "http://127.0.0.1:18080/health", PollSeconds: 60, Retain: 5})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestQueueDeduplicatesConcurrentDeliveriesDurably(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	var wg sync.WaitGroup
	ids := make(chan string, 20)
	for range 20 {
		wg.Go(func() {
			j, err := s.Enqueue(p.ID, "github", "delivery-1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "")
			if err != nil {
				t.Error(err)
				return
			}
			ids <- j.ID
		})
	}
	wg.Wait()
	close(ids)
	var first string
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("duplicate jobs: %s, %s", first, id)
		}
	}
	jobs, err := s.Jobs(p.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%v err=%v", jobs, err)
	}
	root := s.Root
	s.Close()
	reopened, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	j, err := reopened.Enqueue(p.ID, "github", "delivery-1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "")
	if err != nil || j.ID != first {
		t.Fatalf("duplicate after restart: %v %v", j, err)
	}
}

func TestQueueClaimIsGlobalAndFIFO(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	first, _ := s.Enqueue(p.ID, "manual", "", "", "")
	second, _ := s.Enqueue(p.ID, "manual", "", "", "")
	var wg sync.WaitGroup
	claimed := make(chan Job, 10)
	for range 10 {
		wg.Go(func() {
			j, err := s.Claim()
			if err == nil {
				claimed <- j
			} else if !errors.Is(err, ErrNoJob) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	close(claimed)
	n := 0
	for j := range claimed {
		n++
		if j.ID != first.ID {
			t.Fatalf("non FIFO claim: %s", j.ID)
		}
	}
	if n != 1 {
		t.Fatalf("claimed %d jobs", n)
	}
	if err := s.Finish(first.ID, "failed", "build failed", ""); err != nil {
		t.Fatal(err)
	}
	next, err := s.Claim()
	if err != nil || next.ID != second.ID {
		t.Fatalf("next=%v %v", next, err)
	}
}

func TestAutomaticCommitDeduplicationAndManualRetry(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	a, err := s.Enqueue(p.ID, "poll", "", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Enqueue(p.ID, "github", "another-delivery", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "")
	if err != nil || b.ID != a.ID {
		t.Fatalf("not deduplicated: %v %v", b, err)
	}
	c, err := s.Enqueue(p.ID, "manual", "", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "")
	if err != nil || c.ID == a.ID {
		t.Fatalf("manual retry suppressed: %v %v", c, err)
	}
}

func TestQueuedProjectSnapshotCannotChangeUnderJob(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	j, _ := s.Enqueue(p.ID, "manual", "", "", "")
	p.Branch = "other"
	if _, err := s.SaveProject(p); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim()
	if err != nil || claimed.ID != j.ID || claimed.Project.Branch != "main" {
		t.Fatalf("snapshot changed: %v %v", claimed, err)
	}
}

func TestEngineLockExcludesSecondProcessOwner(t *testing.T) {
	root := t.TempDir()
	release, err := Lock(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Lock(root)
	if err == nil {
		t.Fatal("second engine acquired lock")
	}
	release()
	again, err := Lock(root)
	if err != nil {
		t.Fatal(err)
	}
	again()
}

func TestJobRecoveryPreservesPinnedCommit(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	j, _ := s.Enqueue(p.ID, "manual", "", "", "")
	s.Claim()
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := s.SetPhase(j.ID, "preparing", sha); err != nil {
		t.Fatal(err)
	}
	if err := s.Requeue(j.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Claim()
	if err != nil || got.Commit != sha {
		t.Fatalf("lost pinned SHA: %v %v", got, err)
	}
	if filepath.Dir(s.Root) == "." {
		t.Fatal("state directory must be absolute")
	}
}

func TestStoreRefusesNewerSchemaWithoutDowngradingVersion(t *testing.T) {
	s := testStore(t)
	root := s.Root
	if _, err := s.db.Exec(`PRAGMA user_version=99`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	newer, err := OpenStore(root)
	if err == nil {
		newer.Close()
		t.Fatal("opened unsupported newer schema")
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 99 {
		t.Fatalf("schema version changed: %d %v", version, err)
	}
}

func TestWaitingCIDoesNotBlockClaimsAndSurvivesRecoveryQuery(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	first, _ := s.Enqueue(p.ID, "manual", "", "", "")
	s.Claim()
	s.SetPhase(first.ID, "waiting_ci", strings.Repeat("a", 40))
	behind, _ := s.Enqueue(p.ID, "manual", "", "", "")
	other := testProject(t, s)
	second, _ := s.Enqueue(other.ID, "manual", "", "", "")
	got, err := s.Claim()
	if err != nil || got.ID != second.ID {
		t.Fatalf("CI wait blocked queue: %+v %v", got, err)
	}
	s.Finish(second.ID, "failed", "fixture", "")
	interrupted, err := s.Interrupted()
	if err != nil || len(interrupted) != 0 {
		t.Fatalf("CI wait treated as interrupted activation: %+v %v", interrupted, err)
	}
	s.Finish(first.ID, "failed", "CI fixture", "")
	next, err := s.Claim()
	if err != nil || next.ID != behind.ID {
		t.Fatalf("project FIFO did not resume: %+v %v", next, err)
	}
}

func TestQueueAndHistoryQueriesUseIndexes(t *testing.T) {
	s := testStore(t)
	for _, query := range []string{
		"SELECT seq FROM jobs WHERE phase='queued' ORDER BY seq LIMIT 1",
		"SELECT id FROM jobs WHERE project_id='p' AND retained=1 ORDER BY seq DESC",
		"SELECT MAX(seq) FROM jobs WHERE retained=1 GROUP BY project_id",
	} {
		rows, err := s.db.Query("EXPLAIN QUERY PLAN " + query)
		if err != nil {
			t.Fatal(err)
		}
		indexed := false
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(detail, "USING") {
				indexed = true
			}
		}
		rows.Close()
		if !indexed {
			t.Errorf("query scans unindexed history: %s", query)
		}
	}
}

func TestInFlightQueueCheckUsesPartialIndex(t *testing.T) {
	s := testStore(t)
	rows, err := s.db.Query("PRAGMA index_list(jobs)")
	if err != nil {
		t.Fatal(err)
	}
	var partials []string
	for rows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatal(err)
		}
		if partial != 0 {
			partials = append(partials, name)
		}
	}
	rows.Close()
	rows, err = s.db.Query("EXPLAIN QUERY PLAN SELECT 1 FROM jobs WHERE phase NOT IN ('queued','waiting_ci','succeeded','failed')")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		for _, name := range partials {
			if strings.Contains(detail, "INDEX "+name) {
				return
			}
		}
	}
	t.Fatal("idle worker scans terminal history to check whether a job is in flight")
}
