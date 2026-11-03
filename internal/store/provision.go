package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ProvisionJobRecord is the durable row of one on-demand creation job
// (plan P1-9). Provision jobs live in memory while they run; this row is what
// a restart reads back, so a request the platform already accepted keeps
// being followed instead of being asked for a second time.
type ProvisionJobRecord struct {
	Name string
	// PartnerID owns the request.
	PartnerID string
	// Path is the pipeline that drives the job: "ssh" (create → wait → SSH →
	// onboarding) or "self" (the container carries its own bootstrap).
	Path string
	// State mirrors the in-memory tracker: QUEUED, CREATING, WAITING_SSH,
	// HANDING_OVER, MANAGED, FAILED or CANCELLED.
	State string
	// Stage is the human-readable progress note shown in the console.
	Stage string
	// CreationKey is the opaque poll key the partner handed back for the
	// create request. Without it a restart cannot follow a request the
	// platform has already accepted; re-creating under the same name may be
	// refused and re-creating under a fresh name would allocate a second
	// container. It is persisted the moment CreateInstance returns.
	CreationKey string
	// InstanceID is the platform id once WaitCreation resolved it.
	InstanceID string
	// LastError is the most recent failure or hold reason.
	LastError string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ProvisionJobPersister is the optional store capability that makes provision
// jobs survive control plane restarts. It is deliberately separate from the
// core Store contract: provisioning is an API-surface concern, the read-mostly
// contract the controller depends on stays untouched, and test fakes keep
// working without knowing about provisions. SQLiteStore implements it; callers
// discover it with a type assertion.
type ProvisionJobPersister interface {
	SaveProvisionJob(ctx context.Context, job ProvisionJobRecord) error
	// DeleteProvisionJob removes a row. Rename moves a job rather than
	// duplicating it, so the old name must not linger.
	DeleteProvisionJob(ctx context.Context, name string) error
	// ListProvisionJobs returns every tracked job, oldest first. Terminal
	// rows are kept on purpose: they are the reconciliation record of what
	// this control plane asked the platform for.
	ListProvisionJobs(ctx context.Context) ([]ProvisionJobRecord, error)
}

const provisionJobColumns = `name, partner_id, path, state, stage, creation_key, instance_id, last_error, created_at, updated_at`

// SaveProvisionJob upserts one provision job row.
func (s *SQLiteStore) SaveProvisionJob(ctx context.Context, job ProvisionJobRecord) error {
	if job.Name == "" {
		return errors.New("provision job requires a name")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO provision_jobs (`+provisionJobColumns+`)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
    partner_id = excluded.partner_id,
    path = excluded.path,
    state = excluded.state,
    stage = excluded.stage,
    creation_key = excluded.creation_key,
    instance_id = excluded.instance_id,
    last_error = excluded.last_error,
    updated_at = excluded.updated_at`,
		job.Name, job.PartnerID, job.Path, job.State, job.Stage,
		job.CreationKey, job.InstanceID, job.LastError,
		FormatTime(job.CreatedAt), FormatTime(job.UpdatedAt))
	if err != nil {
		return fmt.Errorf("save provision job %s: %w", job.Name, err)
	}
	return nil
}

// DeleteProvisionJob removes one row.
func (s *SQLiteStore) DeleteProvisionJob(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM provision_jobs WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete provision job %s: %w", name, err)
	}
	return nil
}

// ListProvisionJobs returns every tracked job, oldest first.
func (s *SQLiteStore) ListProvisionJobs(ctx context.Context) ([]ProvisionJobRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+provisionJobColumns+` FROM provision_jobs ORDER BY created_at, name`)
	if err != nil {
		return nil, fmt.Errorf("list provision jobs: %w", err)
	}
	defer rows.Close()
	var jobs []ProvisionJobRecord
	for rows.Next() {
		job, err := scanProvisionJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func scanProvisionJob(row rowScanner) (ProvisionJobRecord, error) {
	var (
		job         ProvisionJobRecord
		stage       sql.NullString
		creationKey sql.NullString
		instanceID  sql.NullString
		lastError   sql.NullString
		createdAt   string
		updatedAt   string
	)
	if err := row.Scan(&job.Name, &job.PartnerID, &job.Path, &job.State, &stage,
		&creationKey, &instanceID, &lastError, &createdAt, &updatedAt); err != nil {
		return ProvisionJobRecord{}, err
	}
	job.Stage = stage.String
	job.CreationKey = creationKey.String
	job.InstanceID = instanceID.String
	job.LastError = lastError.String
	var err error
	if job.CreatedAt, err = ParseTime(createdAt); err != nil {
		return ProvisionJobRecord{}, err
	}
	if job.UpdatedAt, err = ParseTime(updatedAt); err != nil {
		return ProvisionJobRecord{}, err
	}
	return job, nil
}
