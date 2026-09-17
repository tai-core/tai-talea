// Package store persists control plane intent and the audit trail.
//
// SQLite is the source of truth for control intent; the container, SGLang and
// the Router remain the sources of truth for actual state (development
// document §13). Every state change goes through ApplyTransition so that
// idempotency, transition validation, the instance row, the operation row and
// the audit row commit atomically.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
)

// Sentinel errors returned by Store implementations.
var (
	// ErrNotFound means the requested record does not exist.
	ErrNotFound = errors.New("record not found")
	// ErrConflict means the compare-and-swap guard rejected the transition.
	ErrConflict = errors.New("instance state changed concurrently")
	// ErrDuplicateEvent means the event id was already claimed.
	ErrDuplicateEvent = errors.New("capacity event already recorded")
)

// OperationType enumerates the long running lifecycle operations.
type OperationType string

const (
	OpPrepare  OperationType = "PREPARE"
	OpStart    OperationType = "START"
	OpRegister OperationType = "REGISTER"
	OpDrain    OperationType = "DRAIN"
	OpRelease  OperationType = "RELEASE"
	OpRetire   OperationType = "RETIRE"
)

// OperationStatus is the durable status of one operation row.
type OperationStatus string

const (
	OpPending    OperationStatus = "PENDING"
	OpInProgress OperationStatus = "IN_PROGRESS"
	OpSucceeded  OperationStatus = "SUCCEEDED"
	OpFailed     OperationStatus = "FAILED"
)

// Terminal reports whether the operation no longer needs to be resumed.
func (s OperationStatus) Terminal() bool { return s == OpSucceeded || s == OpFailed }

// Operation is one resumable lifecycle action.
type Operation struct {
	OperationID string          `json:"operation_id"`
	InstanceID  string          `json:"instance_id"`
	Type        OperationType   `json:"operation_type"`
	Status      OperationStatus `json:"status"`
	Attempt     int             `json:"attempt"`
	NextRetryAt time.Time       `json:"next_retry_at,omitempty"`
	LastError   string          `json:"last_error,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// EventRecord is the durable audit record of one received capacity event.
type EventRecord struct {
	EventID     string             `json:"event_id"`
	PartnerID   string             `json:"partner_id"`
	EventType   domain.EventType   `json:"event_type"`
	PayloadJSON string             `json:"payload_json"`
	Source      domain.EventSource `json:"source"`
	OccurredAt  time.Time          `json:"occurred_at"`
	ReceivedAt  time.Time          `json:"received_at"`
	ProcessedAt time.Time          `json:"processed_at,omitempty"`
	Result      domain.EventResult `json:"result"`
	Reason      string             `json:"reason,omitempty"`
}

// SnapshotRecord is the last successfully validated partner snapshot.
type SnapshotRecord struct {
	PartnerID     string    `json:"partner_id"`
	Version       string    `json:"snapshot_version"`
	PayloadJSON   string    `json:"payload_json"`
	ObservedAt    time.Time `json:"observed_at"`
	Status        string    `json:"status"`
	InstanceCount int       `json:"instance_count"`
}

// Snapshot statuses.
const (
	SnapshotStatusOK      = "OK"
	SnapshotStatusInvalid = "INVALID"
)

// AuditEntry is one lifecycle action written to the audit log.
type AuditEntry struct {
	OccurredAt time.Time         `json:"occurred_at"`
	Actor      string            `json:"actor"`
	Action     string            `json:"action"`
	InstanceID string            `json:"instance_id,omitempty"`
	PartnerID  string            `json:"partner_id,omitempty"`
	EventID    string            `json:"event_id,omitempty"`
	Details    map[string]string `json:"details,omitempty"`
}

// StateCount is one aggregation bucket.
type StateCount struct {
	Partner string
	State   string
	Count   int
}

// RoleStateCount is one service aggregation bucket.
type RoleStateCount struct {
	Role  string
	State string
	Count int
}

// Transition is one atomic state change request.
type Transition struct {
	// InstanceID identifies the row to update.
	InstanceID string
	// Next is the desired instance row. CreatedAt/UpdatedAt are overwritten.
	Next domain.Instance

	// ExpectInstanceState and ExpectServiceState form a compare-and-swap guard.
	// Empty values skip the corresponding check. When the instance row does not
	// exist yet, the only accepted Next.InstanceState is ALLOCATING.
	ExpectInstanceState domain.InstanceState
	ExpectServiceState  domain.ServiceState

	// Operation, when set, is upserted in the same transaction.
	Operation *Operation

	// Audit rows the action in the same transaction.
	Audit AuditEntry
}

// Store is the persistence contract used by the controller and the reconciler.
type Store interface {
	Close() error

	ClaimEvent(ctx context.Context, event domain.CapacityEvent, result domain.EventResult, reason string) (bool, error)
	CompleteEvent(ctx context.Context, eventID string, result domain.EventResult, reason string) error
	GetEvent(ctx context.Context, eventID string) (EventRecord, error)
	ListRecentEvents(ctx context.Context, limit int) ([]EventRecord, error)
	AbandonStaleEvents(ctx context.Context, olderThan time.Time) (int, error)

	GetInstance(ctx context.Context, id string) (domain.Instance, error)
	ListInstances(ctx context.Context) ([]domain.Instance, error)
	CountInstancesByState(ctx context.Context) ([]StateCount, error)
	CountServicesByRoleState(ctx context.Context) ([]RoleStateCount, error)

	ApplyTransition(ctx context.Context, transition Transition) error

	SaveSnapshot(ctx context.Context, snapshot SnapshotRecord) error
	GetSnapshot(ctx context.Context, partnerID string) (SnapshotRecord, bool, error)

	ListOpenOperations(ctx context.Context, limit int) ([]Operation, error)
	GetOperation(ctx context.Context, operationID string) (Operation, error)
	CompleteOperation(ctx context.Context, operationID string, status OperationStatus, detail string, nextRetryAt time.Time) error

	AppendAudit(ctx context.Context, entry AuditEntry) error
	ListAudit(ctx context.Context, instanceID string, limit int) ([]AuditEntry, error)
}

// TimeLayout is the on-disk timestamp layout. UTC RFC3339 with nanoseconds
// keeps ordering stable and human readable when inspecting the database.
const TimeLayout = time.RFC3339Nano

// FormatTime renders a timestamp for SQLite storage.
func FormatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(TimeLayout)
}

// ParseTime reads a timestamp written by FormatTime. Empty values yield the
// zero time so that nullable columns round-trip cleanly.
func ParseTime(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(TimeLayout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", value, err)
	}
	return parsed.UTC(), nil
}
