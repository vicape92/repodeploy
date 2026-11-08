package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vicape92/repodeploy/internal/envvars"
)

type Runtime interface {
	Build(context.Context, Project, string, string, io.Writer) (string, error)
	Activate(context.Context, Project, string, io.Writer) error
	Healthy(context.Context, Project, string, io.Writer) error
	Remove(context.Context, Project, string, io.Writer) error
	Forget(context.Context, Project, string, io.Writer) error
}

type Engine struct {
	Store    *Store
	Runtime  Runtime
	Timeout  time.Duration
	ciChecks map[string]ciCheck
	// mu serialises deployment steps with project deletion.
	mu sync.Mutex
}

func (e *Engine) Step(ctx context.Context) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.recheckCI(ctx); err != nil {
		return false, err
	}
	j, err := e.Store.Claim()
	if errors.Is(err, ErrNoJob) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	log := e.Store.LogWriter(j.ID)
	defer log.Close()
	defer func() {
		if err := e.prune(j.ProjectID); err != nil {
			fmt.Fprintln(log, "Retention cleanup failed; will retry after the next deployment")
		}
	}()
	fmt.Fprintf(log, "Preparing deployment %s\n", j.ID)
	parent := ctx
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err = e.execute(ctx, &j, log)
	if errors.Is(err, errWaitingCI) {
		return true, nil
	}
	delete(e.ciChecks, j.ID)
	if err == nil {
		return true, nil
	}
	latest, loadErr := e.Store.Job(j.ID)
	if loadErr != nil {
		return true, loadErr
	}
	if latest.Phase == "activating" || latest.Phase == "checking" {
		recoveryCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		return true, e.restore(recoveryCtx, latest, err.Error(), log)
	}
	if parent.Err() != nil {
		return true, parent.Err()
	}
	if ctx.Err() != nil {
		err = errors.New("deployment timeout exceeded")
	}
	fmt.Fprintf(log, "Failed: %s\n", err)
	return true, e.Store.Finish(j.ID, "failed", err.Error(), "")
}

func (e *Engine) execute(ctx context.Context, j *Job, log io.Writer) error {
	dir := filepath.Join(e.Store.Root, "releases", j.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	current, err := e.Store.Project(j.ProjectID)
	if err != nil {
		return err
	}
	previous := current.Active
	if j.Rollback != "" {
		target, err := e.Store.Job(j.Rollback)
		if err != nil {
			return errors.New("rollback release not found")
		}
		if target.ProjectID != j.ProjectID || target.Phase != "succeeded" || target.Manifest == "" {
			return errors.New("rollback target is not a successful release of this project")
		}
		if err = e.Store.Pause(j.ProjectID, false); err != nil {
			return err
		}
		j.Manifest, err = copyArtifact(target.Manifest, dir)
		if err != nil {
			return errors.New("rollback artifact unavailable")
		}
		j.Commit = target.Commit
		j.Project = target.Project
		if err = e.Store.SetSnapshot(j.ID, j.Project); err != nil {
			return err
		}
		if err = e.Store.SetPhase(j.ID, "building", j.Commit); err != nil {
			return err
		}
	} else {
		if !userSource(j.Source) {
			head, err := (Git{Sources: e.Store.Sources}).Resolve(ctx, j.Project)
			if err != nil {
				return err
			}
			if head != j.Commit {
				if _, err = e.Store.Enqueue(j.ProjectID, "poll", "", head, ""); err != nil {
					return err
				}
				return errors.New("automatic target superseded by the current branch head; latest target reconciled")
			}
		}
		if j.Commit == "" {
			if j.Project.IsUpload() {
				return errors.New("upload projects deploy uploaded archives; upload a new version")
			}
			j.Commit, err = (Git{Sources: e.Store.Sources}).Resolve(ctx, j.Project)
			if err != nil {
				return err
			}
		}
		if err = e.Store.SetPhase(j.ID, "preparing", j.Commit); err != nil {
			return err
		}
		if j.Project.IsUpload() {
			fmt.Fprintf(log, "Upload %s\n", j.Commit)
		} else {
			fmt.Fprintf(log, "Commit %s\n", j.Commit)
		}
		if j.Project.WaitCI {
			if err = e.waitCI(ctx, *j); err != nil {
				if errors.Is(err, errWaitingCI) {
					fmt.Fprintln(log, "Waiting for CI/CD results")
				}
				return err
			}
		}
		checkout := filepath.Join(dir, "source")
		if err = e.sourceFor(j.Project).Prepare(ctx, j.Project, j.Commit, checkout); err != nil {
			return err
		}
		if err = e.Store.SetPhase(j.ID, "building", j.Commit); err != nil {
			return err
		}
		vars, err := e.Store.Env.List(j.ProjectID)
		if err != nil {
			return err
		}
		j.Project.Env, j.Project.EnvDigest = vars, envvars.Digest(vars)
		if err = e.Store.SetSnapshot(j.ID, j.Project); err != nil {
			return err
		}
		fmt.Fprintln(log, "Building; the active release is unchanged")
		j.Manifest, err = e.Runtime.Build(ctx, j.Project, checkout, dir, log)
		if err != nil {
			return err
		}
	}
	if err = e.Store.SetArtifact(j.ID, j.Manifest, previous); err != nil {
		return err
	}
	if err = e.Store.SetPhase(j.ID, "activating", j.Commit); err != nil {
		return err
	}
	fmt.Fprintln(log, "Activating")
	if err = e.Runtime.Activate(ctx, j.Project, j.Manifest, log); err != nil {
		return err
	}
	if err = e.Store.SetPhase(j.ID, "checking", j.Commit); err != nil {
		return err
	}
	fmt.Fprintln(log, "Checking health")
	if err = e.Runtime.Healthy(ctx, j.Project, j.Manifest, log); err != nil {
		return err
	}
	if err = e.Store.SetActive(j.ProjectID, j.ID); err != nil {
		return err
	}
	if err = e.Store.Finish(j.ID, "succeeded", "", ""); err != nil {
		return err
	}
	fmt.Fprintln(log, "Deployment succeeded")
	return nil
}

func copyArtifact(source, dir string) (string, error) {
	b, err := os.ReadFile(source)
	if err != nil {
		return "", err
	}
	var cfg map[string]any
	if json.Unmarshal(b, &cfg) == nil {
		for _, kind := range []string{"secrets", "configs"} {
			resources, _ := cfg[kind].(map[string]any)
			for name, v := range resources {
				m, ok := v.(map[string]any)
				if !ok || !namePattern.MatchString(name) {
					return "", errors.New("invalid retained resource")
				}
				path, ok := m["file"].(string)
				if !ok {
					return "", errors.New("invalid retained resource file")
				}
				value, err := os.ReadFile(path)
				if err != nil {
					return "", err
				}
				target := filepath.Join(dir, "secrets", name)
				if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					return "", err
				}
				if err = os.WriteFile(target, value, 0600); err != nil {
					return "", err
				}
				m["file"] = target
			}
		}
		b, err = json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return "", err
		}
	}
	target := filepath.Join(dir, "compose.json")
	return target, os.WriteFile(target, b, 0600)
}

