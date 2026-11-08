package upload

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type entry struct {
	name, body, link string
	mode             int64
	kind             byte
}

func tgz(t *testing.T, entries ...entry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		kind, mode := e.kind, e.mode
		if kind == 0 {
			kind = tar.TypeReg
		}
		if mode == 0 {
			mode = 0644
		}
		h := &tar.Header{Name: e.name, Typeflag: kind, Mode: mode, Linkname: e.link, ModTime: time.Now()}
		if kind == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	f.Close()
	return path
}

func zipFile(t *testing.T, entries ...entry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := fs.FileMode(e.mode)
		if mode == 0 {
			mode = 0644
		}
		body := e.body
		switch e.kind {
		case tar.TypeSymlink:
			mode |= fs.ModeSymlink
			body = e.link
		case tar.TypeDir:
			mode |= fs.ModeDir
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	f.Close()
	return path
}

func plainFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	os.WriteFile(path, []byte(body), 0600)
	return path
}

func TestSaveNormalisesZipAndTarToTheSameHash(t *testing.T) {
	s := New(t.TempDir())
	entries := []entry{
		{name: "app/Dockerfile", body: "FROM scratch"},
		{name: "app/run.sh", body: "#!/bin/sh", mode: 0755},
		{name: "app/link", link: "run.sh", kind: tar.TypeSymlink},
		{name: "__MACOSX/app/._run.sh", body: "junk"},
		{name: "app/.DS_Store", body: "junk"},
	}
	fromTar, err := s.Save("p1", tgz(t, entries...), DefaultLimits(DefaultMaxBytes))
	if err != nil {
		t.Fatal(err)
	}
	fromZip, err := s.Save("p1", zipFile(t, entries...), DefaultLimits(DefaultMaxBytes))
	if err != nil {
		t.Fatal(err)
	}
	if fromTar != fromZip || !ValidHash(fromTar) {
		t.Fatalf("hashes differ: %s %s", fromTar, fromZip)
	}
	dest := filepath.Join(t.TempDir(), "src")
	if err = s.Prepare("p1", fromTar, dest); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "Dockerfile")); string(b) != "FROM scratch" {
		t.Fatalf("single root not flattened: %q", b)
	}
	if info, _ := os.Stat(filepath.Join(dest, "run.sh")); info.Mode().Perm() != 0755 {
		t.Fatalf("executable bit lost: %v", info.Mode())
	}
	if target, _ := os.Readlink(filepath.Join(dest, "link")); target != "run.sh" {
		t.Fatalf("symlink: %q", target)
	}
	for _, junk := range []string{"__MACOSX", ".DS_Store"} {
		if _, err := os.Stat(filepath.Join(dest, junk)); !os.IsNotExist(err) {
			t.Fatalf("%s extracted", junk)
		}
	}
	if stored, _ := os.Stat(filepath.Join(s.root, "p1", fromTar+".tar.gz")); stored.Mode().Perm() != 0600 {
		t.Fatalf("archive mode %v", stored.Mode())
	}
}

