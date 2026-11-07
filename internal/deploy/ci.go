package deploy

import (
	"context"
	"errors"
	"fmt"
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
	if last, ok := e.ciChecks[j.ID]; ok && time.Since(last.at) < 10*time.Second {
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
	if e.ciChecks == nil {
		e.ciChecks = make(map[string]ciCheck)
	}
	e.ciChecks[j.ID] = ciCheck{at: checked, state: state}
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

func (e *Engine) recheckCI(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rows, err := e.Store.db.Query(`SELECT ` + jobColumns + ` FROM jobs WHERE phase='waiting_ci' ORDER BY seq`)
	if err != nil {
		return err
	}
	var jobs []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if last, ok := e.ciChecks[j.ID]; ok && time.Since(last.at) < 10*time.Second {
			continue
		}
		current, err := e.Store.Project(j.ProjectID)
		if err != nil {
			return err
		}
		if current.Blocked || (current.Paused && j.Source != "manual") {
			continue
		}
		if j.Source != "manual" {
			head, headErr := (Git{Sources: e.Store.Sources}).Resolve(ctx, j.Project)
			err = headErr
			if err == nil && head != j.Commit {
				_, err = e.Store.Enqueue(j.ProjectID, "poll", "", head, "")
				if err == nil {
					err = errors.New("automatic target superseded by the current branch head; latest target reconciled")
				}
			}
		}
		if err == nil {
			var state string
			state, err = e.ciState(ctx, j)
			if err == nil {
				switch state {
				case "passed":
					if err = e.Store.Requeue(j.ID); err != nil {
						return err
					}
				case "failed":
					err = errors.New("CI/CD failed for the selected commit")
				}
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			delete(e.ciChecks, j.ID)
			log := e.Store.LogWriter(j.ID)
			fmt.Fprintf(log, "Failed: %s\n", err)
			log.Close()
			if err = e.Store.Finish(j.ID, "failed", err.Error(), ""); err != nil {
				return err
			}
			if err = e.prune(j.ProjectID); err != nil {
				log := e.Store.LogWriter(j.ID)
				fmt.Fprintln(log, "Retention cleanup failed; will retry after the next deployment")
				log.Close()
			}
		}
	}
	return nil
}
