package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	// Pure-Go SQLite driver: keeps the control plane a single cgo-free binary,
	// which matters because the control plane runs next to the Router.
	_ "modernc.org/sqlite"

	"github.com/tai-core/tai-talea/internal/domain"
)

// SQLiteStore is the default Store implementation.
type SQLiteStore struct {
	db *sql.DB
}

// OpenSQLite opens (and migrates) the control plane database. A single
// connection is used on purpose: the control plane is the only writer, and a
// serialized writer removes busy-retry noise on Windows and NFS-backed volumes.
func OpenSQLite(path string) (*SQLiteStore, error) {
	if path == "" {
		return nil, errors.New("sqlite path is required")
	}
	directory := filepath.Dir(path)
	if directory != "" && directory != "." {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return nil, fmt.Errorf("create sqlite directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA foreign_keys = ON",
	}
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	store := &SQLiteStore{db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

// Close releases the database handle.
func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *SQLiteStore) migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()

	var current int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if current >= schemaVersion {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, schemaDDL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("bump user_version: %w", err)
	}
	return tx.Commit()
}

// ---------------------------------------------------------------- events

// ClaimEvent records an event as PENDING. The primary key on event_id makes
// duplicate delivery idempotent: the caller receives claimed=false and must not
// trigger any lifecycle action again.
func (s *SQLiteStore) ClaimEvent(ctx context.Context, event domain.CapacityEvent, result domain.EventResult, reason string) (bool, error) {
	payload := event.Raw
	if len(payload) == 0 {
		encoded, err := json.Marshal(event)
		if err != nil {
			return false, fmt.Errorf("encode event payload: %w", err)
		}
		payload = encoded
	}
	receivedAt := event.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = time.Now().UTC()
	}
	outcome, err := s.db.ExecContext(ctx, `
INSERT INTO capacity_events
    (event_id, partner_id, event_type, payload_json, source, occurred_at, received_at, result, reason)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(event_id) DO NOTHING`,
		event.EventID,
		event.PartnerID,
		string(event.Type),
		string(payload),
		string(event.Source),
		FormatTime(event.OccurredAt),
		FormatTime(receivedAt),
		string(result),
		reason,
	)
	if err != nil {
		return false, fmt.Errorf("claim event %s: %w", event.EventID, err)
	}
	affected, err := outcome.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim event %s: %w", event.EventID, err)
	}
	return affected > 0, nil
}

// CompleteEvent finalizes the durable result of one event.
func (s *SQLiteStore) CompleteEvent(ctx context.Context, eventID string, result domain.EventResult, reason string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE capacity_events SET result = ?, reason = ?, processed_at = ? WHERE event_id = ?`,
		string(result), reason, FormatTime(time.Now()), eventID)
	if err != nil {
		return fmt.Errorf("complete event %s: %w", eventID, err)
	}
	return nil
}

// GetEvent reads one event record.
func (s *SQLiteStore) GetEvent(ctx context.Context, eventID string) (EventRecord, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT event_id, partner_id, event_type, payload_json, source, occurred_at, received_at, processed_at, result, reason
FROM capacity_events WHERE event_id = ?`, eventID)
	record, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return EventRecord{}, fmt.Errorf("%w: event %s", ErrNotFound, eventID)
	}
	return record, err
}

