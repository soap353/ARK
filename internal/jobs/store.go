package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// JobStore provides SQLite persistence for the job queue.
type JobStore struct {
	db *sql.DB
}

// NewJobStore creates a new JobStore.
func NewJobStore(db *sql.DB) *JobStore {
	return &JobStore{db: db}
}

// EnsureTable ensures the jobs table and its indexes exist.
func (s *JobStore) EnsureTable(ctx context.Context) error {
	schema := `
	PRAGMA journal_mode = WAL;
	PRAGMA busy_timeout = 5000;

	CREATE TABLE IF NOT EXISTS jobs (
		id TEXT PRIMARY KEY,
		type TEXT NOT NULL,
		target TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'pending',
		progress REAL NOT NULL DEFAULT 0.0,
		progress_msg TEXT NOT NULL DEFAULT '',
		error_msg TEXT NOT NULL DEFAULT '',
		bytes_downloaded INTEGER NOT NULL DEFAULT 0,
		total_bytes INTEGER NOT NULL DEFAULT 0,
		eta_seconds INTEGER NOT NULL DEFAULT 0,
		metadata TEXT NOT NULL DEFAULT '{}',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		completed_at DATETIME
	);
	CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
	CREATE INDEX IF NOT EXISTS idx_jobs_created_at ON jobs(created_at);
	`
	_, err := s.db.ExecContext(ctx, schema)
	return err
}

// Create inserts a new job into the queue.
func (s *JobStore) Create(ctx context.Context, job *Job) error {
	if job.Status == "" {
		job.Status = StatusPending
	}
	if job.Metadata == "" {
		job.Metadata = "{}"
	}

	query := `
		INSERT INTO jobs (
			id, type, target, status, progress, progress_msg, error_msg,
			bytes_downloaded, total_bytes, eta_seconds, metadata
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := s.db.ExecContext(ctx, query,
		job.ID,
		job.Type,
		job.Target,
		job.Status,
		job.Progress,
		job.ProgressMsg,
		job.ErrorMsg,
		job.BytesDownloaded,
		job.TotalBytes,
		job.ETASeconds,
		job.Metadata,
	)
	return err
}

// Get retrieves a job by ID.
func (s *JobStore) Get(ctx context.Context, id string) (*Job, error) {
	query := `
		SELECT id, type, target, status, progress, progress_msg, error_msg,
		       bytes_downloaded, total_bytes, eta_seconds, metadata,
		       created_at, updated_at, completed_at
		FROM jobs
		WHERE id = ?
	`
	row := s.db.QueryRowContext(ctx, query, id)

	var j Job
	var createdAtStr, updatedAtStr string
	var completedAtStr sql.NullString

	if err := row.Scan(
		&j.ID,
		&j.Type,
		&j.Target,
		&j.Status,
		&j.Progress,
		&j.ProgressMsg,
		&j.ErrorMsg,
		&j.BytesDownloaded,
		&j.TotalBytes,
		&j.ETASeconds,
		&j.Metadata,
		&createdAtStr,
		&updatedAtStr,
		&completedAtStr,
	); err != nil {
		return nil, err
	}

	j.CreatedAt = parseTime(createdAtStr)
	j.UpdatedAt = parseTime(updatedAtStr)
	if completedAtStr.Valid {
		t := parseTime(completedAtStr.String)
		j.CompletedAt = &t
	}

	return &j, nil
}

// UpdateProgress updates the progress information for an active job.
func (s *JobStore) UpdateProgress(ctx context.Context, id string, progress float64, msg string, bytesDown, totalBytes int64, eta int) error {
	query := `
		UPDATE jobs
		SET progress = ?,
		    progress_msg = ?,
		    bytes_downloaded = ?,
		    total_bytes = ?,
		    eta_seconds = ?,
		    updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		_, err = s.db.ExecContext(ctx, query, progress, msg, bytesDown, totalBytes, eta, id)
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "locked") && !strings.Contains(err.Error(), "busy") {
			return err
		}
		time.Sleep(time.Duration(10*(attempt+1)) * time.Millisecond)
	}
	return err
}

