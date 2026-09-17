package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// EventType is the unified capacity event kind.
type EventType string

const (
	EventCapacityAdded             EventType = "CAPACITY_ADDED"
	EventCapacityRevoked           EventType = "CAPACITY_REVOKED"
	EventCapacityUpdated           EventType = "CAPACITY_UPDATED"
	EventCapacitySnapshotConfirmed EventType = "CAPACITY_SNAPSHOT_CONFIRMED"
)

// KnownEventType reports whether the value is a defined capacity event type.
func KnownEventType(kind EventType) bool {
	switch kind {
	case EventCapacityAdded, EventCapacityRevoked, EventCapacityUpdated, EventCapacitySnapshotConfirmed:
		return true
	}
	return false
}

// EventSource records where an event came from. Pull results are the authority;
// Push only accelerates them (development document §5 and §6).
type EventSource string

const (
	SourcePush EventSource = "push"
	SourcePull EventSource = "pull"
)

// EventResult is the durable processing outcome of one capacity event.
type EventResult string

const (
	ResultPending   EventResult = "PENDING"
	ResultApplied   EventResult = "APPLIED"
	ResultDuplicate EventResult = "DUPLICATE"
	ResultRejected  EventResult = "REJECTED"
	ResultFailed    EventResult = "FAILED"
	ResultAbandoned EventResult = "ABANDONED"
)

// Limits protecting the Push API and the SQLite layer.
const (
	MaxEventIDLength    = 256
	MaxPartnerIDLength  = 128
	MaxInstanceIDLength = 256
	MaxEndpointLength   = 512
	MaxLeaseIDLength    = 256
	MaxModelIDLength    = 256
	MaxModelSupportSize = 64
	MaxGraceSeconds     = 24 * 3600
)

// EventInstance is the instance payload carried by a capacity event. Fields are
// optional so that a revoke event can carry only the id.
type EventInstance struct {
	ID       string `json:"id"`
	Endpoint string `json:"endpoint,omitempty"`
	// ServiceEndpoint is the address the Router should route to, for partners
	// that publish the SGLang service on a different port than the bootstrap
	// control interface. Optional; omitted means "same as Endpoint".
	ServiceEndpoint string        `json:"service_endpoint,omitempty"`
	LeaseID         string        `json:"lease_id,omitempty"`
	Spec            *InstanceSpec `json:"spec,omitempty"`
}

// Snapshot is the payload of CAPACITY_SNAPSHOT_CONFIRMED and the normalized
// result of a Pull reconciliation round.
type Snapshot struct {
	Version   string          `json:"version"`
	Instances []EventInstance `json:"instances"`
	Observed  time.Time       `json:"observed_at,omitempty"`
}

// CapacityEvent is the single normalized event shape used by both Push and Pull.
type CapacityEvent struct {
	EventID      string         `json:"event_id"`
	PartnerID    string         `json:"partner_id"`
	Type         EventType      `json:"type"`
	OccurredAt   time.Time      `json:"occurred_at"`
	Instance     *EventInstance `json:"instance,omitempty"`
	GraceSeconds int            `json:"grace_seconds,omitempty"`
	Snapshot     *Snapshot      `json:"snapshot,omitempty"`

	// Source and ReceivedAt are filled by the receiver, never by the partner.
	Source     EventSource     `json:"-"`
	ReceivedAt time.Time       `json:"-"`
	Raw        json.RawMessage `json:"-"`
}

// Validate checks the structural contract that every capacity event must honour
// before it is allowed to touch the state machine.
func (e CapacityEvent) Validate() error {
	if strings.TrimSpace(e.EventID) != e.EventID || e.EventID == "" {
		return fmt.Errorf("event_id is required and must not be padded")
	}
	if len(e.EventID) > MaxEventIDLength {
		return fmt.Errorf("event_id exceeds %d characters", MaxEventIDLength)
	}
	if strings.ContainsAny(e.EventID, "\x00\r\n") {
		return fmt.Errorf("event_id contains control characters")
	}
	if strings.TrimSpace(e.PartnerID) != e.PartnerID || e.PartnerID == "" {
		return fmt.Errorf("partner_id is required and must not be padded")
	}
	if len(e.PartnerID) > MaxPartnerIDLength {
		return fmt.Errorf("partner_id exceeds %d characters", MaxPartnerIDLength)
	}
	if !KnownEventType(e.Type) {
		return fmt.Errorf("unknown capacity event type %q", e.Type)
	}
	if e.OccurredAt.IsZero() {
		return fmt.Errorf("occurred_at is required")
	}
	if e.Type == EventCapacitySnapshotConfirmed {
		if e.Snapshot == nil {
			return fmt.Errorf("CAPACITY_SNAPSHOT_CONFIRMED requires a snapshot payload")
		}
		if err := e.Snapshot.Validate(); err != nil {
			return err
		}
	}
	if e.GraceSeconds < 0 || e.GraceSeconds > MaxGraceSeconds {
		return fmt.Errorf("grace_seconds must be between 0 and %d", MaxGraceSeconds)
	}
	if e.Instance == nil {
		if e.Type != EventCapacitySnapshotConfirmed {
			return fmt.Errorf("%s requires an instance payload", e.Type)
		}
		return nil
	}
	return e.Instance.Validate()
}

