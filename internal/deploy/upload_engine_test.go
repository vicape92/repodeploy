package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vicape92/repodeploy/internal/envvars"
	"github.com/vicape92/repodeploy/internal/upload"
)

func uploadFixture(t *testing.T) (*Engine, *testRuntime, Project) {
	t.Helper()
	s := testStore(t)
	p, err := s.SaveProject(uploadProject())
	if err != nil {
		t.Fatal(err)
	}
	r := &testRuntime{}
	return &Engine{Store: s, Runtime: r}, r, p
}

func storeUpload(t *testing.T, s *Store, p Project, version string) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "version.txt"), []byte(version), 0600)
	archive := filepath.Join(t.TempDir(), "src.tar.gz")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err = upload.Pack(dir, []string{"version.txt"}, f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	hash, err := s.Uploads.Save(p.ID, archive, upload.DefaultLimits(upload.DefaultMaxBytes))
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func archivePath(s *Store, p Project, hash string) string {
	return filepath.Join(s.Root, "uploads", p.ID, hash+".tar.gz")
}

func TestUploadDeploysArchiveAndRestartsFromAFreshExtraction(t *testing.T) {
	e, r, p := uploadFixture(t)
	hash := storeUpload(t, e.Store, p, "first")
	j, err := e.Store.Enqueue(p.ID, "upload", "", hash, "")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := e.Store.Claim()
	if err != nil || claimed.ID != j.ID {
		t.Fatalf("claim: %v %+v", err, claimed)
	}
	leftover := filepath.Join(e.Store.Root, "releases", j.ID, "source")
	os.MkdirAll(leftover, 0700)
	os.WriteFile(filepath.Join(leftover, "partial"), []byte("x"), 0600)
	if err = e.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	j = stepOnce(t, e, j)
	logs, _ := e.Store.Logs(j.ID)
	if j.Phase != "succeeded" || j.Commit != hash || r.active != "first" || !strings.Contains(logs, "Upload "+hash) {
		t.Fatalf("job=%+v active=%s\n%s", j, r.active, logs)
	}
	if _, err := os.Stat(filepath.Join(leftover, "partial")); !os.IsNotExist(err) {
		t.Fatal("partial extraction reused")
	}
}

func TestUploadsRunWhilePausedAndManualDeployNeedsAnArchive(t *testing.T) {
	e, r, p := uploadFixture(t)
	first, _ := e.Store.Enqueue(p.ID, "upload", "", storeUpload(t, e.Store, p, "first"), "")
	if first = stepOnce(t, e, first); first.Phase != "succeeded" {
		t.Fatalf("first: %+v", first)
	}
	if err := e.Store.Pause(p.ID, false); err != nil {
		t.Fatal(err)
	}
	second, err := e.Store.Enqueue(p.ID, "upload", "", storeUpload(t, e.Store, p, "second"), "")
	if err != nil {
		t.Fatalf("paused project refused an upload: %v", err)
	}
	if second = stepOnce(t, e, second); second.Phase != "succeeded" || r.active != "second" {
		t.Fatalf("second: %+v active=%s", second, r.active)
	}
	manual, _ := e.Store.Enqueue(p.ID, "manual", "", "", "")
	if manual = stepOnce(t, e, manual); manual.Phase != "failed" || !strings.Contains(manual.Error, "upload a new version") {
		t.Fatalf("manual without archive: %+v", manual)
	}
}

func TestUploadRetentionKeepsReferencedArchivesOnly(t *testing.T) {
	e, _, p := uploadFixture(t)
	p.Retain = 1
	p, _ = e.Store.SaveProject(p)
	var hashes []string
	for _, version := range []string{"one", "two", "three"} {
		hash := storeUpload(t, e.Store, p, version)
		old := time.Now().Add(-2 * time.Hour)
		os.Chtimes(archivePath(e.Store, p, hash), old, old)
		j, _ := e.Store.Enqueue(p.ID, "upload", "", hash, "")
		if j = stepOnce(t, e, j); j.Phase != "succeeded" {
			t.Fatalf("%s: %+v", version, j)
		}
		hashes = append(hashes, hash)
	}
	exists := func(hash string) bool { _, err := os.Stat(archivePath(e.Store, p, hash)); return err == nil }
	if exists(hashes[0]) || !exists(hashes[1]) || !exists(hashes[2]) {
		t.Fatalf("retention: one=%v two=%v three=%v", exists(hashes[0]), exists(hashes[1]), exists(hashes[2]))
	}
}

func TestConfigChangeRebuildsTheActiveUpload(t *testing.T) {
	e, r, p := uploadFixture(t)
	hash := storeUpload(t, e.Store, p, "first")
	j, _ := e.Store.Enqueue(p.ID, "upload", "", hash, "")
	stepOnce(t, e, j)
	e.Store.Env.Set(p.ID, []envvars.Update{{Name: "COLOR", Value: "blue"}})
	change, err := e.Store.EnqueueConfigChange(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if change = stepOnce(t, e, change); change.Phase != "succeeded" || change.Commit != hash || len(r.env) != 1 {
		t.Fatalf("config change: %+v env=%v", change, r.env)
	}
}

func TestDeleteProjectRemovesUploads(t *testing.T) {
	e, _, p := uploadFixture(t)
	e.Runtime = &purgeRuntime{testRuntime: &testRuntime{}}
	j, _ := e.Store.Enqueue(p.ID, "upload", "", storeUpload(t, e.Store, p, "first"), "")
	stepOnce(t, e, j)
	if err := e.DeleteProject(context.Background(), p.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.Store.Root, "uploads", p.ID)); !os.IsNotExist(err) {
		t.Fatal("uploads remain after deletion")
	}
}

func TestBackupIncludesUploadsButNotTemporaryFiles(t *testing.T) {
	s := testStore(t)
	os.MkdirAll(filepath.Join(s.Root, "uploads", "p1"), 0700)
	os.WriteFile(filepath.Join(s.Root, "uploads", "p1", "x.tar.gz"), []byte("a"), 0600)
	os.MkdirAll(filepath.Join(s.Root, "uploads", "tmp"), 0700)
	os.WriteFile(filepath.Join(s.Root, "uploads", "tmp", "upload-1"), []byte("b"), 0600)
	var buf bytes.Buffer
	if err := Backup(s.Root, &buf); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[h.Name] = true
	}
	if !names["uploads/p1/x.tar.gz"] || names["uploads/tmp/upload-1"] {
		t.Fatalf("backup entries: %v", names)
	}
}
