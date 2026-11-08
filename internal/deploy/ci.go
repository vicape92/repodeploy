package deploy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

var errWaitingCI = errors.New("waiting for CI/CD results")

type ciCheck struct {
	at    time.Time
	state string
}

func (e *Engine) ciState(ctx context.Context, j Job) (string, error) {
	started, err := time.Parse(time.RFC3339Nano, j.Started)
	if err != nil {
		return "", errors.New("invalid CI/CD start time")
	}
	timeout := j.Project.CITimeout
	if timeout <= 0 {
		timeout = 1800
	}
	if time.Since(started).Seconds() >= float64(timeout) {
		return "", errors.New("CI/CD wait timeout exceeded")
	}
	if last, ok := e.cachedCICheck(j.ID); ok && time.Since(last.at) < 10*time.Second {
		return last.state, nil
	}
	provider := j.Project.CIProvider
	if provider == "" {
		provider = j.Project.Connection
	}
	if provider == "" || j.Project.RepositoryID == "" || e.Store.Sources == nil {
		return "", errors.New("CI/CD requires a connected provider and repository")
	}
	checked := time.Now()
	state, err := e.Store.Sources.CIState(ctx, provider, j.Project.RepositoryID, j.Commit)
	if err != nil {
		return "", fmt.Errorf("cannot read CI/CD results: %w", err)
	}
	e.ciMu.Lock()
	if e.ciChecks == nil {
		e.ciChecks = make(map[string]ciCheck)
	}
	e.ciChecks[j.ID] = ciCheck{at: checked, state: state}
	e.ciMu.Unlock()
	return state, nil
}

func (e *Engine) waitCI(ctx context.Context, j Job) error {
	state, err := e.ciState(ctx, j)
	if err != nil {
		return err
	}
	switch state {
	case "passed":
		return nil
	case "failed":
		return errors.New("CI/CD failed for the selected commit")
	default:
		if err := e.Store.SetPhase(j.ID, "waiting_ci", j.Commit); err != nil {
			return err
		}
		return errWaitingCI
	}
}

func (e *Engine) cachedCICheck(id string) (ciCheck, bool) {
	e.ciMu.Lock()
	defer e.ciMu.Unlock()
	check, ok := e.ciChecks[id]
	return check, ok
}

func (e *Engine) forgetCICheck(id string) {
	e.ciMu.Lock()
	defer e.ciMu.Unlock()
	delete(e.ciChecks, id)
}

func (e *Engine) recheckCI(ctx context.Context) error {
	e.checkMu.Lock()
	defer e.checkMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	rows, err := e.Store.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE phase='waiting_ci' ORDER BY seq`)
	if err != nil {
		return err
	}
	var jobs []Job
	for rows.Next() {
		j, scanErr := scanJob(rows)
		if scanErr != nil {
			rows.Close()
			return scanErr
		}
		jobs = append(jobs, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var workers sync.WaitGroup
	slots := make(chan struct{}, 4)
	results := make(chan error, len(jobs))
	for _, j := range jobs {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			workers.Wait()
			return ctx.Err()
		}
		workers.Add(1)
		go func(j Job) {
			defer workers.Done()
			defer func() { <-slots }()
			results <- e.recheckJob(ctx, j)
		}(j)
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) recheckJob(ctx context.Context, j Job) error {
	if last, ok := e.cachedCICheck(j.ID); ok && time.Since(last.at) < 10*time.Second {
		return nil
	}
	current, err := e.Store.Project(j.ProjectID)
	if errors.Is(err, sql.ErrNoRows) {
		e.forgetCICheck(j.ID)
		return nil
	}
	if err != nil {
		return err
	}
	if current.Blocked || (current.Paused && j.Source != "manual") {
		return nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var head, state string
	if j.Source != "manual" {
		head, err = (Git{Sources: e.Store.Sources}).Resolve(checkCtx, j.Project)
	}
	if err == nil && (head == "" || head == j.Commit) {
		state, err = e.ciState(checkCtx, j)
		if state == "failed" && err == nil {
			err = errors.New("CI/CD failed for the selected commit")
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	latest, loadErr := e.Store.Job(j.ID)
	if errors.Is(loadErr, sql.ErrNoRows) {
		e.forgetCICheck(j.ID)
		return nil
	}
	if loadErr != nil {
		return loadErr
	}
	if latest.Phase != "waiting_ci" {
		e.forgetCICheck(j.ID)
		return nil
	}
	current, loadErr = e.Store.Project(j.ProjectID)
	if errors.Is(loadErr, sql.ErrNoRows) {
		return nil
	}
	if loadErr != nil {
		return loadErr
	}
	if current.Blocked || (current.Paused && j.Source != "manual") {
		return nil
	}
	if err == nil && head != "" && head != j.Commit {
		_, err = e.Store.EnqueueObserved(j.Project, "poll", "", head)
		if err == nil {
			err = errors.New("automatic target superseded by the current branch head; latest target reconciled")
		}
	}
	if err == nil && state == "passed" {
		_, err = e.Store.db.Exec(`UPDATE jobs SET phase='queued' WHERE id=? AND phase='waiting_ci'`, j.ID)
		return err
	}
	if err == nil {
		return nil
	}
	e.forgetCICheck(j.ID)
	log := e.Store.LogWriter(j.ID)
	fmt.Fprintf(log, "Failed: %s\n", err)
	message := log.Redact(err.Error())
	log.Close()
	_, err = e.Store.db.Exec(`UPDATE jobs SET phase='failed',finished=?,error=? WHERE id=? AND phase='waiting_ci'`, now(), message, j.ID)
	if err != nil {
		return err
	}
	if err := e.prune(j.ProjectID); err != nil {
		log := e.Store.LogWriter(j.ID)
		fmt.Fprintln(log, "Retention cleanup failed; will retry after the next deployment")
		log.Close()
	}
	return nil
}
