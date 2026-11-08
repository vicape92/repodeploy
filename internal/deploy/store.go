package deploy

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/vicape92/repodeploy/internal/envvars"
	"github.com/vicape92/repodeploy/internal/scm"
	"github.com/vicape92/repodeploy/internal/upload"

	_ "modernc.org/sqlite"
)

var ErrNoJob = errors.New("no job available")
var ErrPaused = errors.New("project automation is paused or blocked")

type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Source is "git" (or empty) for repository projects and "upload" for
	// projects that deploy uploaded archives.
	Source        string `json:"source,omitempty"`
	Repo          string `json:"repo"`
	Branch        string `json:"branch"`
	Connection    string `json:"connection,omitempty"`
	RepositoryID  string `json:"repository_id,omitempty"`
	WaitCI        bool   `json:"wait_ci"`
	CIProvider    string `json:"ci_provider,omitempty"`
	CITimeout     int    `json:"ci_timeout"`
	SSHKey        string `json:"ssh_key,omitempty"`
	KnownHosts    string `json:"known_hosts,omitempty"`
	Mode          string `json:"mode"`
	Dockerfile    string `json:"dockerfile,omitempty"`
	ComposeFile   string `json:"compose_file,omitempty"`
	Port          int    `json:"port,omitempty"`
	ContainerPort int    `json:"container_port,omitempty"`
	// BindAddress is the host interface for the Dockerfile-mode port. Empty
	// selects 127.0.0.1 so the application stays behind a reverse proxy.
	BindAddress string `json:"bind_address,omitempty"`
	// AllowPrivilegeEscalation disables the injected no-new-privileges option.
	AllowPrivilegeEscalation bool `json:"allow_privilege_escalation,omitempty"`
	// PullImages pulls image: services and Dockerfile base images on every
	// deployment instead of reusing locally cached tags.
	PullImages    bool   `json:"pull_images,omitempty"`
	HealthURL     string `json:"health_url,omitempty"`
	HealthService string `json:"health_service,omitempty"`
	HealthTimeout int    `json:"health_timeout"`
	PollSeconds   int    `json:"poll_seconds"`
	Retain        int    `json:"retain"`
	Paused        bool   `json:"paused"`
	Blocked       bool   `json:"blocked"`
	Active        string `json:"active,omitempty"`
	PollError     string `json:"poll_error,omitempty"`
	// EnvDigest records, in job snapshots only, the variables a release was
	// built with. Project settings never store it.
	EnvDigest string `json:"env_digest,omitempty"`
	// Env carries the stored variables into the runtime for one build. It is
	// never persisted.
	Env []envvars.Var `json:"-"`
}

type Job struct {
	ID        string  `json:"id"`
	ProjectID string  `json:"project_id"`
	Project   Project `json:"project"`
	Source    string  `json:"source"`
	Commit    string  `json:"commit,omitempty"`
	Rollback  string  `json:"rollback,omitempty"`
	Phase     string  `json:"phase"`
	Created   string  `json:"created"`
	Started   string  `json:"started,omitempty"`
	Finished  string  `json:"finished,omitempty"`
	Error     string  `json:"error,omitempty"`
	Recovery  string  `json:"recovery,omitempty"`
	Manifest  string  `json:"manifest,omitempty"`
	Previous  string  `json:"previous,omitempty"`
}

// IsUpload reports whether the project deploys uploaded archives.
func (p Project) IsUpload() bool { return p.Source == "upload" }

type Store struct {
	Root            string
	Sources         *scm.Manager
	Env             *envvars.Store
	Uploads         *upload.Store
	db              *sql.DB
	projectRevision atomic.Uint64
	projectMu       sync.Mutex
	projectLocks    map[string]*projectLock
}

type projectLock struct {
	mu   sync.Mutex
	refs int
}

func (s *Store) lockProject(id string) func() {
	s.projectMu.Lock()
	if s.projectLocks == nil {
		s.projectLocks = make(map[string]*projectLock)
	}
	l := s.projectLocks[id]
	if l == nil {
		l = &projectLock{}
		s.projectLocks[id] = l
	}
	l.refs++
	s.projectMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		s.projectMu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(s.projectLocks, id)
		}
		s.projectMu.Unlock()
	}
}

