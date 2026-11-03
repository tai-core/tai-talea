package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/store"
)

// pullAuthoritative reconciles a validated snapshot with current intent, not
// only the previous snapshot. This repairs a missed Push, a reverted snapshot,
// and an unchanged snapshot after state drift.
func (c *Controller) pullAuthoritative(ctx context.Context, schedule PullSchedule) (err error) {
	value, _ := c.pullOperations.LoadOrStore(schedule.Adapter.PartnerID(), &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	// Use the start of observation as the ordering barrier. Pushes arriving
	// while the partner's two HTTP reads are in flight must win this round.
	observed := c.now()
	plan, err := schedule.Puller.Plan(ctx, schedule.Adapter)
	if err != nil {
		return err // Plan recorded it.
	}
	// Everything past this point is what decides whether the authoritative view
	// moves forward, and a failure here is not a lost round: the snapshot
	// version stays where it was, so the next round re-diffs the same baseline
	// into the same events and fails the same way. One instance that can never
	// be applied therefore wedges capacity reconciliation for good. That used to
	// be told to nobody - each path returned its error to a caller that logged
	// it at debug - so a wedged control plane and a healthy one looked alike.
	// Recording from one deferred hook instead of at each return means a path
	// added later cannot be the one that forgets.
	defer func() {
		if err != nil {
			schedule.Puller.RecordFailure(ctx, schedule.Adapter.PartnerID(), err)
		}
	}()
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
	// A sweep, not a transaction. One instance that cannot be applied yet must
	// not stop the rest of the snapshot: the event that matters most here is a
	// revocation, and a container that disappeared while another worker was
	// loading still has to be revoked. Aborting at the first pending event left
	// it registered for as long as the load lasted.
	//
	// Deferring is durable, not lossy: ClaimEvent already wrote the event with a
	// pending result, and ResumePendingEvents replays it once the instance is
	// free.
	var deferred []string
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
		if outcome.Result == domain.ResultPending {
			deferred = append(deferred, event.Instance.ID)
			return nil
		}
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
		if found {
			// Visible again: any absence clock is forgiven, so a later
			// absence owes its full grace from its own first miss.
			c.pullMissingSince.Delete(offered.ID)
		}
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
	// An instance absent from the authoritative snapshot is only believed to
	// be revoked once its absence outlasts the grace (plan G4). The AlayaNeW
	// list can omit a RUNNING instance for minutes after a STOP/START, and a
	// revocation fired at that live container ends in the reclaim deleting it.
	for _, row := range instances {
		if row.PartnerID != plan.PartnerID || present[row.ID] {
			continue
		}
		if !row.Active() || row.PendingRelease {
			// Not a revocation candidate this round: drop any stale clock so
			// a future absence owes its full grace from its own first miss.
			c.pullMissingSince.Delete(row.ID)
			continue
		}
		if row.LeaseUpdatedAt.After(observed) {
			continue // A Push received during this poll is newer than this snapshot.
		}
		since, missing := c.pullMissingSince.Load(row.ID)
		if !missing {
			c.pullMissingSince.Store(row.ID, observed)
			c.log().Info("snapshot is missing an active instance - revocation grace starts",
				"partner_id", plan.PartnerID, "instance", row.ID, "grace", c.RevocationGrace().String())
			continue
		}
		if observed.Sub(since.(time.Time)) < c.RevocationGrace() {
			continue
		}
		c.pullMissingSince.Delete(row.ID)
		if err := apply(domain.CapacityEvent{PartnerID: plan.PartnerID, Type: domain.EventCapacityRevoked,
			OccurredAt: observed, Source: domain.SourcePull, ReceivedAt: c.now(),
			Instance: &domain.EventInstance{ID: row.ID, LeaseID: row.LeaseID}}, &row); err != nil {
			return err
		}
	}
	if len(deferred) > 0 {
		c.log().Info("pull round deferred instances that are mid-operation",
			"partner_id", plan.PartnerID, "instances", strings.Join(deferred, ","))
	}
	c.audit(ctx, store.AuditEntry{Action: "capacity_snapshot_reconciled", PartnerID: plan.PartnerID,
		Details: map[string]string{"version": plan.Snapshot.Version, "instance_count": fmt.Sprint(len(plan.Snapshot.Instances))}})
	return schedule.Puller.Commit(ctx, plan)
}
