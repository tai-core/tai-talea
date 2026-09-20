package partner

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/obs"
	"github.com/tai-core/tai-talea/internal/store"
)

// PullPlan is the deterministic result of one Pull reconciliation round.
// Plan has no side effects on the state machine: the caller applies Events and
// only then calls Commit, so a failure can never silently revoke capacity.
type PullPlan struct {
	PartnerID string
	Snapshot  Snapshot
	Previous  *Snapshot
	Added     []CapacityInstance
	Updated   []CapacityInstance
	Revoked   []string
	Events    []domain.CapacityEvent
	Unchanged bool
}

// Puller executes the Pull reconciliation steps of development document §6.2.
//
// Milestone 1 ships the full mechanism but keeps it inert by default: the
// documentation states that Pull becomes the authoritative capacity source in
// M2, so the controller only enables the schedule when configured to.
type Puller struct {
	Store            store.Store
	Metrics          *obs.Registry
	Alerter          *obs.Alerter
	Logger           *slog.Logger
	FailureThreshold int
	Now              func() time.Time

	mu       sync.Mutex
	failures map[string]int
}

// NewPuller builds a Puller. FailureThreshold <= 0 defaults to 3.
func NewPuller(persistence store.Store, metrics *obs.Registry, alerter *obs.Alerter, logger *slog.Logger, failureThreshold int) *Puller {
	if failureThreshold <= 0 {
		failureThreshold = 3
	}
	return &Puller{
		Store:            persistence,
		Metrics:          metrics,
		Alerter:          alerter,
		Logger:           logger,
		FailureThreshold: failureThreshold,
		Now:              func() time.Time { return time.Now().UTC() },
		failures:         map[string]int{},
	}
}

func (p *Puller) clock() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

// Failures returns the current consecutive failure count for one partner.
func (p *Puller) Failures(partnerID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failures[partnerID]
}

// Plan reads the partner snapshot and diffs it against the last successful one.
//
// On failure the previous snapshot is preserved untouched and no event is
// produced: a partner outage must never be mistaken for a mass revocation.
func (p *Puller) Plan(ctx context.Context, adapter PartnerAdapter) (PullPlan, error) {
	partnerID := adapter.PartnerID()
	snapshot, err := CollectSnapshot(ctx, adapter)
	if err != nil {
		p.recordFailure(ctx, partnerID, err)
		return PullPlan{}, err
	}
	if p.Metrics != nil {
		p.Metrics.IncCounter(obs.MetricCapacityEventsTotal,
			"Capacity events observed by the control plane, by type, source and result.",
			obs.LabelsEvent, "type", "PULL", "source", string(domain.SourcePull), "result", "SNAPSHOT_OK")
	}

	var previous *Snapshot
	record, found, err := p.Store.GetSnapshot(ctx, partnerID)
	if err != nil {
		return PullPlan{}, err
	}
	if found {
		stored, err := ParseSnapshot(record.PartnerID, record.Version, record.PayloadJSON, record.ObservedAt)
		if err != nil {
			return PullPlan{}, err
		}
		previous = &stored
	}

	plan := PullPlan{PartnerID: partnerID, Snapshot: snapshot, Previous: previous}
	var baseline []CapacityInstance
	if previous != nil {
		baseline = previous.Instances
	}
	plan.Added, plan.Updated, plan.Revoked = diffInstances(baseline, snapshot.Instances)
	plan.Unchanged = len(plan.Added) == 0 && len(plan.Updated) == 0 && len(plan.Revoked) == 0

	now := p.clock()
	for _, instance := range plan.Added {
		plan.Events = append(plan.Events, buildPullEvent(partnerID, snapshot.Version, domain.EventCapacityAdded, instance, now))
	}
	for _, instance := range plan.Updated {
		plan.Events = append(plan.Events, buildPullEvent(partnerID, snapshot.Version, domain.EventCapacityUpdated, instance, now))
	}
	for _, id := range plan.Revoked {
		plan.Events = append(plan.Events, domain.CapacityEvent{
			EventID:    fmt.Sprintf("%s:pull:%s:revoke:%s", partnerID, snapshot.Version, id),
			PartnerID:  partnerID,
			Type:       domain.EventCapacityRevoked,
			OccurredAt: now,
			Instance:   &domain.EventInstance{ID: id},
			Source:     domain.SourcePull,
			ReceivedAt: now,
		})
	}
	if plan.Unchanged {
		// A confirmed snapshot is the authority statement "nothing changed".
		// The deterministic id keeps it idempotent across rounds.
		payload := make([]domain.EventInstance, 0, len(snapshot.Instances))
		for _, instance := range snapshot.Instances {
			payload = append(payload, instance.ToEventInstance())
		}
		plan.Events = append(plan.Events, domain.CapacityEvent{
			EventID:    fmt.Sprintf("%s:pull:%s:confirm", partnerID, snapshot.Version),
			PartnerID:  partnerID,
			Type:       domain.EventCapacitySnapshotConfirmed,
			OccurredAt: now,
			Snapshot:   &domain.Snapshot{Version: snapshot.Version, Instances: payload, Observed: snapshot.Observed},
			Source:     domain.SourcePull,
			ReceivedAt: now,
		})
	}
	return plan, nil
}