func (e *Engine) restore(ctx context.Context, j Job, message string, log io.Writer) error {
	fmt.Fprintf(log, "Failed: %s\nRecovering previous release\n", message)
	result := "removed"
	var recoveryErr error
	if j.Previous != "" {
		previous, err := e.Store.Job(j.Previous)
		if err == nil {
			err = e.Runtime.Activate(ctx, previous.Project, previous.Manifest, log)
		}
		if err == nil {
			err = e.Runtime.Healthy(ctx, previous.Project, previous.Manifest, log)
		}
		recoveryErr = err
		result = "restored"
	} else {
		recoveryErr = e.Runtime.Remove(ctx, j.Project, j.Manifest, log)
	}
	if recoveryErr != nil {
		result = "failed"
		fmt.Fprintln(log, "Recovery failed; project is blocked")
		if err := e.Store.SetActive(j.ProjectID, ""); err != nil {
			return err
		}
		if err := e.Store.Pause(j.ProjectID, true); err != nil {
			return err
		}
	} else if err := e.Store.SetActive(j.ProjectID, j.Previous); err != nil {
		return err
	}
	return e.Store.Finish(j.ID, "failed", message, result)
}

func (e *Engine) Recover(ctx context.Context) error {
	jobs, err := e.Store.Interrupted()
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.Phase == "activating" || j.Phase == "checking" {
			log := e.Store.LogWriter(j.ID)
			err = e.restore(ctx, j, "engine restarted during activation", log)
			log.Close()
		} else {
			err = e.Store.Requeue(j.ID)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) Run(ctx context.Context) error {
	e.mu.Lock()
	err := e.Recover(ctx)
	e.mu.Unlock()
	if err != nil {
		return err
	}
	for ctx.Err() == nil {
		did, err := e.Step(ctx)
		if err != nil {
			return err
		}
		if !did {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	return ctx.Err()
}

func (e *Engine) prune(project string) error {
	p, err := e.Store.Project(project)
	if err != nil {
		return err
	}
	jobs, err := e.Store.Jobs(project)
	if err != nil {
		return err
	}
	protected := map[string]bool{p.Active: true}
	if active, err := e.Store.Job(p.Active); err == nil {
		protected[active.Previous] = true
	}
	for _, j := range jobs {
		if j.Phase != "succeeded" && j.Phase != "failed" {
			protected[j.ID] = true
			protected[j.Previous] = true
			protected[j.Rollback] = true
		}
		if p.Blocked && (j.Recovery == "failed" || j.Rollback != "") {
			protected[j.ID] = true
			protected[j.Previous] = true
			protected[j.Rollback] = true
		}
	}
	kept := 0
	for _, j := range jobs {
		if j.Phase != "succeeded" && j.Phase != "failed" {
			protected[j.ID] = true
			protected[j.Previous] = true
			continue
		}
		if kept < p.Retain || protected[j.ID] {
			kept++
			continue
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err = e.Runtime.Forget(cleanupCtx, p, filepath.Join(e.Store.Root, "releases", j.ID), io.Discard)
		cancel()
		if err != nil {
			return err
		}
		if err = os.RemoveAll(filepath.Join(e.Store.Root, "releases", j.ID)); err != nil {
			return err
		}
		if err = os.Remove(filepath.Join(e.Store.Root, "logs", j.ID+".log")); err != nil && !os.IsNotExist(err) {
			return err
		}
		// Delivery tombstones intentionally outlive visible history.
		if _, err = e.Store.db.Exec(`UPDATE jobs SET retained=0,snapshot='{}',manifest='',error='' WHERE id=?`, j.ID); err != nil {
			return err
		}
	}
	if p.IsUpload() {
		return e.Store.pruneUploads(p.ID)
	}
	return nil
}

type logWriter struct {
	store   *Store
	id      string
	pending string
	masks   []string
}

const (
	minWholeMask    = 4
	minFragmentMask = 8
)

// appendMasks adds the strings to redact for one secret text. Very short
// fragments ("1", "true", "app") would redact ordinary log text. The whole
// text is masked from minWholeMask characters; individual lines (the log
// writer masks line by line, so multi-line values need them) and KEY=value
// values from minFragmentMask, since config files contain many common lines.
func appendMasks(masks []string, text string) []string {
	if whole := strings.TrimSpace(text); len(whole) >= minWholeMask {
		masks = append(masks, whole)
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if len(line) >= minFragmentMask {
			masks = append(masks, line)
		}
		if _, value, ok := strings.Cut(line, "="); ok && len(value) >= minFragmentMask {
			masks = append(masks, value)
		}
	}
	return masks
}

func (s *Store) LogWriter(id string) *logWriter {
	masks := []string{}
	entries, _ := os.ReadDir(filepath.Join(s.Root, "secrets"))
	for _, entry := range entries {
		if !entry.IsDir() && !envvars.IsStoreFile(entry.Name()) {
			if b, err := os.ReadFile(filepath.Join(s.Root, "secrets", entry.Name())); err == nil && len(b) > 0 {
				masks = appendMasks(masks, string(b))
			}
		}
	}
	for _, value := range s.Env.Values() {
		masks = appendMasks(masks, value)
	}
	sort.Slice(masks, func(i, j int) bool { return len(masks[i]) > len(masks[j]) })
	return &logWriter{store: s, id: id, masks: masks}
}
func (w *logWriter) Write(b []byte) (int, error) {
	w.pending += string(b)
	for {
		line, rest, ok := strings.Cut(w.pending, "\n")
		if !ok {
			break
		}
		w.pending = rest
		if err := w.append(line + "\n"); err != nil {
			return 0, err
		}
	}
	if len(w.pending) > 65536 {
		w.pending = "[oversized log line omitted]"
	}
	return len(b), nil
}
func (w *logWriter) Close() error {
	if w.pending != "" {
		err := w.append(w.pending + "\n")
		w.pending = ""
		return err
	}
	return nil
}
func (w *logWriter) append(s string) error {
	for _, mask := range w.masks {
		if mask != "" {
			s = strings.ReplaceAll(s, mask, "[REDACTED]")
		}
	}
	w.store.logMu.Lock()
	defer w.store.logMu.Unlock()
	f, err := os.OpenFile(filepath.Join(w.store.Root, "logs", w.id+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	const limit = 1 << 20
	if stat.Size() >= limit {
		return nil
	}
	if int64(len(s)) > limit-stat.Size() {
		s = s[:limit-stat.Size()]
	}
	_, err = io.WriteString(f, s)
	return err
}

func (s *Store) Logs(id string) (string, error) {
	if _, err := s.Job(id); err != nil {
		return "", err
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	b, err := os.ReadFile(filepath.Join(s.Root, "logs", id+".log"))
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(b), err
}
