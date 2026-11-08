package upload

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Store keeps normalised archives at root/<project>/<sha256>.tar.gz and
// in-flight files under root/tmp.
type Store struct{ root string }

// New returns a store rooted at root (the state's uploads directory).
func New(root string) *Store { return &Store{root: root} }

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidHash reports whether ref can name a stored archive.
func ValidHash(ref string) bool { return hashPattern.MatchString(ref) }

// TempDir returns the private directory for in-flight uploads.
func (s *Store) TempDir() (string, error) {
	dir := filepath.Join(s.root, "tmp")
	return dir, os.MkdirAll(dir, 0700)
}

func (s *Store) path(projectID, hash string) string {
	return filepath.Join(s.root, projectID, hash+".tar.gz")
}

// Save validates the archive, normalises it and stores it, returning the
// SHA-256 of the normalised archive. Nothing remains on disk on error.
func (s *Store) Save(projectID, archive string, lim Limits) (string, error) {
	if !validProjectID(projectID) {
		return "", errors.New("invalid project ID")
	}
	tmp, err := s.TempDir()
	if err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(tmp, "extract-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	tree := filepath.Join(work, "tree")
	if err = extract(archive, tree, lim); err != nil {
		return "", err
	}
	if tree, err = singleRoot(tree); err != nil {
		return "", err
	}
	if err = checkLinks(tree); err != nil {
		return "", err
	}
	files, err := listTree(tree)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", invalid("the archive is empty")
	}
	packed, err := os.CreateTemp(tmp, "packed-")
	if err != nil {
		return "", err
	}
	defer os.Remove(packed.Name())
	sum := sha256.New()
	err = Pack(tree, files, io.MultiWriter(packed, sum))
	if err == nil {
		err = packed.Sync()
	}
	if closeErr := packed.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	hash := hex.EncodeToString(sum.Sum(nil))
	if err = os.MkdirAll(filepath.Join(s.root, projectID), 0700); err != nil {
		return "", err
	}
	if err = os.Rename(packed.Name(), s.path(projectID, hash)); err != nil {
		return "", err
	}
	return hash, nil
}

// singleRoot descends into the only top-level directory, as produced by
// "compress folder" tools.
func singleRoot(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	if len(entries) == 1 && entries[0].IsDir() {
		return filepath.Join(dir, entries[0].Name()), nil
	}
	return dir, nil
}

// listTree returns every path below root, directories included, so empty
// directories survive normalisation.
func listTree(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, err
}

// Prepare extracts a stored archive into dest, replacing anything a previous
// interrupted attempt left there.
func (s *Store) Prepare(projectID, hash, dest string) error {
	if !validProjectID(projectID) {
		return errors.New("invalid project ID")
	}
	if !ValidHash(hash) {
		return errors.New("invalid upload identifier")
	}
	archive := s.path(projectID, hash)
	if _, err := os.Stat(archive); err != nil {
		return errors.New("upload archive unavailable; upload it again")
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := extract(archive, dest, unlimited); err != nil {
		return err
	}
	if err := checkLinks(dest); err != nil {
		os.RemoveAll(dest)
		return err
	}
	return nil
}

// Prune removes the project's archives that keep does not list, except those
// modified within minAge: their deployment may not be queued yet.
func (s *Store) Prune(projectID string, keep map[string]bool, minAge time.Duration) error {
	if !validProjectID(projectID) {
		return errors.New("invalid project ID")
	}
	paths, err := filepath.Glob(filepath.Join(s.root, projectID, "*.tar.gz"))
	if err != nil {
		return err
	}
	for _, p := range paths {
		hash := strings.TrimSuffix(filepath.Base(p), ".tar.gz")
		info, err := os.Stat(p)
		if err != nil || keep[hash] || time.Since(info.ModTime()) < minAge {
			continue
		}
		if err = os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// validProjectID rejects IDs that could name the temp directory or escape the
// store.
func validProjectID(id string) bool {
	return id != "" && id != "tmp" && !strings.ContainsAny(id, `/\.`)
}

// DeleteProject removes every archive of a project.
func (s *Store) DeleteProject(projectID string) error {
	if !validProjectID(projectID) {
		return errors.New("invalid project ID")
	}
	return os.RemoveAll(filepath.Join(s.root, projectID))
}

// SweepTemp removes in-flight files older than age, left by interrupted
// uploads or daemon restarts.
func (s *Store) SweepTemp(age time.Duration) error {
	dir, err := s.TempDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) <= age {
			continue
		}
		if err = os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
