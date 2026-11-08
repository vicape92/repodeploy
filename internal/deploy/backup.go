package deploy

import (
	"archive/tar"
	"compress/gzip"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Backup writes a gzip-compressed tar archive of the state directory that is
// consistent even while the daemon runs: the database is copied with
// VACUUM INTO, and Git checkouts (reproducible, often large) are skipped.
// The archive contains credentials and secrets; protect it accordingly.
func Backup(root string, w io.Writer) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	release, err := lockFile(root, "artifacts.lock", false)
	if err != nil {
		return err
	}
	defer release()
	dbPath := filepath.Join(root, "state.db")
	if _, err = os.Stat(dbPath); err != nil {
		return errors.New("no state database found; check --state")
	}
	snapshot := filepath.Join(root, ".backup-"+newID()+".db")
	defer os.Remove(snapshot)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA busy_timeout=5000`)
	if err == nil {
		_, err = db.Exec(`VACUUM INTO ?`, snapshot)
	}
	db.Close()
	if err != nil {
		return fmt.Errorf("cannot snapshot the database: %w", err)
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if err = addFile(tw, snapshot, "state.db"); err != nil {
		return err
	}
	releaseCredentials, err := lockFile(root, "credentials.lock", false)
	if err != nil {
		return err
	}
	for _, name := range []string{"admin.json", "admin.token"} {
		if err = addFile(tw, filepath.Join(root, name), name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			releaseCredentials()
			return err
		}
	}
	releaseCredentials()
	if err = addFile(tw, filepath.Join(root, "connections.json"), "connections.json"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if rel == "." {
			return nil
		}
		switch parts[0] {
		case "secrets", "ssh", "logs":
		case "uploads":
			// Archives let "deploy changes" rebuild after a restore; in-flight
			// files are not worth keeping.
			if len(parts) >= 2 && parts[1] == "tmp" {
				return filepath.SkipDir
			}
		case "releases":
			// Keep manifests and secret snapshots needed for rollback.
			if len(parts) >= 3 && parts[2] == "source" {
				return filepath.SkipDir
			}
		default:
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		return addFile(tw, path, filepath.ToSlash(rel))
	})
	if err != nil {
		return err
	}
	if err = tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func addFile(tw *tar.Writer, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = name
	header.Mode = int64(info.Mode().Perm())
	header.Uname, header.Gname = "", ""
	if err = tw.WriteHeader(header); err != nil {
		return err
	}
	_, err = io.CopyN(tw, f, info.Size())
	return err
}