func TestSaveRejectsUnsafeArchivesAndLeavesNothing(t *testing.T) {
	lim := Limits{MaxExpanded: 1024, MaxEntries: 5}
	cases := map[string]string{
		"traversal":      tgz(t, entry{name: "../evil", body: "x"}),
		"absolute":       tgz(t, entry{name: "/etc/evil", body: "x"}),
		"backslash":      zipFile(t, entry{name: `..\evil`, body: "x"}),
		"escaping link":  tgz(t, entry{name: "link", link: "../../etc", kind: tar.TypeSymlink}),
		"absolute link":  tgz(t, entry{name: "link", link: "/etc/passwd", kind: tar.TypeSymlink}),
		"through link":   tgz(t, entry{name: "a/", kind: tar.TypeDir}, entry{name: "a/up", link: "..", kind: tar.TypeSymlink}, entry{name: "a/up/x", body: "x"}),
		"hardlink":       tgz(t, entry{name: "f", body: "x"}, entry{name: "h", link: "f", kind: tar.TypeLink}),
		"fifo":           tgz(t, entry{name: "pipe", kind: tar.TypeFifo}),
		"duplicate":      tgz(t, entry{name: "f", body: "x"}, entry{name: "f", body: "y"}),
		"too large":      tgz(t, entry{name: "big", body: strings.Repeat("x", 2048)}),
		"too many":       tgz(t, entry{name: "1", body: "x"}, entry{name: "2", body: "x"}, entry{name: "3", body: "x"}, entry{name: "4", body: "x"}, entry{name: "5", body: "x"}, entry{name: "6", body: "x"}),
		"empty":          tgz(t),
		"not an archive": plainFile(t, "plain text"),
	}
	for name, archive := range cases {
		t.Run(name, func(t *testing.T) {
			s := New(t.TempDir())
			_, err := s.Save("p1", archive, lim)
			var invalid *Error
			if !errors.As(err, &invalid) {
				t.Fatalf("expected *upload.Error, got %v", err)
			}
			if left, _ := filepath.Glob(filepath.Join(s.root, "*", "*")); len(left) != 0 {
				t.Fatalf("files left behind: %v", left)
			}
		})
	}
}

func TestPrepareRetentionAndDeletion(t *testing.T) {
	s := New(t.TempDir())
	a, err := s.Save("p1", tgz(t, entry{name: "v", body: "a"}), DefaultLimits(DefaultMaxBytes))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Save("p1", tgz(t, entry{name: "v", body: "b"}), DefaultLimits(DefaultMaxBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Prepare("p1", "not-a-hash", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("invalid hash accepted")
	}
	if err := s.Prune("p1", map[string]bool{a: true}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.Prepare("p1", b, filepath.Join(t.TempDir(), "x")); err != nil {
		t.Fatal("recent archive was pruned")
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, h := range []string{a, b} {
		os.Chtimes(filepath.Join(s.root, "p1", h+".tar.gz"), old, old)
	}
	if err := s.Prune("p1", map[string]bool{a: true}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.Prepare("p1", a, filepath.Join(t.TempDir(), "x")); err != nil {
		t.Fatal("kept archive was pruned")
	}
	if err := s.Prepare("p1", b, filepath.Join(t.TempDir(), "x")); err == nil || !strings.Contains(err.Error(), "upload it again") {
		t.Fatalf("pruned archive: %v", err)
	}
	if err := s.DeleteProject("p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.root, "p1")); !os.IsNotExist(err) {
		t.Fatal("project archives remain")
	}
	if err := s.DeleteProject(""); err == nil {
		t.Fatal("empty project ID accepted")
	}
}

func TestSweepTempRemovesOnlyOldFiles(t *testing.T) {
	s := New(t.TempDir())
	dir, err := s.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "old"), []byte("x"), 0600)
	os.WriteFile(filepath.Join(dir, "new"), []byte("x"), 0600)
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(filepath.Join(dir, "old"), old, old)
	if err := s.SweepTemp(24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old")); !os.IsNotExist(err) {
		t.Fatal("old temp file kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "new")); err != nil {
		t.Fatal("recent temp file removed")
	}
}

func TestPackIsDeterministicAndRejectsOutsideSymlinks(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a"), []byte("x"), 0600)
	var one, two bytes.Buffer
	if err := Pack(root, []string{"a"}, &one); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(root, "a"), later, later)
	if err := Pack(root, []string{"a"}, &two); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one.Bytes(), two.Bytes()) {
		t.Fatal("pack depends on modification times")
	}
	os.Symlink("../outside", filepath.Join(root, "bad"))
	if err := Pack(root, []string{"a", "bad"}, io.Discard); err == nil {
		t.Fatal("escaping symlink packed")
	}
}
