package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrBusy reports that a deployment step is running and owns the engine.
var ErrBusy = errors.New("a deployment is in progress; retry when it finishes")

// Purger removes everything a runtime created for a project.
type Purger interface {
	Purge(ctx context.Context, p Project, manifest string, volumes bool, log io.Writer) error
}

// DeleteProject stops and removes a project's containers, built images,
// releases, logs, managed secrets and history. Named volumes (application
// data) are removed only when volumes is true. It refuses to run while a
// deployment step holds the engine, so it never races an activation.
func (e *Engine) DeleteProject(ctx context.Context, id string, volumes bool) error {
	if !e.mu.TryLock() {
		return ErrBusy
	}
	defer e.mu.Unlock()
	p, err := e.Store.Project(id)
	if err != nil {
		return errors.New("project not found")
	}
	purger, ok := e.Runtime.(Purger)
	if !ok {
		return errors.New("this runtime cannot delete projects")
	}
	manifest := ""
	if p.Active != "" {
		if active, err := e.Store.Job(p.Active); err == nil {
			manifest = active.Manifest
		}
	}
	var output bytes.Buffer
	if err = purger.Purge(ctx, p, manifest, volumes, &output); err != nil {
		return fmt.Errorf("cannot remove the project's containers: %s", lastLine(output.String(), err))
	}
	ids, err := e.Store.jobIDs(id)
	if err != nil {
		return err
	}
	for _, jobID := range ids {
		dir := filepath.Join(e.Store.Root, "releases", jobID)
		if err = e.Runtime.Forget(ctx, p, dir, io.Discard); err != nil {
			return fmt.Errorf("cannot remove built image for release %s: %w", jobID, err)
		}
		if err = os.RemoveAll(dir); err != nil {
			return err
		}
		if err = os.Remove(filepath.Join(e.Store.Root, "logs", jobID+".log")); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	secrets, _ := filepath.Glob(filepath.Join(e.Store.Root, "secrets", p.ID+"-*"))
	secrets = append(secrets, filepath.Join(e.Store.Root, "secrets", p.ID+".webhook"))
	for _, path := range secrets {
		if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err = e.Store.Env.DeleteProject(p.ID); err != nil {
		return err
	}
	if err = e.Store.Uploads.DeleteProject(p.ID); err != nil {
		return err
	}
	return e.Store.deleteProjectRows(id)
}

func lastLine(output string, err error) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if line := strings.TrimSpace(lines[len(lines)-1]); line != "" {
		return line
	}
	return err.Error()
}

func (s *Store) jobIDs(project string) ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM jobs WHERE project_id=?`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) deleteProjectRows(project string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{`DELETE FROM deliveries WHERE project_id=?`, `DELETE FROM observed WHERE project_id=?`, `DELETE FROM jobs WHERE project_id=?`, `DELETE FROM projects WHERE id=?`} {
		if _, err = tx.Exec(statement, project); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Purge stops the project's Compose application and removes leftovers that
// carry its labels. Volume names are prefixed with the project ID, so the
// optional volume sweep cannot touch another project's data.
func (d Docker) Purge(ctx context.Context, p Project, manifest string, volumes bool, log io.Writer) error {
	if manifest != "" {
		if _, err := os.Stat(manifest); err == nil {
			args := []string{"down", "--remove-orphans", "--timeout", "10"}
			if volumes {
				args = append(args, "--volumes")
			}
			if err = d.compose(ctx, p, manifest, log, args...); err != nil {
				return err
			}
		}
	}
	containers, err := dockerOutput(ctx, "ps", "--all", "--quiet", "--filter", "label=io.pushvori.project="+p.ID)
	if err != nil {
		return err
	}
	if ids := strings.Fields(string(containers)); len(ids) > 0 {
		if err = dockerRun(ctx, log, append([]string{"rm", "--force"}, ids...)...); err != nil {
			return err
		}
	}
	networks, err := dockerOutput(ctx, "network", "ls", "--quiet", "--filter", "label=com.docker.compose.project=pv-"+p.ID)
	if err == nil {
		for _, id := range strings.Fields(string(networks)) {
			dockerRun(ctx, log, "network", "rm", id)
		}
	}
	if !volumes {
		return nil
	}
	names, err := dockerOutput(ctx, "volume", "ls", "--quiet", "--filter", "name=pv-"+p.ID+"-")
	if err != nil {
		return err
	}
	for _, name := range strings.Fields(string(names)) {
		if !strings.HasPrefix(name, "pv-"+p.ID+"-") {
			continue
		}
		if err = dockerRun(ctx, log, "volume", "rm", name); err != nil {
			return err
		}
	}
	return nil
}