// SetStatus transitions a job's status and records error messages / completion time.
func (s *JobStore) SetStatus(ctx context.Context, id string, status string, errorMsg string) error {
	var query string
	var args []any

	if status == StatusDone || status == StatusFailed {
		query = `
			UPDATE jobs
			SET status = ?,
			    error_msg = ?,
			    completed_at = CURRENT_TIMESTAMP,
			    updated_at = CURRENT_TIMESTAMP
			WHERE id = ?
		`
		args = []any{status, errorMsg, id}
	} else {
		query = `
			UPDATE jobs
			SET status = ?,
			    error_msg = ?,
			    updated_at = CURRENT_TIMESTAMP
			WHERE id = ?
		`
		args = []any{status, errorMsg, id}
	}

	var err error
	for attempt := 0; attempt < 10; attempt++ {
		_, err = s.db.ExecContext(ctx, query, args...)
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "locked") && !strings.Contains(err.Error(), "busy") {
			return err
		}
		time.Sleep(time.Duration(10*(attempt+1)) * time.Millisecond)
	}
	return err
}

// List returns jobs matching the optional status filter, ordered newest first.
func (s *JobStore) List(ctx context.Context, status string, limit int) ([]Job, error) {
	query := `
		SELECT id, type, target, status, progress, progress_msg, error_msg,
		       bytes_downloaded, total_bytes, eta_seconds, metadata,
		       created_at, updated_at, completed_at
		FROM jobs
	`
	var args []any
	if status != "" {
		query += " WHERE status = ?"
		args = append(args, status)
	}
	query += " ORDER BY created_at DESC"
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobsList []Job
	for rows.Next() {
		var j Job
		var createdAtStr, updatedAtStr string
		var completedAtStr sql.NullString

		if err := rows.Scan(
			&j.ID,
			&j.Type,
			&j.Target,
			&j.Status,
			&j.Progress,
			&j.ProgressMsg,
			&j.ErrorMsg,
			&j.BytesDownloaded,
			&j.TotalBytes,
			&j.ETASeconds,
			&j.Metadata,
			&createdAtStr,
			&updatedAtStr,
			&completedAtStr,
		); err != nil {
			return nil, err
		}

		j.CreatedAt = parseTime(createdAtStr)
		j.UpdatedAt = parseTime(updatedAtStr)
		if completedAtStr.Valid {
			t := parseTime(completedAtStr.String)
			j.CompletedAt = &t
		}

		jobsList = append(jobsList, j)
	}

	return jobsList, nil
}

// AcquireNextPending atomically transitions the oldest pending job of allowedTypes to 'running'.
func (s *JobStore) AcquireNextPending(ctx context.Context, allowedTypes []string) (*Job, error) {
	if len(allowedTypes) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(allowedTypes))
	args := make([]any, len(allowedTypes))
	for i, t := range allowedTypes {
		placeholders[i] = "?"
		args[i] = t
	}

	query := fmt.Sprintf(`
		UPDATE jobs
		SET status = 'running', updated_at = CURRENT_TIMESTAMP
		WHERE id = (
			SELECT id FROM jobs
			WHERE status = 'pending' AND type IN (%s)
			ORDER BY created_at ASC
			LIMIT 1
		)
		RETURNING id, type, target, status, progress, progress_msg, error_msg,
		          bytes_downloaded, total_bytes, eta_seconds, metadata,
		          created_at, updated_at, completed_at
	`, strings.Join(placeholders, ","))

	var row *sql.Row
	var j Job
	var createdAtStr, updatedAtStr string
	var completedAtStr sql.NullString
	var err error

	for attempt := 0; attempt < 10; attempt++ {
		row = s.db.QueryRowContext(ctx, query, args...)
		err = row.Scan(
			&j.ID,
			&j.Type,
			&j.Target,
			&j.Status,
			&j.Progress,
			&j.ProgressMsg,
			&j.ErrorMsg,
			&j.BytesDownloaded,
			&j.TotalBytes,
			&j.ETASeconds,
			&j.Metadata,
			&createdAtStr,
			&updatedAtStr,
			&completedAtStr,
		)
		if err == nil {
			break
		}
		if err == sql.ErrNoRows {
			return nil, nil // No pending jobs matching types
		}
		if !strings.Contains(err.Error(), "locked") && !strings.Contains(err.Error(), "busy") {
			return nil, err
		}
		time.Sleep(time.Duration(10*(attempt+1)) * time.Millisecond)
	}

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	j.CreatedAt = parseTime(createdAtStr)
	j.UpdatedAt = parseTime(updatedAtStr)
	if completedAtStr.Valid {
		t := parseTime(completedAtStr.String)
		j.CompletedAt = &t
	}

	return &j, nil
}

// CountByStatus returns a map of count by job status.
func (s *JobStore) CountByStatus(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		counts[status] = count
	}
	return counts, nil
}

func parseTime(str string) time.Time {
	if t, err := time.Parse("2006-01-02 15:04:05", str); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, str); err == nil {
		return t
	}
	return time.Now()
}