// ListRecentEvents returns the newest events, newest first.
func (s *SQLiteStore) ListRecentEvents(ctx context.Context, limit int) ([]EventRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT event_id, partner_id, event_type, payload_json, source, occurred_at, received_at, processed_at, result, reason
FROM capacity_events ORDER BY received_at DESC, event_id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	var records []EventRecord
	for rows.Next() {
		record, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// AbandonStaleEvents marks events that were claimed but never finalized during
// a previous process lifetime. They are never replayed automatically: the
// reconciler rebuilds actual state from the container, SGLang and the Router.
func (s *SQLiteStore) AbandonStaleEvents(ctx context.Context, olderThan time.Time) (int, error) {
	outcome, err := s.db.ExecContext(ctx, `
UPDATE capacity_events
SET result = ?, reason = 'control plane restarted while processing', processed_at = ?
WHERE result = ? AND received_at < ?`,
		string(ResultAbandoned()), FormatTime(time.Now()), string(domain.ResultPending), FormatTime(olderThan))
	if err != nil {
		return 0, fmt.Errorf("abandon stale events: %w", err)
	}
	affected, _ := outcome.RowsAffected()
	return int(affected), nil
}

// ResultAbandoned exposes the abandoned result value for SQL parameters.
func ResultAbandoned() domain.EventResult { return domain.ResultAbandoned }

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEvent(row rowScanner) (EventRecord, error) {
	var (
		record     EventRecord
		eventType  string
		source     string
		result     string
		occurredAt string
		receivedAt string
		processed  sql.NullString
		reason     sql.NullString
	)
	if err := row.Scan(&record.EventID, &record.PartnerID, &eventType, &record.PayloadJSON,
		&source, &occurredAt, &receivedAt, &processed, &result, &reason); err != nil {
		return EventRecord{}, err
	}
	record.EventType = domain.EventType(eventType)
	record.Source = domain.EventSource(source)
	record.Result = domain.EventResult(result)
	record.Reason = reason.String
	var err error
	if record.OccurredAt, err = ParseTime(occurredAt); err != nil {
		return EventRecord{}, err
	}
	if record.ReceivedAt, err = ParseTime(receivedAt); err != nil {
		return EventRecord{}, err
	}
	if record.ProcessedAt, err = ParseTime(processed.String); err != nil {
		return EventRecord{}, err
	}
	return record, nil
}

// ------------------------------------------------------------- instances

const instanceColumns = `id, partner_id, endpoint, lease_id, spec_json, instance_state, service_state,
	role, role_assigned_at, router_worker_id, readiness_generation, prepare_attempts, start_attempts,
	drain_deadline_at, pending_release, lease_updated_at, last_error, last_seen_at, created_at, updated_at`

// GetInstance reads one instance row.
func (s *SQLiteStore) GetInstance(ctx context.Context, id string) (domain.Instance, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+instanceColumns+" FROM capacity_instances WHERE id = ?", id)
	instance, err := scanInstance(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Instance{}, fmt.Errorf("%w: instance %s", ErrNotFound, id)
	}
	return instance, err
}

// ListInstances returns every tracked instance ordered by id.
func (s *SQLiteStore) ListInstances(ctx context.Context) ([]domain.Instance, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+instanceColumns+" FROM capacity_instances ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	defer rows.Close()
	var instances []domain.Instance
	for rows.Next() {
		instance, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		instances = append(instances, instance)
	}
	return instances, rows.Err()
}

// CountInstancesByState aggregates container states, excluding RELEASED rows
// which have already left the control plane.
func (s *SQLiteStore) CountInstancesByState(ctx context.Context) ([]StateCount, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT partner_id, instance_state, COUNT(*)
FROM capacity_instances
WHERE instance_state <> ?
GROUP BY partner_id, instance_state`, string(domain.InstanceReleased))
	if err != nil {
		return nil, fmt.Errorf("count instances: %w", err)
	}
	defer rows.Close()
	var counts []StateCount
	for rows.Next() {
		var count StateCount
		if err := rows.Scan(&count.Partner, &count.State, &count.Count); err != nil {
			return nil, err
		}
		counts = append(counts, count)
	}
	return counts, rows.Err()
}

// CountServicesByRoleState aggregates service states that are not NONE.
func (s *SQLiteStore) CountServicesByRoleState(ctx context.Context) ([]RoleStateCount, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(role, ''), service_state, COUNT(*)
FROM capacity_instances
WHERE service_state <> ?
GROUP BY COALESCE(role, ''), service_state`, string(domain.ServiceNone))
	if err != nil {
		return nil, fmt.Errorf("count services: %w", err)
	}
	defer rows.Close()
	var counts []RoleStateCount
	for rows.Next() {
		var count RoleStateCount
		if err := rows.Scan(&count.Role, &count.State, &count.Count); err != nil {
			return nil, err
		}
		counts = append(counts, count)
	}
	return counts, rows.Err()
}

