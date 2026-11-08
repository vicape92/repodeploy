package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"github.com/vicape92/repodeploy/internal/envvars"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type purgeRuntime struct {
	*testRuntime
	purged   bool
	volumes  bool
	manifest string
}

func (r *purgeRuntime) Purge(ctx context.Context, p Project, manifest string, volumes bool, log io.Writer) error {
	r.purged, r.volumes, r.manifest = true, volumes, manifest
	return nil
}

func TestDeleteProjectRemovesEverythingItOwns(t *testing.T) {
	e, _, p, _ := engineFixture(t)
	runtime := &purgeRuntime{testRuntime: &testRuntime{}}
	e.Runtime = runtime
	j := runDeployment(t, e, p)
	keep := testProject(t, e.Store)
	os.WriteFile(filepath.Join(e.Store.Root, "secrets", p.ID+"-db_password"), []byte("x"), 0600)
	os.WriteFile(filepath.Join(e.Store.Root, "secrets", p.ID+".webhook"), []byte("x"), 0600)
	os.WriteFile(filepath.Join(e.Store.Root, "secrets", keep.ID+"-db_password"), []byte("y"), 0600)
	if err := e.DeleteProject(context.Background(), p.ID, true); err != nil {
		t.Fatal(err)
	}
	if !runtime.purged || !runtime.volumes || runtime.manifest != j.Manifest {
		t.Fatalf("runtime not purged correctly: %+v", runtime)
	}
	if _, err := e.Store.Project(p.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("project row remains: %v", err)
	}
	if _, err := e.Store.Job(j.ID); err == nil {
		t.Fatal("job row remains")
	}
	for _, path := range []string{filepath.Join("releases", j.ID), filepath.Join("logs", j.ID+".log"), filepath.Join("secrets", p.ID+"-db_password"), filepath.Join("secrets", p.ID+".webhook")} {
		if _, err := os.Stat(filepath.Join(e.Store.Root, path)); !os.IsNotExist(err) {
			t.Fatalf("%s remains", path)
		}
	}
	if _, err := os.Stat(filepath.Join(e.Store.Root, "secrets", keep.ID+"-db_password")); err != nil {
		t.Fatal("another project's secret was deleted")
	}
	if _, err := e.Store.Project(keep.ID); err != nil {
		t.Fatal("another project was deleted")
	}
}

func TestDeleteProjectRefusesWhileDeploymentRuns(t *testing.T) {
	e, _, p, _ := engineFixture(t)
	e.Runtime = &purgeRuntime{testRuntime: &testRuntime{}}
	e.mu.Lock()
	err := e.DeleteProject(context.Background(), p.ID, false)
	e.mu.Unlock()
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("deletion raced a deployment: %v", err)
	}
	if _, err := e.Store.Project(p.ID); err != nil {
		t.Fatal("project deleted while busy")
	}
}

func TestSecretsListStoredAndRequiredNames(t *testing.T) {
	e, _, p, _ := engineFixture(t)
	j := runDeployment(t, e, p)
	os.WriteFile(j.Manifest, []byte(`{"services":{},"secrets":{"db_password":{"file":"x"}},"configs":{"app_config":{"file":"y"}}}`), 0600)
	if err := e.Store.SaveSecret(p.ID, "db_password", []byte("s3cret")); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.SaveSecret(p.ID, "unused", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.SaveSecret(p.ID, "../escape", []byte("v")); err == nil {
		t.Fatal("path-like secret name accepted")
	}
	p, _ = e.Store.Project(p.ID)
	secrets, err := e.Store.Secrets(p)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]SecretInfo{}
	for _, s := range secrets {
		got[s.Name] = s
	}
	if len(got) != 3 || !got["db_password"].Stored || !got["db_password"].Required || got["db_password"].Size != 6 || got["app_config"].Stored || !got["app_config"].Required || !got["unused"].Stored || got["unused"].Required {
		t.Fatalf("unexpected secrets: %+v", secrets)
	}
	if err := e.Store.DeleteSecret(p.ID, "unused"); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.DeleteSecret(p.ID, "unused"); err == nil {
		t.Fatal("deleting a missing secret succeeded")
	}
}

func TestLatestJobsReturnsMostRecentPerProject(t *testing.T) {
	s := testStore(t)
	a, b := testProject(t, s), testProject(t, s)
	s.Enqueue(a.ID, "manual", "", "", "")
	second, _ := s.Enqueue(a.ID, "manual", "", "", "")
	only, _ := s.Enqueue(b.ID, "manual", "", "", "")
	latest, err := s.LatestJobs()
	if err != nil || len(latest) != 2 || latest[a.ID].ID != second.ID || latest[b.ID].ID != only.ID {
		t.Fatalf("latest=%v err=%v", latest, err)
	}
}