func lockFile(root, name string, shared bool) (func(), error) {
	f, err := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	mode := syscall.LOCK_EX
	if shared {
		mode = syscall.LOCK_SH
	}
	if err = syscall.Flock(int(f.Fd()), mode); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func (s *Store) WithProject(id string, fn func(Project) error) error {
	unlock := s.lockProject(id)
	defer unlock()
	release, err := lockFile(s.Root, "artifacts.lock", true)
	if err != nil {
		return err
	}
	defer release()
	p, err := s.Project(id)
	if err != nil {
		return err
	}
	return fn(p)
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func marshal(v any) (string, error) { b, err := json.Marshal(v); return string(b), err }
func now() string                   { return time.Now().UTC().Format(time.RFC3339Nano) }

func OpenStore(root string) (*Store, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	if err = os.Chmod(root, 0700); err != nil {
		return nil, err
	}
	for _, sub := range []string{"releases", "logs", "secrets", "uploads"} {
		if err = os.MkdirAll(filepath.Join(root, sub), 0700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "state.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var version int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version > 1 {
		db.Close()
		return nil, fmt.Errorf("unsupported database schema version %d; use a matching binary or restore its backup", version)
	}
	s := &Store{Root: root, db: db, Sources: scm.New(root), Env: envvars.New(filepath.Join(root, "secrets")), Uploads: upload.New(filepath.Join(root, "uploads"))}
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS projects(id TEXT PRIMARY KEY, config TEXT NOT NULL, active TEXT NOT NULL DEFAULT '', paused INTEGER NOT NULL DEFAULT 0, blocked INTEGER NOT NULL DEFAULT 0, poll_error TEXT NOT NULL DEFAULT '');
 CREATE TABLE IF NOT EXISTS jobs(seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT NOT NULL UNIQUE,project_id TEXT NOT NULL REFERENCES projects(id),snapshot TEXT NOT NULL,source TEXT NOT NULL,sha TEXT NOT NULL DEFAULT '',rollback TEXT NOT NULL DEFAULT '',phase TEXT NOT NULL DEFAULT 'queued',created TEXT NOT NULL,started TEXT NOT NULL DEFAULT '',finished TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',recovery TEXT NOT NULL DEFAULT '',manifest TEXT NOT NULL DEFAULT '',previous TEXT NOT NULL DEFAULT '',retained INTEGER NOT NULL DEFAULT 1);
 CREATE TABLE IF NOT EXISTS deliveries(project_id TEXT NOT NULL,key TEXT NOT NULL,job TEXT NOT NULL,PRIMARY KEY(project_id,key));
 CREATE TABLE IF NOT EXISTS observed(project_id TEXT NOT NULL,sha TEXT NOT NULL,job TEXT NOT NULL,PRIMARY KEY(project_id,sha));
 CREATE INDEX IF NOT EXISTS jobs_phase_seq ON jobs(phase,seq);
 CREATE INDEX IF NOT EXISTS jobs_inflight_seq ON jobs(phase,seq) WHERE phase NOT IN ('queued','waiting_ci','succeeded','failed');
 CREATE INDEX IF NOT EXISTS jobs_project_retained_seq ON jobs(project_id,retained,seq);
 CREATE INDEX IF NOT EXISTS jobs_retained_project_seq ON jobs(project_id,seq) WHERE retained=1;
 PRAGMA user_version=1;`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = os.Chmod(filepath.Join(root, "state.db"), 0600); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func Lock(root string) (func(), error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(root, "engine.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another engine owns this state directory: %w", err)
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func (s *Store) SaveProject(p Project) (Project, error) {
	creating := p.ID == ""
	if creating {
		p.ID = newID()
	}
	unlock := s.lockProject(p.ID)
	defer unlock()
	release, err := lockFile(s.Root, "artifacts.lock", true)
	if err != nil {
		return p, err
	}
	defer release()
	if !creating {
		if _, err = s.Project(p.ID); err != nil {
			return p, err
		}
	}
	if err = validateSSHFiles(p); err != nil {
		return p, err
	}
	p.EnvDigest, p.Env = "", nil
	if p.IsUpload() {
		p.Mode = "manual"
	}
	if p.CITimeout == 0 {
		p.CITimeout = 1800
	}
	if p.HealthTimeout == 0 {
		p.HealthTimeout = 60
	}
	if p.PollSeconds == 0 {
		p.PollSeconds = 60
	}
	if p.Retain == 0 {
		p.Retain = 5
	}
	b, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	if creating {
		_, err = s.db.Exec(`INSERT INTO projects(id,config) VALUES(?,?)`, p.ID, string(b))
	} else {
		var result sql.Result
		result, err = s.db.Exec(`UPDATE projects SET config=? WHERE id=?`, string(b), p.ID)
		if err == nil {
			var n int64
			n, err = result.RowsAffected()
			if err == nil && n == 0 {
				err = sql.ErrNoRows
			}
		}
	}
	if err != nil {
		return p, err
	}
	s.projectRevision.Add(1)
	return s.Project(p.ID)
}

type scanner interface{ Scan(...any) error }

func scanProject(row scanner) (Project, error) {
	var p Project
	var b string
	err := row.Scan(&b, &p.Active, &p.Paused, &p.Blocked, &p.PollError)
	if err != nil {
		return p, err
	}
	active, paused, blocked, pollErr := p.Active, p.Paused, p.Blocked, p.PollError
	err = json.Unmarshal([]byte(b), &p)
	p.Active, p.Paused, p.Blocked, p.PollError = active, paused, blocked, pollErr
	return p, err
}
func (s *Store) Project(id string) (Project, error) {
	return scanProject(s.db.QueryRow(`SELECT config,active,paused,blocked,poll_error FROM projects WHERE id=?`, id))
}
func (s *Store) Projects() ([]Project, error) {
	rows, err := s.db.Query(`SELECT config,active,paused,blocked,poll_error FROM projects ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

const jobColumns = `id,project_id,snapshot,source,sha,rollback,phase,created,started,finished,error,recovery,manifest,previous`

func scanJob(row scanner) (Job, error) {
	var j Job
	var snapshot string
	err := row.Scan(&j.ID, &j.ProjectID, &snapshot, &j.Source, &j.Commit, &j.Rollback, &j.Phase, &j.Created, &j.Started, &j.Finished, &j.Error, &j.Recovery, &j.Manifest, &j.Previous)
	if err == nil {
		err = json.Unmarshal([]byte(snapshot), &j.Project)
	}
	return j, err
}
func (s *Store) Job(id string) (Job, error) {
	return scanJob(s.db.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE id=?`, id))
}
func (s *Store) Jobs(project string) ([]Job, error) {
	rows, err := s.db.Query(`SELECT `+jobColumns+` FROM jobs WHERE project_id=? AND retained=1 ORDER BY seq DESC`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) Enqueue(project, source, event, sha, rollback string) (j Job, err error) {
	err = s.WithProject(project, func(Project) error { j, err = s.enqueue(project, source, event, sha, rollback); return err })
	return
}

func (s *Store) enqueue(project, source, event, sha, rollback string) (Job, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	p, err := scanProject(tx.QueryRow(`SELECT config,active,paused,blocked,poll_error FROM projects WHERE id=?`, project))
	if err != nil {
		return Job{}, err
	}
	if rollback != "" {
		var valid int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE id=? AND project_id=? AND retained=1 AND phase='succeeded'`, rollback, project).Scan(&valid); err != nil {
			return Job{}, err
		}
		if valid == 0 {
			return Job{}, errors.New("rollback release is unavailable")
		}
	}
	var existing string
	if event != "" {
		err = tx.QueryRow(`SELECT job FROM deliveries WHERE project_id=? AND key=?`, project, event).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Job{}, err
		}
	}
	if existing == "" && !userSource(source) && sha != "" {
		err = tx.QueryRow(`SELECT job FROM observed WHERE project_id=? AND sha=?`, project, sha).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Job{}, err
		}
	}
	if existing == "" && !userSource(source) && sha != "" && p.Active != "" {
		var activeSHA string
		if err = tx.QueryRow(`SELECT sha FROM jobs WHERE id=?`, p.Active).Scan(&activeSHA); err != nil {
			return Job{}, err
		}
		if activeSHA == sha {
			existing = p.Active
		}
	}
	if existing == "" {
		if (p.Paused && !userSource(source)) || (p.Blocked && rollback == "") {
			return Job{}, ErrPaused
		}
		existing = newID()
		snapshot, _ := json.Marshal(p)
		if _, err = tx.Exec(`INSERT INTO jobs(id,project_id,snapshot,source,sha,rollback,created) VALUES(?,?,?,?,?,?,?)`, existing, project, string(snapshot), source, sha, rollback, now()); err != nil {
			return Job{}, err
		}
	}
	if sha != "" && !userSource(source) {
		if _, err = tx.Exec(`DELETE FROM observed WHERE project_id=?`, project); err != nil {
			return Job{}, err
		}
		if _, err = tx.Exec(`INSERT INTO observed(project_id,sha,job) VALUES(?,?,?)`, project, sha, existing); err != nil {
			return Job{}, err
		}
	}
	if event != "" {
		if _, err = tx.Exec(`INSERT OR IGNORE INTO deliveries(project_id,key,job) VALUES(?,?,?)`, project, event, existing); err != nil {
			return Job{}, err
		}
	}
	j, err := scanJob(tx.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE id=?`, existing))
	if err != nil {
		return j, err
	}
	return j, tx.Commit()
}

func (s *Store) Claim() (Job, error) {
	row := s.db.QueryRow(`UPDATE jobs SET phase='preparing',started=CASE WHEN started='' THEN ? ELSE started END WHERE seq=(SELECT j.seq FROM jobs j JOIN projects p ON p.id=j.project_id WHERE j.phase='queued' AND (p.blocked=0 OR j.rollback!='') AND (p.paused=0 OR j.source IN ('manual','upload')) AND (j.rollback!='' OR NOT EXISTS(SELECT 1 FROM jobs waiting WHERE waiting.project_id=j.project_id AND waiting.phase='waiting_ci' AND waiting.seq<j.seq)) ORDER BY j.seq LIMIT 1) AND NOT EXISTS(SELECT 1 FROM jobs WHERE phase NOT IN ('queued','waiting_ci','succeeded','failed')) RETURNING `+jobColumns, now())
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNoJob
	}
	return j, err
}

func (s *Store) SetPhase(id, phase, sha string) error {
	_, err := s.db.Exec(`UPDATE jobs SET phase=?,sha=CASE WHEN ?='' THEN sha ELSE ? END WHERE id=?`, phase, sha, sha, id)
	return err
}

// SetSnapshot replaces the project settings recorded for a job.
func (s *Store) SetSnapshot(id string, p Project) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE jobs SET snapshot=? WHERE id=?`, string(b), id)
	return err
}

func (s *Store) SetArtifact(id, manifest, previous string) error {
	_, err := s.db.Exec(`UPDATE jobs SET manifest=?,previous=? WHERE id=?`, manifest, previous, id)
	return err
}
func (s *Store) Requeue(id string) error {
	_, err := s.db.Exec(`UPDATE jobs SET phase='queued',error='' WHERE id=?`, id)
	return err
}
func (s *Store) Finish(id, phase, message, recovery string) error {
	_, err := s.db.Exec(`UPDATE jobs SET phase=?,error=?,recovery=?,finished=? WHERE id=?`, phase, message, recovery, now(), id)
	return err
}
func (s *Store) SetActive(project, release string) error {
	_, err := s.db.Exec(`UPDATE projects SET active=?,blocked=CASE WHEN ?='' THEN blocked ELSE 0 END WHERE id=?`, release, release, project)
	return err
}
func (s *Store) Pause(project string, blocked bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE projects SET paused=1,blocked=MAX(blocked,?) WHERE id=?`, blocked, project); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE jobs SET phase='failed',error='automation paused',finished=? WHERE project_id=? AND ((source NOT IN ('manual','upload') AND phase='queued') OR phase='waiting_ci')`, now(), project); err != nil {
		return err
	}
	err = tx.Commit()
	if err == nil {
		s.projectRevision.Add(1)
	}
	return err
}
func (s *Store) Resume(project string) error {
	_, err := s.db.Exec(`UPDATE projects SET paused=0 WHERE id=? AND blocked=0`, project)
	if err == nil {
		s.projectRevision.Add(1)
	}
	return err
}
func (s *Store) PollError(project, message string) error {
	_, err := s.db.Exec(`UPDATE projects SET poll_error=? WHERE id=?`, message, project)
	return err
}
func (s *Store) Interrupted() ([]Job, error) {
	rows, err := s.db.Query(`SELECT ` + jobColumns + ` FROM jobs WHERE phase NOT IN ('queued','waiting_ci','succeeded','failed') ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

var ErrSourceChanged = errors.New("project source changed during detection")

func (s *Store) EnqueueObserved(observed Project, source, event, sha string) (j Job, err error) {
	err = s.WithProject(observed.ID, func(current Project) error {
		if !sameSource(current, observed) {
			return ErrSourceChanged
		}
		j, err = s.enqueue(current.ID, source, event, sha, "")
		return err
	})
	return
}

func sameSource(a, b Project) bool {
	return a.Source == b.Source && a.Repo == b.Repo && a.Branch == b.Branch && a.Connection == b.Connection && a.RepositoryID == b.RepositoryID && a.SSHKey == b.SSHKey && a.KnownHosts == b.KnownHosts && a.Mode == b.Mode
}