func scanInstance(row rowScanner) (domain.Instance, error) {
	var (
		instance       domain.Instance
		specJSON       string
		instanceState  string
		serviceState   string
		role           sql.NullString
		roleAssignedAt sql.NullString
		leaseID        sql.NullString
		routerWorkerID sql.NullString
		drainDeadline  sql.NullString
		pendingRelease int
		leaseUpdated   sql.NullString
		lastError      sql.NullString
		lastSeen       sql.NullString
		createdAt      string
		updatedAt      string
	)
	if err := row.Scan(
		&instance.ID, &instance.PartnerID, &instance.Endpoint, &leaseID, &specJSON,
		&instanceState, &serviceState, &role, &roleAssignedAt, &routerWorkerID, &instance.ReadinessGeneration,
		&instance.PrepareAttempts, &instance.StartAttempts, &drainDeadline, &pendingRelease, &leaseUpdated,
		&lastError, &lastSeen, &createdAt, &updatedAt,
	); err != nil {
		return domain.Instance{}, err
	}
	instance.LeaseID = leaseID.String
	instance.Role = domain.Role(role.String)
	instance.RouterWorkerID = routerWorkerID.String
	instance.InstanceState = domain.InstanceState(instanceState)
	instance.ServiceState = domain.ServiceState(serviceState)
	instance.LastError = lastError.String
	instance.PendingRelease = pendingRelease != 0
	if specJSON != "" {
		if err := json.Unmarshal([]byte(specJSON), &instance.Spec); err != nil {
			return domain.Instance{}, fmt.Errorf("decode spec of instance %s: %w", instance.ID, err)
		}
	}
	var err error
	if instance.RoleAssignedAt, err = ParseTime(roleAssignedAt.String); err != nil {
		return domain.Instance{}, err
	}
	if instance.DrainDeadlineAt, err = ParseTime(drainDeadline.String); err != nil {
		return domain.Instance{}, err
	}
	if instance.LeaseUpdatedAt, err = ParseTime(leaseUpdated.String); err != nil {
		return domain.Instance{}, err
	}
	if instance.LastSeenAt, err = ParseTime(lastSeen.String); err != nil {
		return domain.Instance{}, err
	}
	if instance.CreatedAt, err = ParseTime(createdAt); err != nil {
		return domain.Instance{}, err
	}
	if instance.UpdatedAt, err = ParseTime(updatedAt); err != nil {
		return domain.Instance{}, err
	}
	return instance, nil
}

// ------------------------------------------------------------ transition