func TestBackupIsConsistentAndSkipsCheckouts(t *testing.T) {
	e, _, p, _ := engineFixture(t)
	j := runDeployment(t, e, p)
	root := e.Store.Root
	os.WriteFile(filepath.Join(root, "admin.json"), []byte(`{"salt":"s"}`), 0600)
	os.MkdirAll(filepath.Join(root, "releases", j.ID, "source"), 0700)
	os.WriteFile(filepath.Join(root, "releases", j.ID, "source", "big.bin"), []byte("checkout"), 0600)
	e.Store.SaveSecret(p.ID, "db_password", []byte("s3cret"))
	var archive bytes.Buffer
	if err := Backup(root, &archive); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&archive)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	modes := map[string]int64{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		files[h.Name], modes[h.Name] = b, h.Mode
	}
	for _, name := range []string{"state.db", "admin.json", "secrets/" + p.ID + "-db_password", "releases/" + j.ID + "/compose.json", "logs/" + j.ID + ".log"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("backup lacks %s (has %v)", name, keys(files))
		}
	}
	if modes["secrets/"+p.ID+"-db_password"] != 0600 {
		t.Fatalf("secret mode not preserved: %o", modes["secrets/"+p.ID+"-db_password"])
	}
	for name := range files {
		if strings.Contains(name, "/source/") || strings.HasPrefix(name, ".backup-") || strings.HasSuffix(name, "-wal") {
			t.Fatalf("backup contains %s", name)
		}
	}
	restored := filepath.Join(t.TempDir(), "state.db")
	os.WriteFile(restored, files["state.db"], 0600)
	db, err := sql.Open("sqlite", restored)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err = db.QueryRow(`SELECT COUNT(*) FROM projects WHERE id=?`, p.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("snapshot unusable: count=%d err=%v", count, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(root, ".backup-*")); len(leftovers) != 0 {
		t.Fatalf("temporary snapshot left behind: %v", leftovers)
	}
}

func keys(m map[string][]byte) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

type backupSnapshotGate struct {
	started, release chan struct{}
	once             sync.Once
	bytes.Buffer
}

func (w *backupSnapshotGate) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started); <-w.release })
	return w.Buffer.Write(p)
}
func TestBackupPinsReferencedArtifactsUntilCopyCompletes(t *testing.T) {
	e, _, p, _ := engineFixture(t)
	e.Runtime = &purgeRuntime{testRuntime: &testRuntime{}}
	j := runDeployment(t, e, p)
	output := &backupSnapshotGate{started: make(chan struct{}), release: make(chan struct{})}
	copied := make(chan error, 1)
	go func() { copied <- Backup(e.Store.Root, output) }()
	<-output.started
	deleted := make(chan error, 1)
	go func() { deleted <- e.DeleteProject(context.Background(), p.ID, false) }()
	removed := false
	select {
	case err := <-deleted:
		removed = true
		if err != nil {
			t.Error(err)
		}
		t.Error("deletion removed artifacts while their backup snapshot was pinned")
	case <-time.After(100 * time.Millisecond):
	}
	close(output.release)
	if err := <-copied; err != nil {
		t.Fatal(err)
	}
	if !removed {
		if err := <-deleted; err != nil {
			t.Fatal(err)
		}
	}
	gz, err := gzip.NewReader(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	manifest, _ := filepath.Rel(e.Store.Root, j.Manifest)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == filepath.ToSlash(manifest) {
			return
		}
	}
	t.Fatal("successful backup omitted its database snapshot's active manifest")
}
func TestDeletedProjectCannotBeResurrectedOrReceiveSecrets(t *testing.T) {
	e, _, p, _ := engineFixture(t)
	e.Runtime = &purgeRuntime{testRuntime: &testRuntime{}}
	if _, err := e.Store.Enqueue(p.ID, "github", "audit-delivery", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteProject(context.Background(), p.ID, false); err != nil {
		t.Fatal(err)
	}
	var deliveries int
	if err := e.Store.db.QueryRow("SELECT COUNT(*) FROM deliveries WHERE project_id=?", p.ID).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Error("deleted project's delivery tombstones were retained")
	}
	if err := e.Store.SetEnv(p.ID, []envvars.Update{{Name: "TOKEN", Value: "secret-value"}}); err == nil {
		t.Error("variables written for deleted project")
	}
	if err := e.Store.SaveSecret(p.ID, "password", []byte("secret-value")); err == nil {
		t.Error("secret written for deleted project")
	}
	if _, err := e.Store.SaveProject(p); err == nil {
		t.Error("update resurrected deleted project")
	}
}
func TestSecretSizeLimitAppliesToStore(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	if err := s.SaveSecret(p.ID, "large", make([]byte, (1<<20)+1)); err == nil {
		t.Fatal("oversized secret accepted")
	}
}

func TestBackupExcludesConcurrentSecretDeletion(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	if err := s.SaveSecret(p.ID, "password", []byte("old-secret")); err != nil {
		t.Fatal(err)
	}
	output := &backupSnapshotGate{started: make(chan struct{}), release: make(chan struct{})}
	copied := make(chan error, 1)
	go func() { copied <- Backup(s.Root, output) }()
	<-output.started
	deleted := make(chan error, 1)
	go func() { deleted <- s.DeleteSecret(p.ID, "password") }()
	finished := false
	select {
	case err := <-deleted:
		finished = true
		if err != nil {
			t.Error(err)
		}
		t.Error("secret mutation was not excluded from the backup snapshot")
	case <-time.After(100 * time.Millisecond):
	}
	close(output.release)
	if err := <-copied; err != nil {
		t.Fatal(err)
	}
	if !finished {
		if err := <-deleted; err != nil {
			t.Fatal(err)
		}
	}
}
