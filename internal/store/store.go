// Package store is the instance's SQLite state: ownership metadata, runner
// records, job history and the setup/migration journal.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver
)

// migrations are applied in order; index+1 is the schema version.
var migrations = []string{
	`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
	 CREATE TABLE runners (
	   name TEXT PRIMARY KEY,
	   owner TEXT NOT NULL, repo TEXT NOT NULL DEFAULT '', os TEXT NOT NULL,
	   lane INTEGER NOT NULL DEFAULT 0,
	   backend TEXT NOT NULL DEFAULT '', backend_id TEXT NOT NULL DEFAULT '',
	   github_runner_id INTEGER NOT NULL DEFAULT 0,
	   state TEXT NOT NULL,
	   created INTEGER NOT NULL,
	   job_run_id INTEGER NOT NULL DEFAULT 0, job_name TEXT NOT NULL DEFAULT '',
	   job_repo TEXT NOT NULL DEFAULT ''
	 );
	 CREATE INDEX runners_state ON runners(state);
	 CREATE TABLE jobs (
	   id INTEGER PRIMARY KEY AUTOINCREMENT,
	   owner TEXT NOT NULL, repo TEXT NOT NULL, workflow TEXT NOT NULL, job TEXT NOT NULL,
	   cost INTEGER NOT NULL DEFAULT 0, peak_mem INTEGER NOT NULL DEFAULT 0,
	   duration_ms INTEGER NOT NULL DEFAULT 0, result TEXT NOT NULL DEFAULT '',
	   finished INTEGER NOT NULL
	 );
	 CREATE INDEX jobs_key ON jobs(owner, repo, workflow, job, finished);
	 CREATE TABLE journal (
	   id INTEGER PRIMARY KEY AUTOINCREMENT,
	   ts INTEGER NOT NULL, kind TEXT NOT NULL, step TEXT NOT NULL,
	   status TEXT NOT NULL, data TEXT NOT NULL DEFAULT ''
	 );`,
	`ALTER TABLE runners ADD COLUMN job_id TEXT NOT NULL DEFAULT '';
	 ALTER TABLE runners ADD COLUMN job_workflow_ref TEXT NOT NULL DEFAULT '';
	 ALTER TABLE runners ADD COLUMN job_event TEXT NOT NULL DEFAULT '';`,
}

// SchemaVersion is the newest schema this build writes.
var SchemaVersion = len(migrations)

// Store wraps the database.
type Store struct{ db *sql.DB }