// ApplyTransition atomically validates and applies one state change.
//
// Ordering follows development document §12: the instance row, the operation
// row and the audit row commit together, or nothing does.
func (s *SQLiteStore) ApplyTransition(ctx context.Context, transition Transition) error {
	if transition.InstanceID == "" {
		return errors.New("transition requires an instance id")
	}
	if transition.Next.ID != "" && transition.Next.ID != transition.InstanceID {
		return fmt.Errorf("transition instance id %q does not match payload id %q", transition.InstanceID, transition.Next.ID)
	}
	next := transition.Next
	next.ID = transition.InstanceID

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transition: %w", err)
	}
	defer tx.Rollback()

	current, err := getInstanceTx(ctx, tx, transition.InstanceID)
	switch {
	case errors.Is(err, ErrNotFound):
		if next.InstanceState != domain.InstanceAllocating {
			return fmt.Errorf("%w: instance %s", ErrNotFound, transition.InstanceID)
		}
		if err := insertInstanceTx(ctx, tx, next); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if transition.ExpectInstanceState != "" && current.InstanceState != transition.ExpectInstanceState {
			return fmt.Errorf("%w: instance %s is %s, expected %s",
				ErrConflict, transition.InstanceID, current.InstanceState, transition.ExpectInstanceState)
		}
		if transition.ExpectServiceState != "" && current.ServiceState != transition.ExpectServiceState {
			return fmt.Errorf("%w: instance %s service is %s, expected %s",
				ErrConflict, transition.InstanceID, current.ServiceState, transition.ExpectServiceState)
		}
		if err := domain.ValidateTransition(current, next); err != nil {
			return err
		}
		if verdict := domain.CheckCombination(next.InstanceState, next.ServiceState, next.Role); !verdict.Legal {
			return domain.NewViolation(verdict.Code, verdict.Severity, "refuse %s -> %s/%s/%s: %s",
				current, next.InstanceState, next.ServiceState, next.Role, verdict.Reason)
		}
		if err := updateInstanceTx(ctx, tx, current, next); err != nil {
			return err
		}
	}

	if transition.Operation != nil {
		if err := upsertOperationTx(ctx, tx, *transition.Operation); err != nil {
			return err
		}
	}
	if transition.Audit.Action != "" {
		if err := appendAuditTx(ctx, tx, transition.Audit); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func getInstanceTx(ctx context.Context, tx *sql.Tx, id string) (domain.Instance, error) {
	row := tx.QueryRowContext(ctx, "SELECT "+instanceColumns+" FROM capacity_instances WHERE id = ?", id)
	instance, err := scanInstance(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Instance{}, fmt.Errorf("%w: instance %s", ErrNotFound, id)
	}
	return instance, err
}

func insertInstanceTx(ctx context.Context, tx *sql.Tx, instance domain.Instance) error {
	specJSON, err := json.Marshal(instance.Spec)
	if err != nil {
		return fmt.Errorf("encode spec: %w", err)
	}
	now := time.Now().UTC()
	if instance.CreatedAt.IsZero() {
		instance.CreatedAt = now
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO capacity_instances (
    id, partner_id, endpoint, lease_id, spec_json, instance_state, service_state, role, role_assigned_at,
    router_worker_id, readiness_generation, prepare_attempts, start_attempts,
    drain_deadline_at, pending_release, lease_updated_at, last_error, last_seen_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		instance.ID, instance.PartnerID, instance.Endpoint, nullString(instance.LeaseID), string(specJSON),
		string(instance.InstanceState), string(instance.ServiceState), nullString(string(instance.Role)),
		nullTime(instance.RoleAssignedAt), nullString(instance.RouterWorkerID), instance.ReadinessGeneration,
		instance.PrepareAttempts, instance.StartAttempts, nullTime(instance.DrainDeadlineAt),
		boolInt(instance.PendingRelease), nullTime(instance.LeaseUpdatedAt),
		nullString(instance.LastError), nullTime(instance.LastSeenAt),
		FormatTime(instance.CreatedAt), FormatTime(now))
	if err != nil {
		return fmt.Errorf("insert instance %s: %w", instance.ID, err)
	}
	return nil
}

func updateInstanceTx(ctx context.Context, tx *sql.Tx, current, next domain.Instance) error {
	if next.CreatedAt.IsZero() {
		next.CreatedAt = current.CreatedAt
	}
	specJSON, err := json.Marshal(next.Spec)
	if err != nil {
		return fmt.Errorf("encode spec: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
UPDATE capacity_instances SET
    partner_id = ?, endpoint = ?, lease_id = ?, spec_json = ?, instance_state = ?, service_state = ?,
    role = ?, role_assigned_at = ?, router_worker_id = ?, readiness_generation = ?, prepare_attempts = ?,
    start_attempts = ?, drain_deadline_at = ?, pending_release = ?, lease_updated_at = ?, last_error = ?,
    last_seen_at = ?, updated_at = ?
WHERE id = ?`,
		next.PartnerID, next.Endpoint, nullString(next.LeaseID), string(specJSON),
		string(next.InstanceState), string(next.ServiceState), nullString(string(next.Role)),
		nullTime(next.RoleAssignedAt), nullString(next.RouterWorkerID), next.ReadinessGeneration,
		next.PrepareAttempts, next.StartAttempts, nullTime(next.DrainDeadlineAt),
		boolInt(next.PendingRelease), nullTime(next.LeaseUpdatedAt), nullString(next.LastError),
		nullTime(next.LastSeenAt), FormatTime(time.Now()), next.ID)
	if err != nil {
		return fmt.Errorf("update instance %s: %w", next.ID, err)
	}
	return nil
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return FormatTime(value)
}

// ------------------------------------------------------------- snapshots

// SaveSnapshot stores the newest validated partner snapshot.
func (s *SQLiteStore) SaveSnapshot(ctx context.Context, snapshot SnapshotRecord) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO partner_snapshots (partner_id, snapshot_version, payload_json, observed_at, status, instance_count)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(partner_id) DO UPDATE SET
    snapshot_version = excluded.snapshot_version,
    payload_json = excluded.payload_json,
    observed_at = excluded.observed_at,
    status = excluded.status,
    instance_count = excluded.instance_count`,
		snapshot.PartnerID, snapshot.Version, snapshot.PayloadJSON,
		FormatTime(snapshot.ObservedAt), snapshot.Status, snapshot.InstanceCount)
	if err != nil {
		return fmt.Errorf("save snapshot for %s: %w", snapshot.PartnerID, err)
	}
	return nil
}

// GetSnapshot reads the last successfully validated snapshot.
func (s *SQLiteStore) GetSnapshot(ctx context.Context, partnerID string) (SnapshotRecord, bool, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT partner_id, snapshot_version, payload_json, observed_at, status, instance_count
FROM partner_snapshots WHERE partner_id = ?`, partnerID)
	var (
		snapshot   SnapshotRecord
		observedAt string
	)
	if err := row.Scan(&snapshot.PartnerID, &snapshot.Version, &snapshot.PayloadJSON,
		&observedAt, &snapshot.Status, &snapshot.InstanceCount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SnapshotRecord{}, false, nil
		}
		return SnapshotRecord{}, false, fmt.Errorf("read snapshot for %s: %w", partnerID, err)
	}
	parsed, err := ParseTime(observedAt)
	if err != nil {
		return SnapshotRecord{}, false, err
	}
	snapshot.ObservedAt = parsed
	return snapshot, true, nil
}

// ------------------------------------------------------------ operations

func upsertOperationTx(ctx context.Context, tx *sql.Tx, operation Operation) error {
	now := time.Now().UTC()
	if operation.CreatedAt.IsZero() {
		operation.CreatedAt = now
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO operations (operation_id, instance_id, operation_type, status, attempt, next_retry_at, last_error, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(operation_id) DO UPDATE SET
    status = excluded.status,
    attempt = excluded.attempt,
    next_retry_at = excluded.next_retry_at,
    last_error = excluded.last_error,
    updated_at = excluded.updated_at`,
		operation.OperationID, operation.InstanceID, string(operation.Type), string(operation.Status),
		operation.Attempt, nullTime(operation.NextRetryAt), nullString(operation.LastError),
		FormatTime(operation.CreatedAt), FormatTime(now))
	if err != nil {
		return fmt.Errorf("upsert operation %s: %w", operation.OperationID, err)
	}
	return nil
}

// ListOpenOperations returns resumable operations whose retry time has passed.
func (s *SQLiteStore) ListOpenOperations(ctx context.Context, limit int) ([]Operation, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT operation_id, instance_id, operation_type, status, attempt, next_retry_at, last_error, created_at, updated_at
FROM operations
WHERE status IN (?, ?)
ORDER BY created_at
LIMIT ?`, string(OpPending), string(OpInProgress), limit)
	if err != nil {
		return nil, fmt.Errorf("list operations: %w", err)
	}
	defer rows.Close()
	var operations []Operation
	for rows.Next() {
		operation, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		operations = append(operations, operation)
	}
	return operations, rows.Err()
}

// GetOperation reads one operation row.
func (s *SQLiteStore) GetOperation(ctx context.Context, operationID string) (Operation, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT operation_id, instance_id, operation_type, status, attempt, next_retry_at, last_error, created_at, updated_at
FROM operations WHERE operation_id = ?`, operationID)
	operation, err := scanOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, fmt.Errorf("%w: operation %s", ErrNotFound, operationID)
	}
	return operation, err
}

