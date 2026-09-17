// Package partner adapts a partner container platform into the unified
// capacity event model (development document §6.2). Each partner gets its own
// PartnerAdapter; the rest of the control plane never talks to a partner API
// directly.
package partner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
)

// CapacityInstance is one container as reported by a partner.
type CapacityInstance struct {
	ID       string `json:"id"`
	Endpoint string `json:"endpoint"`
	// ServiceEndpoint is where the Router reaches SGLang. Partners that publish
	// the control interface and the service on different ports set it; leaving
	// it empty means Endpoint serves both roles.
	ServiceEndpoint string              `json:"service_endpoint,omitempty"`
	LeaseID         string              `json:"lease_id"`
	LeaseUpdatedAt  time.Time           `json:"lease_updated_at,omitempty"`
	Spec            domain.InstanceSpec `json:"spec"`
}

// ToEventInstance converts the partner view into the event payload shape.
func (c CapacityInstance) ToEventInstance() domain.EventInstance {
	spec := c.Spec
	return domain.EventInstance{
		ID:              c.ID,
		Endpoint:        c.Endpoint,
		ServiceEndpoint: c.ServiceEndpoint,
		LeaseID:         c.LeaseID,
		Spec:            &spec,
	}
}

// PartnerAdapter is the only interface the control plane requires from a
// partner platform. It is intentionally read-mostly: capacity creation is the
// partner's job, we only ever give capacity back.
type PartnerAdapter interface {
	// PartnerID is the stable partner identifier used in events and metrics.
	PartnerID() string
	// ListAvailableInstances returns containers the partner currently offers.
	ListAvailableInstances(ctx context.Context) ([]CapacityInstance, error)
	// ListActiveLeases returns the authoritative lease view. It is a superset
	// of ListAvailableInstances; anything missing here is already gone.
	ListActiveLeases(ctx context.Context) ([]CapacityInstance, error)
	// ReleaseInstance asks the partner to reclaim one container.
	ReleaseInstance(ctx context.Context, instanceID string) error
}

// Errors raised by the adapter layer.
var (
	// ErrSnapshotIncomplete means the partner returned a snapshot that fails
	// the completeness check, so it must never be treated as authoritative.
	ErrSnapshotIncomplete = errors.New("partner snapshot is incomplete")
	// ErrPartnerUnavailable means the partner API could not be reached.
	ErrPartnerUnavailable = errors.New("partner api unavailable")
)

// Snapshot is a validated, versioned view of one partner's capacity.
type Snapshot struct {
	PartnerID string             `json:"partner_id"`
	Version   string             `json:"version"`
	Observed  time.Time          `json:"observed_at"`
	Instances []CapacityInstance `json:"instances"`
}

// CollectSnapshot performs the Pull steps 1 and 2 of development document §6.2:
// read both views, then verify completeness before anything else happens.
func CollectSnapshot(ctx context.Context, adapter PartnerAdapter) (Snapshot, error) {
	partnerID := adapter.PartnerID()
	available, err := adapter.ListAvailableInstances(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: list available instances for %s: %v", ErrPartnerUnavailable, partnerID, err)
	}
	leases, err := adapter.ListActiveLeases(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: list active leases for %s: %v", ErrPartnerUnavailable, partnerID, err)
	}

	leaseIndex := make(map[string]CapacityInstance, len(leases))
	for _, instance := range leases {
		if err := validateInstance(instance); err != nil {
			return Snapshot{}, fmt.Errorf("%w: %v", ErrSnapshotIncomplete, err)
		}
		if _, duplicate := leaseIndex[instance.ID]; duplicate {
			return Snapshot{}, fmt.Errorf("%w: lease view repeats instance %s", ErrSnapshotIncomplete, instance.ID)
		}
		leaseIndex[instance.ID] = instance
	}

	merged := make(map[string]CapacityInstance, len(available))
	for _, instance := range available {
		if err := validateInstance(instance); err != nil {
			return Snapshot{}, fmt.Errorf("%w: %v", ErrSnapshotIncomplete, err)
		}
		lease, ok := leaseIndex[instance.ID]
		if !ok {
			// A container without a lease cannot be trusted: refuse the whole
			// snapshot instead of silently dropping or accepting it.
			return Snapshot{}, fmt.Errorf("%w: instance %s has no active lease", ErrSnapshotIncomplete, instance.ID)
		}
		merged[instance.ID] = mergeInstance(instance, lease)
	}

	instances := make([]CapacityInstance, 0, len(merged))
	for _, instance := range merged {
		instances = append(instances, instance)
	}
	sort.Slice(instances, func(i, j int) bool { return instances[i].ID < instances[j].ID })

	return Snapshot{
		PartnerID: partnerID,
		Version:   SnapshotVersion(instances),
		Observed:  time.Now().UTC(),
		Instances: instances,
	}, nil
}

// SnapshotVersion derives a stable version from the instance content so that an
// unchanged partner view never produces duplicate lifecycle events.
func SnapshotVersion(instances []CapacityInstance) string {
	lines := make([]string, 0, len(instances))
	for _, instance := range instances {
		models := instance.Spec.SnapshotSpec()
		lease := instance.LeaseID
		if !instance.LeaseUpdatedAt.IsZero() {
			lease = fmt.Sprintf("%s@%d", lease, instance.LeaseUpdatedAt.Unix())
		}
		lines = append(lines, fmt.Sprintf("%s|%s|%s|%s|%d|%s",
			instance.ID, instance.Endpoint, lease, instance.Spec.GPU, instance.Spec.GPUCount, strings.Join(models, ",")))
	}
	sort.Strings(lines)
	digest := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(digest[:])[:32]
}

// ParseSnapshot decodes a stored snapshot payload.
func ParseSnapshot(partnerID, version, payload string, observedAt time.Time) (Snapshot, error) {
	var stored struct {
		Instances []CapacityInstance `json:"instances"`
	}
	if err := jsonUnmarshal(payload, &stored); err != nil {
		return Snapshot{}, fmt.Errorf("decode stored snapshot for %s: %w", partnerID, err)
	}
	return Snapshot{PartnerID: partnerID, Version: version, Observed: observedAt, Instances: stored.Instances}, nil
}

// SnapshotPayload renders the persisted payload of a snapshot.
func SnapshotPayload(snapshot Snapshot) (string, error) {
	encoded, err := jsonMarshal(map[string]any{
		"partner_id":  snapshot.PartnerID,
		"version":     snapshot.Version,
		"observed_at": snapshot.Observed.UTC().Format(time.RFC3339Nano),
		"instances":   snapshot.Instances,
	})
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func validateInstance(instance CapacityInstance) error {
	payload := instance.ToEventInstance()
	if err := payload.Validate(); err != nil {
		return fmt.Errorf("partner instance is invalid: %w", err)
	}
	return nil
}

func mergeInstance(available, lease CapacityInstance) CapacityInstance {
	merged := lease
	if available.Endpoint != "" {
		merged.Endpoint = available.Endpoint
	}
	if available.ServiceEndpoint != "" {
		merged.ServiceEndpoint = available.ServiceEndpoint
	}
	if available.LeaseID != "" {
		merged.LeaseID = available.LeaseID
	}
	if !available.LeaseUpdatedAt.IsZero() {
		merged.LeaseUpdatedAt = available.LeaseUpdatedAt
	}
	if available.Spec.GPU != "" || available.Spec.GPUCount != 0 || len(available.Spec.ModelSupport) > 0 {
		merged.Spec = available.Spec
	}
	return merged
}