// Commit persists the snapshot as the new authoritative version. It must only
// be called after every produced event was accepted.
func (p *Puller) Commit(ctx context.Context, plan PullPlan) error {
	payload, err := SnapshotPayload(plan.Snapshot)
	if err != nil {
		return err
	}
	if err := p.Store.SaveSnapshot(ctx, store.SnapshotRecord{
		PartnerID:     plan.PartnerID,
		Version:       plan.Snapshot.Version,
		PayloadJSON:   payload,
		ObservedAt:    plan.Snapshot.Observed,
		Status:        store.SnapshotStatusOK,
		InstanceCount: len(plan.Snapshot.Instances),
	}); err != nil {
		return err
	}
	if p.Metrics != nil {
		p.Metrics.SetGauge(obs.MetricPartnerSyncSuccessTime,
			"Unix timestamp of the last successful partner pull reconciliation.",
			obs.LabelsPartner, float64(p.clock().Unix()), "partner", plan.PartnerID)
	}
	p.resetFailures(plan.PartnerID)
	if p.Logger != nil {
		p.Logger.Info("partner snapshot committed",
			"partner_id", plan.PartnerID,
			"version", plan.Snapshot.Version,
			"instances", len(plan.Snapshot.Instances),
			"added", len(plan.Added),
			"updated", len(plan.Updated),
			"revoked", len(plan.Revoked))
	}
	return nil
}

func (p *Puller) recordFailure(ctx context.Context, partnerID string, cause error) {
	p.mu.Lock()
	p.failures[partnerID]++
	count := p.failures[partnerID]
	p.mu.Unlock()

	if p.Metrics != nil {
		p.Metrics.IncCounter(obs.MetricPartnerSyncFailures,
			"Failed partner pull reconciliation attempts.",
			obs.LabelsPartner, "partner", partnerID)
	}
	if p.Logger != nil {
		p.Logger.Error("partner sync failed",
			"partner_id", partnerID, "consecutive_failures", count, "error", cause.Error())
	}
	// The first failure is alerted immediately: it is the earliest signal that
	// the authoritative capacity view is going stale.
	if p.Alerter != nil {
		p.Alerter.Fire(ctx, obs.Alert{
			Name:      obs.AlertPartnerSyncFailed,
			Severity:  obs.SeverityCritical,
			PartnerID: partnerID,
			Message:   "partner pull reconciliation failed; previous snapshot retained",
			Details: map[string]string{
				"consecutive_failures": fmt.Sprintf("%d", count),
				"threshold":            fmt.Sprintf("%d", p.FailureThreshold),
				"error":                cause.Error(),
			},
		})
	}
}

func (p *Puller) resetFailures(partnerID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.failures, partnerID)
}

func buildPullEvent(partnerID, version string, kind domain.EventType, instance CapacityInstance, now time.Time) domain.CapacityEvent {
	suffix := "add"
	if kind == domain.EventCapacityUpdated {
		suffix = "update"
	}
	payload := instance.ToEventInstance()
	return domain.CapacityEvent{
		EventID:    fmt.Sprintf("%s:pull:%s:%s:%s", partnerID, version, suffix, instance.ID),
		PartnerID:  partnerID,
		Type:       kind,
		OccurredAt: now,
		Instance:   &payload,
		Source:     domain.SourcePull,
		ReceivedAt: now,
	}
}

// diffInstances computes the ADDED / UPDATED / REVOKED sets between the last
// successful snapshot and the current one.
func diffInstances(previous, current []CapacityInstance) (added, updated []CapacityInstance, revoked []string) {
	previousIndex := make(map[string]CapacityInstance, len(previous))
	for _, instance := range previous {
		previousIndex[instance.ID] = instance
	}
	currentIndex := make(map[string]CapacityInstance, len(current))
	for _, instance := range current {
		currentIndex[instance.ID] = instance
		if before, ok := previousIndex[instance.ID]; !ok {
			added = append(added, instance)
		} else if instanceChanged(before, instance) {
			updated = append(updated, instance)
		}
	}
	for _, instance := range previous {
		if _, ok := currentIndex[instance.ID]; !ok {
			revoked = append(revoked, instance.ID)
		}
	}
	sort.Slice(added, func(i, j int) bool { return added[i].ID < added[j].ID })
	sort.Slice(updated, func(i, j int) bool { return updated[i].ID < updated[j].ID })
	sort.Strings(revoked)
	return added, updated, revoked
}

func instanceChanged(before, after CapacityInstance) bool {
	if before.Endpoint != after.Endpoint || before.ServiceEndpoint != after.ServiceEndpoint || before.LeaseID != after.LeaseID {
		return true
	}
	if !before.LeaseUpdatedAt.Equal(after.LeaseUpdatedAt) {
		return true
	}
	if before.Spec.GPU != after.Spec.GPU || before.Spec.GPUCount != after.Spec.GPUCount {
		return true
	}
	beforeModels := before.Spec.SnapshotSpec()
	afterModels := after.Spec.SnapshotSpec()
	if len(beforeModels) != len(afterModels) {
		return true
	}
	for index := range beforeModels {
		if beforeModels[index] != afterModels[index] {
			return true
		}
	}
	return false
}