// CompleteOperation finalizes an operation row.
func (s *SQLiteStore) CompleteOperation(ctx context.Context, operationID string, status OperationStatus, detail string, nextRetryAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE operations SET status = ?, last_error = ?, next_retry_at = ?, updated_at = ? WHERE operation_id = ?`,
		string(status), detail, nullTime(nextRetryAt), FormatTime(time.Now()), operationID)
	if err != nil {
		return fmt.Errorf("complete operation %s: %w", operationID, err)
	}
	return nil
}

func scanOperation(row rowScanner) (Operation, error) {
	var (
		operation Operation
		opType    string
		status    string
		nextRetry sql.NullString
		lastError sql.NullString
		createdAt string
		updatedAt string
	)
	if err := row.Scan(&operation.OperationID, &operation.InstanceID, &opType, &status,
		&operation.Attempt, &nextRetry, &lastError, &createdAt, &updatedAt); err != nil {
		return Operation{}, err
	}
	operation.Type = OperationType(opType)
	operation.Status = OperationStatus(status)
	operation.LastError = lastError.String
	var err error
	if operation.NextRetryAt, err = ParseTime(nextRetry.String); err != nil {
		return Operation{}, err
	}
	if operation.CreatedAt, err = ParseTime(createdAt); err != nil {
		return Operation{}, err
	}
	if operation.UpdatedAt, err = ParseTime(updatedAt); err != nil {
		return Operation{}, err
	}
	return operation, nil
}

// ------------------------------------------------------------- audit log

// AppendAudit writes one audit record outside a transition.
func (s *SQLiteStore) AppendAudit(ctx context.Context, entry AuditEntry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin audit write: %w", err)
	}
	defer tx.Rollback()
	if err := appendAuditTx(ctx, tx, entry); err != nil {
		return err
	}
	return tx.Commit()
}

func appendAuditTx(ctx context.Context, tx *sql.Tx, entry AuditEntry) error {
	if entry.OccurredAt.IsZero() {
		entry.OccurredAt = time.Now().UTC()
	}
	details := "{}"
	if len(entry.Details) > 0 {
		encoded, err := json.Marshal(entry.Details)
		if err != nil {
			return fmt.Errorf("encode audit details: %w", err)
		}
		details = string(encoded)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO audit_log (occurred_at, actor, action, instance_id, partner_id, event_id, details_json)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		FormatTime(entry.OccurredAt), entry.Actor, entry.Action,
		nullString(entry.InstanceID), nullString(entry.PartnerID), nullString(entry.EventID), details); err != nil {
		return fmt.Errorf("append audit %s: %w", entry.Action, err)
	}
	return nil
}

// ListAudit returns the newest audit records, newest first.
func (s *SQLiteStore) ListAudit(ctx context.Context, instanceID string, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	query := `SELECT occurred_at, actor, action, instance_id, partner_id, event_id, details_json FROM audit_log`
	args := []any{}
	if instanceID != "" {
		query += " WHERE instance_id = ?"
		args = append(args, instanceID)
	}
	query += " ORDER BY audit_id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()
	var entries []AuditEntry
	for rows.Next() {
		var (
			entry      AuditEntry
			occurredAt string
			instance   sql.NullString
			partner    sql.NullString
			eventID    sql.NullString
			details    string
		)
		if err := rows.Scan(&occurredAt, &entry.Actor, &entry.Action, &instance, &partner, &eventID, &details); err != nil {
			return nil, err
		}
		entry.InstanceID = instance.String
		entry.PartnerID = partner.String
		entry.EventID = eventID.String
		if details != "" && details != "{}" {
			if err := json.Unmarshal([]byte(details), &entry.Details); err != nil {
				return nil, fmt.Errorf("decode audit details: %w", err)
			}
		}
		parsed, err := ParseTime(occurredAt)
		if err != nil {
			return nil, err
		}
		entry.OccurredAt = parsed
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}
