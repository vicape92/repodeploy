package deploy

import (
	"context"
	"time"

	"github.com/vicape92/repodeploy/internal/upload"
)

// SourcePreparer materialises a project's source at ref (a commit SHA or an
// upload hash) into dest, replacing anything an earlier attempt left there.
type SourcePreparer interface {
	Prepare(ctx context.Context, p Project, ref, dest string) error
}

// uploadSource adapts the upload module to the engine.
type uploadSource struct{ uploads *upload.Store }

func (u uploadSource) Prepare(ctx context.Context, p Project, ref, dest string) error {
	return u.uploads.Prepare(p.ID, ref, dest)
}

// sourceFor selects how a project's code is prepared.
func (e *Engine) sourceFor(p Project) SourcePreparer {
	if p.IsUpload() {
		return uploadSource{uploads: e.Store.Uploads}
	}
	return Git{Sources: e.Store.Sources}
}

// userSource reports job sources started by the administrator. They bypass
// automatic deduplication and run while automation is paused.
func userSource(source string) bool { return source == "manual" || source == "upload" }

// uploadGrace protects freshly stored archives whose job may not be queued yet.
const uploadGrace = time.Hour

// pruneUploads removes archives that no retained job references.
func (s *Store) pruneUploads(projectID string) error {
	rows, err := s.db.Query(`SELECT DISTINCT sha FROM jobs WHERE project_id=? AND retained=1 AND sha!=''`, projectID)
	if err != nil {
		return err
	}
	defer rows.Close()
	keep := map[string]bool{}
	for rows.Next() {
		var sha string
		if err = rows.Scan(&sha); err != nil {
			return err
		}
		keep[sha] = true
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return s.Uploads.Prune(projectID, keep, uploadGrace)
}