// Open opens (creating if needed) the database and runs pending migrations.
func Open(path string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("store schema version %d is newer than this binary (%d)", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Version returns the applied schema version.
func (s *Store) Version() (int, error) {
	var v int
	err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	return v, err
}

// ---- meta ----------------------------------------------------------------

// Meta is the ownership record of this instance.
type Meta struct {
	HostID           string
	Prefix           string
	PreviousPrefixes []string
}

// ErrNoMeta is returned by GetMeta before SetMeta has run.
var ErrNoMeta = errors.New("store: no meta")

// SetMeta writes the ownership record.
func (s *Store) SetMeta(m Meta) error {
	prev, _ := json.Marshal(m.PreviousPrefixes)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range map[string]string{
		"host_id": m.HostID, "prefix": m.Prefix, "previous_prefixes": string(prev),
		"schema_version": fmt.Sprint(SchemaVersion),
	} {
		if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetMeta reads the ownership record.
func (s *Store) GetMeta() (Meta, error) {
	rows, err := s.db.Query(`SELECT key, value FROM meta`)
	if err != nil {
		return Meta{}, err
	}
	defer rows.Close()
	kv := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return Meta{}, err
		}
		kv[k] = v
	}
	if err := rows.Err(); err != nil {
		return Meta{}, err
	}
	if kv["host_id"] == "" {
		return Meta{}, ErrNoMeta
	}
	m := Meta{HostID: kv["host_id"], Prefix: kv["prefix"]}
	if p := kv["previous_prefixes"]; p != "" {
		if err := json.Unmarshal([]byte(p), &m.PreviousPrefixes); err != nil {
			return Meta{}, err
		}
	}
	return m, nil
}

// ---- runners -------------------------------------------------------------

// Runner states.
const (
	RunnerStarting = "starting"
	RunnerIdle     = "idle"
	RunnerRunning  = "running"
	RunnerStopping = "stopping"
)

// Runner is a runner this instance started.
type Runner struct {
	Name           string
	Owner, Repo    string // scale-set target
	OS             string
	Lane           int
	Backend        string
	BackendID      string
	GitHubRunnerID int
	State          string
	Created        time.Time
	JobRunID       int64
	JobName        string
	JobRepo        string // owner/repo of the job
	JobID          string // service job id
	JobWorkflowRef string
	JobEvent       string
}

const runnerCols = `name, owner, repo, os, lane, backend, backend_id, github_runner_id, state, created, job_run_id, job_name, job_repo, job_id, job_workflow_ref, job_event`

// UpsertRunner inserts or replaces a runner record.
func (s *Store) UpsertRunner(r Runner) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO runners(`+runnerCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.Name, r.Owner, r.Repo, r.OS, r.Lane, r.Backend, r.BackendID, r.GitHubRunnerID, r.State,
		r.Created.UnixNano(), r.JobRunID, r.JobName, r.JobRepo, r.JobID, r.JobWorkflowRef, r.JobEvent)
	return err
}

// SetRunnerState updates a runner's state; it reports whether the runner exists.
func (s *Store) SetRunnerState(name, state string) (bool, error) {
	res, err := s.db.Exec(`UPDATE runners SET state=? WHERE name=?`, state, name)
	return affected(res, err)
}

// RunnerJob is the job a runner took.
type RunnerJob struct {
	Repo        string // owner/repo
	RunID       int64
	Name        string
	JobID       string
	WorkflowRef string
	Event       string
}

// SetRunnerJob records the job a runner took.
func (s *Store) SetRunnerJob(name string, j RunnerJob) (bool, error) {
	res, err := s.db.Exec(`UPDATE runners SET job_repo=?, job_run_id=?, job_name=?, job_id=?, job_workflow_ref=?, job_event=? WHERE name=?`,
		j.Repo, j.RunID, j.Name, j.JobID, j.WorkflowRef, j.Event, name)
	return affected(res, err)
}

// SetRunnerBackendID records the backend handle once a runner started. A
// "starting" runner becomes idle; any later state (a job already started) and
// the job fields are left alone.
func (s *Store) SetRunnerBackendID(name, backendID string) (bool, error) {
	res, err := s.db.Exec(`UPDATE runners SET backend_id=?, state=CASE WHEN state=? THEN ? ELSE state END WHERE name=?`,
		backendID, RunnerStarting, RunnerIdle, name)
	return affected(res, err)
}

// DeleteRunner removes a runner record; it reports whether it existed.
func (s *Store) DeleteRunner(name string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM runners WHERE name=?`, name)
	return affected(res, err)
}

func affected(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func scanRunner(sc interface{ Scan(...any) error }) (Runner, error) {
	var r Runner
	var created int64
	err := sc.Scan(&r.Name, &r.Owner, &r.Repo, &r.OS, &r.Lane, &r.Backend, &r.BackendID, &r.GitHubRunnerID,
		&r.State, &created, &r.JobRunID, &r.JobName, &r.JobRepo, &r.JobID, &r.JobWorkflowRef, &r.JobEvent)
	r.Created = time.Unix(0, created)
	return r, err
}

// GetRunner returns one runner; ok is false when absent.
func (s *Store) GetRunner(name string) (r Runner, ok bool, err error) {
	r, err = scanRunner(s.db.QueryRow(`SELECT `+runnerCols+` FROM runners WHERE name=?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Runner{}, false, nil
	}
	return r, err == nil, err
}

// ListRunners returns every runner, oldest first.
func (s *Store) ListRunners() ([]Runner, error) {
	rows, err := s.db.Query(`SELECT ` + runnerCols + ` FROM runners ORDER BY created, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Runner
	for rows.Next() {
		r, err := scanRunner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- job history ---------------------------------------------------------

// JobRecord is one finished job, kept for cost learning.
type JobRecord struct {
	Owner, Repo, Workflow, Job string
	Cost                       int64
	PeakMem                    int64
	Duration                   time.Duration
	Result                     string
	Finished                   time.Time
}

// AddJob appends a finished job.
func (s *Store) AddJob(j JobRecord) error {
	_, err := s.db.Exec(`INSERT INTO jobs(owner,repo,workflow,job,cost,peak_mem,duration_ms,result,finished) VALUES(?,?,?,?,?,?,?,?,?)`,
		j.Owner, j.Repo, j.Workflow, j.Job, j.Cost, j.PeakMem, j.Duration.Milliseconds(), j.Result, j.Finished.UnixNano())
	return err
}

// RecentJobs returns up to limit most recent finished runs of one job, newest first.
func (s *Store) RecentJobs(owner, repo, workflow, job string, limit int) ([]JobRecord, error) {
	rows, err := s.db.Query(`SELECT owner,repo,workflow,job,cost,peak_mem,duration_ms,result,finished FROM jobs
		WHERE owner=? AND repo=? AND workflow=? AND job=? ORDER BY finished DESC, id DESC LIMIT ?`,
		owner, repo, workflow, job, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobRecord
	for rows.Next() {
		var j JobRecord
		var ms, fin int64
		if err := rows.Scan(&j.Owner, &j.Repo, &j.Workflow, &j.Job, &j.Cost, &j.PeakMem, &ms, &j.Result, &fin); err != nil {
			return nil, err
		}
		j.Duration = time.Duration(ms) * time.Millisecond
		j.Finished = time.Unix(0, fin)
		out = append(out, j)
	}
	return out, rows.Err()
}

// PruneJobs deletes history older than cutoff and returns the rows removed.
func (s *Store) PruneJobs(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM jobs WHERE finished < ?`, cutoff.UnixNano())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- journal -------------------------------------------------------------

// Journal statuses.
const (
	StepStarted = "started"
	StepDone    = "done"
	StepFailed  = "failed"
)

// JournalEntry is one append-only setup/migration step.
type JournalEntry struct {
	ID     int64
	Time   time.Time
	Kind   string // setup | migrate | prefix ...
	Step   string
	Status string
	Data   string // free-form, usually JSON
}

// Append adds a journal entry and returns its id. The journal is never edited.
func (s *Store) Append(kind, step, status, data string, at time.Time) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO journal(ts,kind,step,status,data) VALUES(?,?,?,?,?)`, at.UnixNano(), kind, step, status, data)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Journal returns entries of a kind (all kinds when empty), oldest first.
func (s *Store) Journal(kind string) ([]JournalEntry, error) {
	q, args := `SELECT id,ts,kind,step,status,data FROM journal`, []any{}
	if kind != "" {
		q += ` WHERE kind=?`
		args = append(args, kind)
	}
	rows, err := s.db.Query(q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JournalEntry
	for rows.Next() {
		var e JournalEntry
		var ts int64
		if err := rows.Scan(&e.ID, &ts, &e.Kind, &e.Step, &e.Status, &e.Data); err != nil {
			return nil, err
		}
		e.Time = time.Unix(0, ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// StepDone reports whether a step has a "done" entry (for resumable setup).
func (s *Store) StepDone(kind, step string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM journal WHERE kind=? AND step=? AND status=?`, kind, step, StepDone).Scan(&n)
	return n > 0, err
}
