package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/store"
)

// pullAuthoritative reconciles a validated snapshot with current intent, not
// only the previous snapshot. This repairs a missed Push, a reverted snapshot,
// and an unchanged snapshot after state drift.
func (c *Controller) pullAuthoritative(ctx context.Context, schedule PullSchedule) error {
	value, _ := c.pullOperations.LoadOrStore(schedule.Adapter.PartnerID(), &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	// Use the start of observation as the ordering barrier. Pushes arriving
	// while the partner's two HTTP reads are in flight must win this round.
	observed := c.now()
	plan, err := schedule.Puller.Plan(ctx, schedule.Adapter)
	if err != nil {
		return err
	}
	instances, err := c.store.ListInstances(ctx)
	if err != nil {
		return err
	}
	current := make(map[string]domain.Instance, len(instances))
	for _, instance := range instances {
		current[instance.ID] = instance
	}
	// Reject ownership collisions before applying any part of this snapshot.
	for _, offered := range plan.Snapshot.Instances {
		if row, found := current[offered.ID]; found && row.PartnerID != plan.PartnerID {
			return fmt.Errorf("snapshot instance %s belongs to another partner", offered.ID)
		}
	}
	present := make(map[string]bool, len(plan.Snapshot.Instances))
	apply := func(event domain.CapacityEvent, before *domain.Instance) error {
		// Scope idempotency to this observation and the preceding intent. A
		// content hash alone would suppress A -> B -> A changes forever.
		key, _ := json.Marshal(struct {
			Event       domain.CapacityEvent
			Before      *domain.Instance
			Observation time.Time
		}{event, before, plan.Snapshot.Observed})
		event.EventID = fmt.Sprintf("pull:%x", sha256.Sum256(key))
		outcome, err := c.HandleCapacityEvent(ctx, event)
		if err != nil {
			return fmt.Errorf("apply pull event: %w", err)
		}
		if !outcome.Accepted || outcome.Result != domain.ResultApplied {
			return fmt.Errorf("pull event is %s: %s", outcome.Result, outcome.Reason)
		}
		return nil
	}
	for _, offered := range plan.Snapshot.Instances {
		present[offered.ID] = true
		row, found := current[offered.ID]
		if found && (row.PendingRelease || row.InstanceState == domain.InstanceReleasing) {
			continue // A local reclaim has precedence until the current lease ends.
		}
		if found && row.InstanceState == domain.InstanceReleased && offered.LeaseID == row.LeaseID {
			continue // The same released lease cannot resurrect the container.
		}
		kind := domain.EventCapacityUpdated
		if !found || row.InstanceState == domain.InstanceReleased {
			kind = domain.EventCapacityAdded
		}
		if found && row.LeaseUpdatedAt.After(observed) {
			continue // A Push received during this poll is newer than this snapshot.
		}
		if found && row.PendingUpdate != nil {
			pending := row.PendingUpdate
			if pending.ObservedAt.After(observed) ||
				(pending.Endpoint == offered.Endpoint && pending.ServiceEndpoint == offered.ServiceEndpoint && pending.LeaseID == offered.LeaseID && reflect.DeepEqual(pending.Spec, offered.Spec)) {
				continue
			}
		}
		payload := offered.ToEventInstance()
		event := domain.CapacityEvent{PartnerID: plan.PartnerID, Type: kind, OccurredAt: observed,
			Source: domain.SourcePull, ReceivedAt: c.now(), Instance: &payload}
		var before *domain.Instance
		if found {
			before = &row
		}
		if err := apply(event, before); err != nil {
			return err
		}
	}
	for _, row := range instances {
		if row.PartnerID != plan.PartnerID || !row.Active() || row.PendingRelease || present[row.ID] || row.LeaseUpdatedAt.After(observed) {
			continue
		}
		if err := apply(domain.CapacityEvent{PartnerID: plan.PartnerID, Type: domain.EventCapacityRevoked,
			OccurredAt: observed, Source: domain.SourcePull, ReceivedAt: c.now(),
			Instance: &domain.EventInstance{ID: row.ID, LeaseID: row.LeaseID}}, &row); err != nil {
			return err
		}
	}
	c.audit(ctx, store.AuditEntry{Action: "capacity_snapshot_reconciled", PartnerID: plan.PartnerID,
		Details: map[string]string{"version": plan.Snapshot.Version, "instance_count": fmt.Sprint(len(plan.Snapshot.Instances))}})
	return schedule.Puller.Commit(ctx, plan)
}
