package deploy

import (
	"bytes"
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
	ciMu     sync.Mutex
	checkMu  sync.Mutex
	// mu serialises deployment steps with project deletion.
	mu sync.Mutex
}

func (e *Engine) Step(ctx context.Context) (bool, error) {
	if err := e.recheckCI(ctx); err != nil {
		return false, err
	}
	return e.step(ctx)
}

func (e *Engine) step(ctx context.Context) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
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
	e.forgetCICheck(j.ID)
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
		return true, e.restore(recoveryCtx, latest, log.Redact(err.Error()), log)
	}
	if parent.Err() != nil {
		return true, parent.Err()
	}
	if ctx.Err() != nil {
		err = errors.New("deployment timeout exceeded")
	}
	fmt.Fprintf(log, "Failed: %s\n", err)
	return true, e.Store.Finish(j.ID, "failed", log.Redact(err.Error()), "")
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
		maskManifest(log, j.Manifest)
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
				if _, err = e.Store.EnqueueObserved(j.Project, "poll", "", head); err != nil {
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
		if w, ok := log.(interface{ Mask(...string) }); ok {
			for _, v := range vars {
				w.Mask(v.Value)
			}
		}
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
	maskManifest(log, j.Manifest)
	if j.Previous != "" {
		if previous, err := e.Store.Job(j.Previous); err == nil {
			maskManifest(log, previous.Manifest)
		}
	}
	if w, ok := log.(interface{ Redact(string) string }); ok {
		message = w.Redact(message)
	}
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
	child, cancel := context.WithCancel(ctx)
	checkDone := make(chan struct{})
	checkErrors := make(chan error, 1)
	go func() {
		defer close(checkDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			if err := e.recheckCI(child); err != nil {
				if child.Err() == nil {
					checkErrors <- err
				}
				return
			}
			select {
			case <-child.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	defer func() { cancel(); <-checkDone }()
	for child.Err() == nil {
		select {
		case err := <-checkErrors:
			return err
		default:
		}
		did, err := e.step(child)
		if err != nil {
			return err
		}
		if !did {
			select {
			case <-child.Done():
				return child.Err()
			case err := <-checkErrors:
				return err
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	return child.Err()
}

func (e *Engine) prune(project string) error {
	unlock, err := lockFile(e.Store.Root, "artifacts.lock", false)
	if err != nil {
		return err
	}
	defer unlock()
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
	mu         sync.Mutex
	file       *os.File
	openErr    error
	pending    string
	discarding bool
	masks      []string
	replacer   *strings.Replacer
	dirty      bool
	size       int64
	closed     bool
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
	w := &logWriter{}
	if !namePattern.MatchString(id) {
		w.openErr = errors.New("invalid job id")
		return w
	}
	w.file, w.openErr = os.OpenFile(filepath.Join(s.Root, "logs", id+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if w.openErr == nil {
		if info, err := w.file.Stat(); err == nil {
			w.size = info.Size()
		} else {
			w.openErr = err
			w.file.Close()
			w.file = nil
		}
	}
	if j, err := s.Job(id); err == nil {
		entries, _ := os.ReadDir(filepath.Join(s.Root, "secrets"))
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasPrefix(entry.Name(), j.ProjectID+"-") {
				if b, err := os.ReadFile(filepath.Join(s.Root, "secrets", entry.Name())); err == nil {
					w.Mask(string(b))
				}
			}
		}
		if vars, err := s.Env.List(j.ProjectID); err == nil {
			for _, v := range vars {
				w.Mask(v.Value)
			}
		}
	}
	return w
}

func (w *logWriter) Mask(values ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, value := range values {
		w.masks = appendMasks(w.masks, value)
	}
	w.dirty = true
}
func (w *logWriter) redact(s string) string {
	if w.dirty {
		sort.Slice(w.masks, func(i, j int) bool { return len(w.masks[i]) > len(w.masks[j]) })
		seen := make(map[string]bool, len(w.masks))
		unique := w.masks[:0]
		pairs := make([]string, 0, len(w.masks)*2)
		for _, mask := range w.masks {
			if !seen[mask] {
				seen[mask] = true
				unique = append(unique, mask)
				pairs = append(pairs, mask, "[REDACTED]")
			}
		}
		w.masks = unique
		w.replacer = strings.NewReplacer(pairs...)
		w.dirty = false
	}
	if w.replacer != nil {
		return w.replacer.Replace(s)
	}
	return s
}
func (w *logWriter) Redact(s string) string { w.mu.Lock(); defer w.mu.Unlock(); return w.redact(s) }
func (w *logWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	if w.openErr != nil {
		return 0, w.openErr
	}
	n := len(b)
	for len(b) > 0 {
		end := bytes.IndexByte(b, '\n')
		if end < 0 {
			if !w.discarding {
				if len(w.pending)+len(b) > 65536 {
					w.pending = ""
					w.discarding = true
				} else {
					w.pending += string(b)
				}
			}
			break
		}
		line := "[oversized log line omitted]"
		if !w.discarding && len(w.pending)+end <= 65536 {
			line = w.pending + string(b[:end])
		}
		w.pending = ""
		w.discarding = false
		if err := w.append(line + "\n"); err != nil {
			return 0, err
		}
		b = b[end+1:]
	}
	return n, nil
}
func (w *logWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	err := w.openErr
	if w.discarding {
		w.pending = "[oversized log line omitted]"
	}
	if err == nil && w.pending != "" {
		err = w.append(w.pending + "\n")
		w.pending = ""
	}
	if w.file != nil {
		if closeErr := w.file.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}
func (w *logWriter) append(s string) error {
	s = w.redact(s)
	const limit = 1 << 20
	if w.size >= limit {
		return nil
	}
	if int64(len(s)) > limit-w.size {
		s = s[:limit-w.size]
	}
	n, err := io.WriteString(w.file, s)
	w.size += int64(n)
	return err
}

func (s *Store) ReadLogs(id string, offset int64) (string, int64, error) {
	if !namePattern.MatchString(id) || offset < 0 {
		return "", 0, errors.New("invalid log request")
	}
	f, err := os.Open(filepath.Join(s.Root, "logs", id+".log"))
	if os.IsNotExist(err) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if offset > info.Size() {
		offset = 0
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return "", 0, err
	}
	b, err := io.ReadAll(f)
	return string(b), offset + int64(len(b)), err
}
func (s *Store) Logs(id string) (string, error) {
	if _, err := s.Job(id); err != nil {
		return "", err
	}
	text, _, err := s.ReadLogs(id, 0)
	return text, err
}

func maskManifest(log io.Writer, path string) {
	w, ok := log.(interface{ Mask(...string) })
	if !ok || path == "" {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var cfg map[string]any
	if json.Unmarshal(b, &cfg) != nil {
		return
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, value := range x {
				if k == "environment" || k == "args" {
					maskValues(w, value)
				} else {
					walk(value)
				}
			}
		case []any:
			for _, value := range x {
				walk(value)
			}
		}
	}
	walk(cfg)
	for _, kind := range []string{"secrets", "configs"} {
		entries, _ := cfg[kind].(map[string]any)
		for _, entry := range entries {
			m, _ := entry.(map[string]any)
			file, _ := m["file"].(string)
			rel, err := filepath.Rel(filepath.Dir(path), file)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				if b, err := os.ReadFile(file); err == nil {
					w.Mask(string(b))
				}
			}
		}
	}
}
func maskValues(w interface{ Mask(...string) }, v any) {
	switch x := v.(type) {
	case string:
		if _, value, ok := strings.Cut(x, "="); ok {
			w.Mask(value, strings.ReplaceAll(value, "$$", "$"))
		} else {
			w.Mask(x, strings.ReplaceAll(x, "$$", "$"))
		}
	case map[string]any:
		for _, value := range x {
			if s, ok := value.(string); ok {
				w.Mask(s, strings.ReplaceAll(s, "$$", "$"))
			}
		}
	case []any:
		for _, value := range x {
			maskValues(w, value)
		}
	}
}