// Validate checks the instance payload shared by events.
// validateEventEndpoint applies the same rules to the bootstrap endpoint and
// to the service endpoint: both become URLs the control plane dials, so a
// malformed one must be refused at the edge rather than at dial time.
func validateEventEndpoint(field, value string) error {
	if value == "" {
		return nil
	}
	if len(value) > MaxEndpointLength {
		return fmt.Errorf("%s exceeds %d characters", field, MaxEndpointLength)
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("%s contains control characters", field)
	}
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		return fmt.Errorf("%s must be an http(s) origin", field)
	}
	return nil
}

func (i EventInstance) Validate() error {
	if strings.TrimSpace(i.ID) != i.ID || i.ID == "" {
		return fmt.Errorf("instance.id is required and must not be padded")
	}
	if len(i.ID) > MaxInstanceIDLength {
		return fmt.Errorf("instance.id exceeds %d characters", MaxInstanceIDLength)
	}
	if strings.ContainsAny(i.ID, "\x00\r\n") {
		return fmt.Errorf("instance.id contains control characters")
	}
	if err := validateEventEndpoint("instance.endpoint", i.Endpoint); err != nil {
		return err
	}
	if err := validateEventEndpoint("instance.service_endpoint", i.ServiceEndpoint); err != nil {
		return err
	}
	if i.LeaseID != "" {
		if len(i.LeaseID) > MaxLeaseIDLength || strings.ContainsAny(i.LeaseID, "\x00\r\n") {
			return fmt.Errorf("instance.lease_id is invalid")
		}
	}
	if i.Spec != nil {
		if i.Spec.GPUCount < 0 {
			return fmt.Errorf("instance.spec.gpu_count must not be negative")
		}
		if len(i.Spec.ModelSupport) > MaxModelSupportSize {
			return fmt.Errorf("instance.spec.model_support exceeds %d entries", MaxModelSupportSize)
		}
		for _, model := range i.Spec.ModelSupport {
			if strings.TrimSpace(model) == "" || len(model) > MaxModelIDLength || strings.ContainsAny(model, "\x00\r\n") {
				return fmt.Errorf("instance.spec.model_support contains an invalid model id")
			}
		}
	}
	return nil
}

// Validate checks the snapshot payload.
func (s Snapshot) Validate() error {
	if strings.TrimSpace(s.Version) != s.Version || s.Version == "" {
		return fmt.Errorf("snapshot.version is required and must not be padded")
	}
	if len(s.Version) > 128 {
		return fmt.Errorf("snapshot.version exceeds 128 characters")
	}
	if len(s.Instances) > 10000 {
		return fmt.Errorf("snapshot contains too many instances")
	}
	seen := make(map[string]bool, len(s.Instances))
	for _, instance := range s.Instances {
		if err := instance.Validate(); err != nil {
			return err
		}
		if seen[instance.ID] {
			return fmt.Errorf("snapshot contains duplicate instance %q", instance.ID)
		}
		seen[instance.ID] = true
	}
	return nil
}

// InstanceIDs returns the snapshot instance ids in payload order.
func (s Snapshot) InstanceIDs() []string {
	ids := make([]string, 0, len(s.Instances))
	for _, instance := range s.Instances {
		ids = append(ids, instance.ID)
	}
	return ids
}

// EventOutcome is what the controller reports back to the receiver.
type EventOutcome struct {
	EventID   string      `json:"event_id"`
	Accepted  bool        `json:"accepted"`
	Duplicate bool        `json:"duplicate"`
	Result    EventResult `json:"result,omitempty"`
	Reason    string      `json:"reason,omitempty"`
}
