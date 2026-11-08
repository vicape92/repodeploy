package deploy

import (
	"errors"

	"github.com/vicape92/repodeploy/internal/envvars"
)

// ErrNoActive reports that a project has no release to redeploy.
var ErrNoActive = errors.New("nothing is deployed yet; deploy the project first")

// EnvPending reports whether the stored variables differ from those the
// active release was built with. Without an active release nothing is
// pending: the first deployment uses the stored values.
func (s *Store) EnvPending(p Project) (bool, error) {
	if p.Active == "" {
		return false, nil
	}
	active, err := s.Job(p.Active)
	if err != nil {
		return false, err
	}
	vars, err := s.Env.List(p.ID)
	if err != nil {
		return false, err
	}
	built := active.Project.EnvDigest
	if built == "" {
		built = envvars.Digest(nil)
	}
	return envvars.Digest(vars) != built, nil
}

// EnqueueConfigChange queues a manual deployment pinned to the active
// release's commit or upload, so it rebuilds with the current variables.
func (s *Store) EnqueueConfigChange(projectID string) (j Job, err error) {
	err = s.WithProject(projectID, func(p Project) error {
		if p.Active == "" {
			return ErrNoActive
		}
		active, err := s.Job(p.Active)
		if err != nil {
			return err
		}
		j, err = s.enqueue(projectID, "manual", "", active.Commit, "")
		return err
	})
	return
}

func (s *Store) SetEnv(id string, updates []envvars.Update) error {
	return s.WithProject(id, func(Project) error { return s.Env.Set(id, updates) })
}
func (s *Store) UnsetEnv(id, name string) error {
	return s.WithProject(id, func(Project) error { return s.Env.Unset(id, name) })
}
