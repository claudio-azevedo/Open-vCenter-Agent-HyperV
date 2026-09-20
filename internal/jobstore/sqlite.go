package jobstore

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// sqliteStore implements Store on top of a single SQLite database file
// (modernc.org/sqlite - pure Go, no cgo).
//
// The service process and the short-lived worker subprocesses (`ovc-agent.exe
// job <id>`) all open the same file. WAL mode plus a generous busy_timeout let
// them interleave reads and writes without the exclusive-lock contention the
// previous bbolt store suffered from.
type sqliteStore struct {
	db *sql.DB
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS jobs (
    job_id        TEXT PRIMARY KEY,
    task_id       TEXT NOT NULL,
    task_type     TEXT NOT NULL,
    action        TEXT NOT NULL DEFAULT '',
    vm_identifier TEXT NOT NULL DEFAULT '',
    payload       BLOB,
    status        TEXT NOT NULL,
    worker_pid    INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL,
    started_at    TEXT,
    completed_at  TEXT,
    error_message TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_jobs_vm ON jobs(vm_identifier);
`

// Open creates or opens the job store at the given path and ensures the schema
// exists. Safe to call from multiple processes against the same file.
func Open(dbPath string) (Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open job store %q: %w", dbPath, err)
	}

	// A single connection avoids "database is locked" between pooled writers in
	// the same process; cross-process access is handled by WAL + busy_timeout.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize job store schema: %w", err)
	}

	return &sqliteStore{db: db}, nil
}

func (s *sqliteStore) Close() error {
	return s.db.Close()
}

// OpenShared is kept for compatibility with earlier call sites. The SQLite store
// no longer needs the open/close-per-operation dance the bbolt store used -
// WAL mode plus busy_timeout handle concurrent service/worker access - so this
// is now just an alias for Open.
func OpenShared(dbPath string) (Store, error) { return Open(dbPath) }

// --- time helpers -----------------------------------------------------------

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func fmtTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}

func parseTimePtr(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, v.String)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

// --- CRUD ------------------------------------------------------------------

func (s *sqliteStore) Create(job *JobRecord) error {
	if job.JobID == "" {
		return fmt.Errorf("job ID is required")
	}
	if job.Status == "" {
		return fmt.Errorf("job status is required")
	}

	_, err := s.db.Exec(
		`INSERT INTO jobs
		    (job_id, task_id, task_type, action, vm_identifier, payload, status, worker_pid, created_at, started_at, completed_at, error_message)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.JobID, job.TaskID, job.TaskType, job.Action, job.VMIdentifier, job.Payload,
		job.Status, job.WorkerPID, fmtTime(job.CreatedAt),
		fmtTimePtr(job.StartedAt), fmtTimePtr(job.CompletedAt), job.ErrorMessage,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return fmt.Errorf("job %q already exists", job.JobID)
		}
		return fmt.Errorf("create job %q: %w", job.JobID, err)
	}
	return nil
}

func scanJob(sc interface {
	Scan(dest ...any) error
}) (*JobRecord, error) {
	var (
		rec                               JobRecord
		payload                           []byte
		startedAt, completedAt, createdAt sql.NullString
	)
	if err := sc.Scan(
		&rec.JobID, &rec.TaskID, &rec.TaskType, &rec.Action, &rec.VMIdentifier,
		&payload, &rec.Status, &rec.WorkerPID, &createdAt, &startedAt, &completedAt, &rec.ErrorMessage,
	); err != nil {
		return nil, err
	}
	rec.Payload = payload
	if t := parseTimePtr(createdAt); t != nil {
		rec.CreatedAt = *t
	}
	rec.StartedAt = parseTimePtr(startedAt)
	rec.CompletedAt = parseTimePtr(completedAt)
	return &rec, nil
}

const jobColumns = `job_id, task_id, task_type, action, vm_identifier, payload, status, worker_pid, created_at, started_at, completed_at, error_message`

func (s *sqliteStore) GetByID(jobID string) (*JobRecord, error) {
	row := s.db.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE job_id = ?`, jobID)
	rec, err := scanJob(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("job %q not found", jobID)
	}
	if err != nil {
		return nil, fmt.Errorf("get job %q: %w", jobID, err)
	}
	return rec, nil
}

func (s *sqliteStore) UpdateStatus(jobID string, from, to string, fields map[string]interface{}) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	row := tx.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE job_id = ?`, jobID)
	rec, err := scanJob(row)
	if err == sql.ErrNoRows {
		return fmt.Errorf("job %q not found", jobID)
	}
	if err != nil {
		return fmt.Errorf("read job %q: %w", jobID, err)
	}

	// Compare-and-swap: reject if current status doesn't match expected.
	if rec.Status != from {
		return fmt.Errorf("job %q status is %q, expected %q", jobID, rec.Status, from)
	}

	rec.Status = to
	if fields != nil {
		applyFields(rec, fields)
	}

	if _, err := tx.Exec(
		`UPDATE jobs SET status = ?, worker_pid = ?, started_at = ?, completed_at = ?, error_message = ? WHERE job_id = ?`,
		rec.Status, rec.WorkerPID, fmtTimePtr(rec.StartedAt), fmtTimePtr(rec.CompletedAt), rec.ErrorMessage, jobID,
	); err != nil {
		return fmt.Errorf("update job %q: %w", jobID, err)
	}

	return tx.Commit()
}

// --- queries -------------------------------------------------------------

func (s *sqliteStore) queryJobs(query string, args ...any) ([]*JobRecord, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*JobRecord
	for rows.Next() {
		rec, err := scanJob(rows)
		if err != nil {
			continue // skip corrupt rows
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *sqliteStore) ListByStatus(status string) ([]*JobRecord, error) {
	return s.queryJobs(
		`SELECT `+jobColumns+` FROM jobs WHERE status = ? ORDER BY created_at ASC`, status)
}

func (s *sqliteStore) ListRunningByVM(vmIdentifier string) ([]*JobRecord, error) {
	return s.queryJobs(
		`SELECT `+jobColumns+` FROM jobs WHERE vm_identifier = ? AND status = ? ORDER BY created_at ASC`,
		vmIdentifier, StatusRunning)
}

// HasConflict checks if a matching queued or running job exists for duplicate
// rejection. For vm_management tasks it also matches the action field.
func (s *sqliteStore) HasConflict(taskType, action, vmIdentifier string) (bool, error) {
	if vmIdentifier == "" {
		return false, nil
	}

	query := `SELECT COUNT(*) FROM jobs
	          WHERE vm_identifier = ?
	            AND status IN (?, ?)
	            AND task_type = ?`
	args := []any{vmIdentifier, StatusQueued, StatusRunning, taskType}

	if taskType == "vm_management" {
		query += ` AND action = ?`
		args = append(args, action)
	}

	var count int
	if err := s.db.QueryRow(query, args...).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *sqliteStore) DeleteOlderThan(age time.Duration, statuses []string) (int, error) {
	if len(statuses) == 0 {
		return 0, nil
	}
	cutoff := fmtTime(time.Now().UTC().Add(-age))

	placeholders := strings.Repeat("?,", len(statuses))
	placeholders = placeholders[:len(placeholders)-1]

	args := make([]any, 0, len(statuses)+1)
	for _, st := range statuses {
		args = append(args, st)
	}
	args = append(args, cutoff)

	res, err := s.db.Exec(
		fmt.Sprintf(`DELETE FROM jobs WHERE status IN (%s) AND created_at < ?`, placeholders), args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
