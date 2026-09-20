package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/obs"
	"github.com/tai-core/tai-talea/internal/store"
)

// HandleCapacityEvent is the single entry point for both Push and Pull events.
//
// Idempotency contract (§5):
//   - a duplicate event never triggers a lifecycle action again;
//   - a matching duplicate receives the original acceptance and result;
//   - the original durable result is echoed back.
func (c *Controller) HandleCapacityEvent(ctx context.Context, event domain.CapacityEvent) (domain.EventOutcome, error) {
	lock := c.eventOperation(event.EventID)
	lock.Lock()
	defer lock.Unlock()
	if event.ReceivedAt.IsZero() {
		event.ReceivedAt = c.now()
	}
	if event.Source == "" {
		event.Source = domain.SourcePush
	}
	if err := event.Validate(); err != nil {
		c.recordEvent(ctx, event, domain.ResultRejected, err.Error())
		c.metrics.RecordEvent(string(event.Type), string(event.Source), string(domain.ResultRejected))
		c.log().Warn("capacity event rejected",
			"event_id", event.EventID, "partner_id", event.PartnerID, "type", string(event.Type), "reason", err.Error())
		return domain.EventOutcome{
			EventID:  event.EventID,
			Accepted: false,
			Result:   domain.ResultRejected,
			Reason:   err.Error(),
		}, nil
	}

	claimed, err := c.store.ClaimEvent(ctx, event, domain.ResultPending, "")
	if err != nil {
		return domain.EventOutcome{}, err
	}
	if !claimed {
		record, lookupErr := c.store.GetEvent(ctx, event.EventID)
		if lookupErr != nil {
			return domain.EventOutcome{}, lookupErr
		}
		if !sameCapacityEvent(record, event) {
			return domain.EventOutcome{EventID: event.EventID, Result: domain.ResultRejected,
				Reason: "event_id is already bound to another partner or payload"}, nil
		}
		c.metrics.RecordEvent(string(record.EventType), string(record.Source), string(domain.ResultDuplicate))
		c.log().Info("duplicate capacity event ignored",
			"event_id", event.EventID, "partner_id", event.PartnerID,
			"type", string(event.Type), "previous_result", string(record.Result))
		return domain.EventOutcome{
			EventID:   event.EventID,
			Accepted:  record.Result != domain.ResultRejected,
			Duplicate: true,
			Result:    record.Result,
			Reason:    "event already recorded; lifecycle action not repeated",
		}, nil
	}
	return c.applyCapacityEvent(ctx, event)
}

// applyCapacityEvent executes a durable intent. Each handler first checks the
// current lease/state, so interrupted delivery can resume without resurrection
// or restarting an already completed lifecycle action.
func (c *Controller) applyCapacityEvent(ctx context.Context, event domain.CapacityEvent) (domain.EventOutcome, error) {
	if event.Instance != nil {
		instance, lookupErr := c.store.GetInstance(ctx, event.Instance.ID)
		if lookupErr != nil && !errors.Is(lookupErr, store.ErrNotFound) {
			return domain.EventOutcome{}, lookupErr
		}
		if lookupErr == nil && instance.PartnerID != event.PartnerID {
			reason := "instance belongs to another partner"
			if err := c.store.CompleteEvent(ctx, event.EventID, domain.ResultRejected, reason); err != nil {
				return domain.EventOutcome{}, err
			}
			return domain.EventOutcome{EventID: event.EventID, Result: domain.ResultRejected, Reason: reason}, nil
		}
	}

	var (
		result = domain.ResultApplied
		reason string
		err    error
	)
	switch event.Type {
	case domain.EventCapacityAdded:
		result, reason, err = c.handleCapacityAdded(ctx, event)
	case domain.EventCapacityUpdated:
		result, reason, err = c.handleCapacityUpdated(ctx, event)
	case domain.EventCapacityRevoked:
		result, reason, err = c.handleCapacityRevoked(ctx, event)
	case domain.EventCapacitySnapshotConfirmed:
		result, reason, err = c.handleSnapshotConfirmed(ctx, event)
	default:
		err = fmt.Errorf("unsupported event type %q", event.Type)
		result, reason = domain.ResultRejected, err.Error()
	}
	if err != nil {
		if result == domain.ResultApplied {
			result = domain.ResultFailed
		}
		if reason == "" {
			reason = err.Error()
		}
	}

	if err != nil && result != domain.ResultRejected {
		// Keep infrastructure failures pending. The reconciler retries the
		// persisted intent even if the HTTP client disconnected.
		result = domain.ResultPending
	} else if completeErr := c.store.CompleteEvent(ctx, event.EventID, result, reason); completeErr != nil {
		err, result = completeErr, domain.ResultPending
	}
	c.metrics.RecordEvent(string(event.Type), string(event.Source), string(result))
	c.log().Info("capacity event processed",
		"event_id", event.EventID, "partner_id", event.PartnerID, "type", string(event.Type),
		"source", string(event.Source), "result", string(result), "reason", reason)

	outcome := domain.EventOutcome{EventID: event.EventID, Accepted: result != domain.ResultRejected, Result: result, Reason: reason}
	if err != nil {
		return outcome, err
	}
	return outcome, nil
}

func sameCapacityEvent(record store.EventRecord, incoming domain.CapacityEvent) bool {
	if record.PartnerID != incoming.PartnerID || record.EventType != incoming.Type {
		return false
	}
	var original domain.CapacityEvent
	if json.Unmarshal([]byte(record.PayloadJSON), &original) != nil {
		return false
	}
	// Older records kept the original HTTP body, which could omit partner_id.
	original.PartnerID = record.PartnerID
	// A caller can recreate a delivery timestamp when retrying the same
	// business intent with its original event_id. The persisted timestamp wins.
	original.OccurredAt, incoming.OccurredAt = time.Time{}, time.Time{}
	if original.Snapshot != nil && incoming.Snapshot != nil {
		original.Snapshot.Observed = time.Time{}
		copySnapshot := *incoming.Snapshot
		copySnapshot.Observed = time.Time{}
		incoming.Snapshot = &copySnapshot
	}
	original.OccurredAt = original.OccurredAt.UTC()
	incoming.OccurredAt = incoming.OccurredAt.UTC()
	a, _ := json.Marshal(original)
	b, _ := json.Marshal(incoming)
	return string(a) == string(b)
}

// recordEvent stores an event that could not even be claimed, so the audit
// trail always contains what the partner sent.
func (c *Controller) recordEvent(ctx context.Context, event domain.CapacityEvent, result domain.EventResult, reason string) {
	if _, err := c.store.ClaimEvent(ctx, event, result, reason); err != nil {
		c.log().Error("failed to record rejected event", "event_id", event.EventID, "error", err.Error())
	}
}

func (c *Controller) handleCapacityAdded(ctx context.Context, event domain.CapacityEvent) (domain.EventResult, string, error) {
	if event.Instance == nil || domain.NormalizeEndpoint(event.Instance.Endpoint) == "" {
		return domain.ResultRejected, "CAPACITY_ADDED requires an instance endpoint", nil
	}
	// Serialize admissions so two concurrent offers cannot claim the same URL.
	if !c.capacityAdmissions.TryLock() {
		return domain.ResultPending, "capacity admission is busy", store.ErrConflict
	}
	defer c.capacityAdmissions.Unlock()
	lock := c.instanceOperation(event.Instance.ID)
	if !lock.TryLock() {
		return domain.ResultPending, "instance lifecycle operation is busy", store.ErrConflict
	}
	defer lock.Unlock()
	if superseded, err := c.store.HasRevokeAfter(ctx, event.PartnerID, event.Instance.ID, event.Instance.LeaseID, event.OccurredAt); err != nil {
		return domain.ResultFailed, "", err
	} else if superseded {
		return domain.ResultRejected, "add predates an accepted revoke for this instance", nil
	}

	existing, err := c.store.GetInstance(ctx, event.Instance.ID)
	found := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return domain.ResultFailed, "", err
	}
	if found {
		if existing.PartnerID != event.PartnerID {
			return domain.ResultRejected, "instance belongs to another partner", nil
		}
		if event.OccurredAt.Before(existing.LeaseUpdatedAt) {
			return domain.ResultRejected, "add event predates the newest lease observation", nil
		}
		if existing.InstanceState == domain.InstanceReleased {
			if event.Instance.LeaseID == "" || event.Instance.LeaseID == existing.LeaseID {
				return domain.ResultApplied, "instance already released; a distinct new lease is required to reoffer", nil
			}
		} else {
			if event.Instance.LeaseID != existing.LeaseID ||
				domain.NormalizeEndpoint(event.Instance.Endpoint) != existing.Endpoint ||
				domain.NormalizeEndpoint(event.Instance.ServiceEndpoint) != existing.ServiceEndpoint {
				return domain.ResultRejected, "active instance has a different lease or endpoint; update or reclaim it first", nil
			}
			if existing.PendingRelease {
				return domain.ResultApplied, "instance reclaim is already pending", nil
			}
			if existing.InstanceState != domain.InstanceAllocating {
				c.audit(ctx, store.AuditEntry{Action: "capacity_add_duplicate", InstanceID: existing.ID,
					PartnerID: event.PartnerID, EventID: event.EventID,
					Details: map[string]string{"instance_state": string(existing.InstanceState)}})
				return domain.ResultApplied, "instance already tracked; lifecycle not restarted", nil
			}
		}
	}

	now := c.now()
	instance := domain.Instance{
		ID: event.Instance.ID, PartnerID: event.PartnerID,
		Endpoint:        domain.NormalizeEndpoint(event.Instance.Endpoint),
		ServiceEndpoint: domain.NormalizeEndpoint(event.Instance.ServiceEndpoint),
		LeaseID:         event.Instance.LeaseID, InstanceState: domain.InstanceAllocating,
		ServiceState: domain.ServiceNone, LeaseUpdatedAt: event.OccurredAt, LastSeenAt: now, CreatedAt: now,
	}
	if event.Instance.Spec != nil {
		instance.Spec = *event.Instance.Spec
	}
	if !found || existing.InstanceState == domain.InstanceReleased {
		rows, err := c.store.ListInstances(ctx)
		if err != nil {
			return domain.ResultFailed, "", err
		}
		for _, row := range rows {
			if row.ID != instance.ID && row.Active() && reservesEndpoint(row, instance.Endpoint, instance.ServiceURL()) {
				return domain.ResultRejected, "endpoint already belongs to another active instance", nil
			}
		}
		if err := c.transition(ctx, store.Transition{
			InstanceID: instance.ID, Next: instance, CreateOnly: !found, Reoffer: found,
			Audit: store.AuditEntry{Action: "capacity_added", InstanceID: instance.ID, PartnerID: event.PartnerID, EventID: event.EventID,
				Details: map[string]string{"endpoint": instance.Endpoint, "lease_id": instance.LeaseID, "source": string(event.Source)}},
		}); err != nil {
			return domain.ResultFailed, "", err
		}
	} else {
		// Resume a crash between the ALLOCATING row and preparation intent.
		instance = existing
	}
	instance.InstanceState = domain.InstancePreparing
	operation := &store.Operation{OperationID: lifecycleOperationID(instance, store.OpPrepare),
		InstanceID: instance.ID, Type: store.OpPrepare, Status: store.OpPending, CreatedAt: now}
	if err := c.transition(ctx, store.Transition{InstanceID: instance.ID, Next: instance,
		ExpectInstanceState: domain.InstanceAllocating, Operation: operation,
		Audit: store.AuditEntry{Action: "instance_allocating", InstanceID: instance.ID, PartnerID: event.PartnerID, EventID: event.EventID,
			Details: map[string]string{"operation_id": operation.OperationID}},
	}); err != nil {
		return domain.ResultFailed, "", err
	}
	if err := c.runPrepare(ctx, instance.ID, operation.OperationID); err != nil {
		// The durable preparation operation owns retries after this point.
		return domain.ResultApplied, "preparation queued: " + err.Error(), nil
	}
	return domain.ResultApplied, "instance allocated, prepared and idle", nil
}

func (c *Controller) handleCapacityUpdated(ctx context.Context, event domain.CapacityEvent) (domain.EventResult, string, error) {
	if event.Instance == nil {
		return domain.ResultRejected, "", errors.New("CAPACITY_UPDATED requires an instance payload")
	}
	if !c.capacityAdmissions.TryLock() {
		return domain.ResultPending, "capacity admission is busy", store.ErrConflict
	}
	defer c.capacityAdmissions.Unlock()
	lock := c.instanceOperation(event.Instance.ID)
	if !lock.TryLock() {
		return domain.ResultPending, "instance lifecycle operation is busy", store.ErrConflict
	}
	defer lock.Unlock()
	instance, err := c.store.GetInstance(ctx, event.Instance.ID)
	if errors.Is(err, store.ErrNotFound) {
		c.audit(ctx, store.AuditEntry{
			Action: "capacity_update_unknown_instance", InstanceID: event.Instance.ID, PartnerID: event.PartnerID,
			EventID: event.EventID, Details: map[string]string{"reason": "instance is not tracked yet"},
		})
		return domain.ResultApplied, "instance not tracked; update recorded only", nil
	}
	if err != nil {
		return domain.ResultFailed, "", err
	}
	if instance.PartnerID != event.PartnerID {
		return domain.ResultRejected, "instance belongs to another partner", nil
	}
	latestObservation := instance.LeaseUpdatedAt
	if instance.PendingUpdate != nil && instance.PendingUpdate.ObservedAt.After(latestObservation) {
		latestObservation = instance.PendingUpdate.ObservedAt
	}
	if event.OccurredAt.Before(latestObservation) {
		return domain.ResultRejected, "update predates the newest lease observation", nil
	}
	if instance.InstanceState == domain.InstanceReleased {
		c.audit(ctx, store.AuditEntry{
			Action: "capacity_update_ignored", InstanceID: instance.ID, PartnerID: event.PartnerID,
			EventID: event.EventID, Details: map[string]string{"reason": "instance already released"},
		})
		return domain.ResultApplied, "instance already released", nil
	}
	if instance.PendingRelease || instance.InstanceState == domain.InstanceReleasing {
		return domain.ResultRejected, "instance reclaim is pending; metadata cannot change until released", nil
	}

	updated := instance
	if instance.PendingUpdate != nil {
		applyInstanceUpdate(&updated, *instance.PendingUpdate)
	}
	changed := []string{}
	if endpoint := domain.NormalizeEndpoint(event.Instance.Endpoint); endpoint != "" && endpoint != updated.Endpoint {
		updated.Endpoint = endpoint
		changed = append(changed, "endpoint")
	}
	if service := domain.NormalizeEndpoint(event.Instance.ServiceEndpoint); (service != "" || event.Source == domain.SourcePull) &&
		service != updated.ServiceEndpoint {
		updated.ServiceEndpoint = service
		changed = append(changed, "service_endpoint")
	}
	if event.Instance.LeaseID != "" && event.Instance.LeaseID != updated.LeaseID {
		updated.LeaseID = event.Instance.LeaseID
		changed = append(changed, "lease_id")
	}
	if event.Instance.Spec != nil {
		updated.Spec = *event.Instance.Spec
		changed = append(changed, "spec")
	}
	updated.LeaseUpdatedAt = event.OccurredAt
	updated.LastSeenAt = c.now()
	rows, err := c.store.ListInstances(ctx)
	if err != nil {
		return domain.ResultFailed, "", err
	}
	for _, row := range rows {
		if row.ID == instance.ID || !row.Active() {
			continue
		}
		if reservesEndpoint(row, updated.Endpoint, updated.ServiceURL()) {
			return domain.ResultRejected, "endpoint already belongs to another active instance", nil
		}
	}

	// A live worker whose endpoint or lease changed must be drained and
	// re-registered: the Router only knows the previous registration.
	if instance.HasService() && (instance.PendingUpdate != nil || contains(changed, "endpoint") || contains(changed, "service_endpoint") || contains(changed, "lease_id")) {
		// Commit the desired identity before any network operation. All probes,
		// load checks and stop calls keep using the old identity until drained.
		pending := instance
		pending.PendingUpdate = &domain.InstanceUpdate{Endpoint: updated.Endpoint, ServiceEndpoint: updated.ServiceEndpoint,
			LeaseID: updated.LeaseID, Spec: updated.Spec, ObservedAt: event.OccurredAt}
		if err := c.transition(ctx, store.Transition{InstanceID: instance.ID, Next: pending,
			Audit: store.AuditEntry{Action: "capacity_update_queued", InstanceID: instance.ID,
				PartnerID: event.PartnerID, EventID: event.EventID, Details: map[string]string{"changed": strings.Join(changed, ",")}}}); err != nil {
			return domain.ResultFailed, "", err
		}
		if err := c.beginDrain(ctx, instance.ID, "instance identity changed", c.DefaultDrainGrace(), false); err != nil {
			return domain.ResultApplied, "identity update queued; drain will retry: " + err.Error(), nil
		}
		return domain.ResultApplied, "identity update queued; old worker draining", nil
	}
	if !instance.HasService() && (contains(changed, "endpoint") || contains(changed, "service_endpoint") || contains(changed, "lease_id")) &&
		(instance.InstanceState == domain.InstanceIdle || instance.InstanceState == domain.InstanceLost) {
		updated.InstanceState = domain.InstancePreparing
		updated.PrepareAttempts, updated.StartAttempts = 0, 0
		updated.RouterWorkerID, updated.ReadinessGeneration = "", 0
	}

	if err := c.transition(ctx, store.Transition{
		InstanceID: instance.ID,
		Next:       updated,
		Audit: store.AuditEntry{
			Action: "capacity_updated", InstanceID: instance.ID, PartnerID: event.PartnerID, EventID: event.EventID,
			Details: map[string]string{"changed": strings.Join(changed, ",")},
		},
	}); err != nil {
		return domain.ResultFailed, "", err
	}
	return domain.ResultApplied, "instance metadata updated: " + strings.Join(changed, ","), nil
}

func (c *Controller) handleCapacityRevoked(ctx context.Context, event domain.CapacityEvent) (domain.EventResult, string, error) {
	if event.Instance == nil {
		return domain.ResultRejected, "", errors.New("CAPACITY_REVOKED requires an instance payload")
	}
	instance, err := c.store.GetInstance(ctx, event.Instance.ID)
	if errors.Is(err, store.ErrNotFound) {
		lock := c.instanceOperation(event.Instance.ID)
		if !lock.TryLock() {
			return domain.ResultPending, "instance admission is still in progress", store.ErrConflict
		}
		defer lock.Unlock()
		if _, latestErr := c.store.GetInstance(ctx, event.Instance.ID); !errors.Is(latestErr, store.ErrNotFound) {
			return domain.ResultPending, "instance admission changed; revoke will retry", store.ErrConflict
		}
		// Already gone is an idempotent success, but it is still audited.
		c.audit(ctx, store.AuditEntry{
			Action: "capacity_revoke_unknown_instance", InstanceID: event.Instance.ID, PartnerID: event.PartnerID,
			EventID: event.EventID, Details: map[string]string{"result": "idempotent_success"},
		})
		return domain.ResultApplied, "instance already absent from the control plane", nil
	}
	if err != nil {
		return domain.ResultFailed, "", err
	}
	if instance.PartnerID != event.PartnerID {
		return domain.ResultRejected, "instance belongs to another partner", nil
	}
	if event.Instance.LeaseID != "" && event.Instance.LeaseID != instance.LeaseID {
		return domain.ResultRejected, "revoke lease does not match the current lease", nil
	}
	if instance.InstanceState == domain.InstanceReleased {
		c.audit(ctx, store.AuditEntry{
			Action: "capacity_revoke_duplicate", InstanceID: instance.ID, PartnerID: event.PartnerID, EventID: event.EventID,
		})
		return domain.ResultApplied, "instance already released", nil
	}

	// A revoke that is older than the newest lease observation must not undo a
	// fresher Pull result (§5).
	latestObservation := instance.LeaseUpdatedAt
	if instance.PendingUpdate != nil && instance.PendingUpdate.ObservedAt.After(latestObservation) {
		latestObservation = instance.PendingUpdate.ObservedAt
	}
	if !latestObservation.IsZero() && event.OccurredAt.Before(latestObservation) {
		c.audit(ctx, store.AuditEntry{
			Action: "capacity_revoke_stale", InstanceID: instance.ID, PartnerID: event.PartnerID, EventID: event.EventID,
			Details: map[string]string{
				"event_occurred_at": event.OccurredAt.Format(time.RFC3339),
				"lease_observed_at": instance.LeaseUpdatedAt.Format(time.RFC3339),
			},
		})
		c.alert(ctx, obs.Alert{
			Name:       obs.AlertIllegalStateTransition,
			Severity:   obs.SeverityWarning,
			InstanceID: instance.ID,
			PartnerID:  event.PartnerID,
			EventID:    event.EventID,
			Message:    "stale revoke event ignored because a newer lease observation exists",
			Details: map[string]string{
				"event_occurred_at": event.OccurredAt.Format(time.RFC3339),
				"lease_observed_at": instance.LeaseUpdatedAt.Format(time.RFC3339),
			},
		})
		return domain.ResultRejected, "revoke event predates the newest lease observation", nil
	}

	grace := time.Duration(event.GraceSeconds) * time.Second
	if event.GraceSeconds == 0 {
		grace = c.cfg.Controller.DrainGrace.Duration()
	}

	if err := c.transition(ctx, store.Transition{InstanceID: instance.ID, RequestRelease: true,
		ExpectPartnerID: event.PartnerID, ExpectLeaseID: &instance.LeaseID, ExpectLeaseUpdatedAt: instance.LeaseUpdatedAt,
		ReleaseObservedAt: event.OccurredAt,
		Next:              domain.Instance{DrainDeadlineAt: c.now().Add(grace)},
		Audit: store.AuditEntry{Action: "instance_reclaim_requested", InstanceID: instance.ID, PartnerID: event.PartnerID,
			EventID: event.EventID, Details: map[string]string{"reason": "capacity revoked by partner", "grace": grace.String()}},
	}); err != nil {
		return domain.ResultFailed, "", err
	}
	// An in-flight start observes the durable reclaim flag. Do not block the
	// HTTP request behind model loading; reconciliation finishes asynchronously.
	lock := c.instanceOperation(instance.ID)
	if !lock.TryLock() {
		return domain.ResultApplied, "reclaim queued; current lifecycle operation is finishing", nil
	}
	defer lock.Unlock()
	latest, err := c.store.GetInstance(ctx, instance.ID)
	if err != nil {
		return domain.ResultFailed, "", err
	}
	if latest.LeaseID != instance.LeaseID || !latest.PendingRelease {
		// Reconciliation may have finished the old reclaim and admitted a new
		// lease between the durable request above and acquiring this lock.
		return domain.ResultApplied, "original lease reclaim has already completed", nil
	}
	if err := c.beginDrain(ctx, instance.ID, "capacity revoked by partner", grace, true); err != nil {
		c.recordReclaimError(ctx, instance.ID, err)
		return domain.ResultApplied, "reclaim queued for retry: " + err.Error(), nil
	}
	return domain.ResultApplied, "reclaim accepted; service drain and release are tracked durably", nil
}

func (c *Controller) handleSnapshotConfirmed(ctx context.Context, event domain.CapacityEvent) (domain.EventResult, string, error) {
	if event.Snapshot == nil {
		return domain.ResultRejected, "", errors.New("CAPACITY_SNAPSHOT_CONFIRMED requires a snapshot payload")
	}
	instances, err := c.store.ListInstances(ctx)
	if err != nil {
		return domain.ResultFailed, "", err
	}
	confirmed := make(map[string]bool, len(event.Snapshot.Instances))
	for _, item := range event.Snapshot.Instances {
		confirmed[item.ID] = true
	}
	var drifted []string
	for _, instance := range instances {
		if confirmed[instance.ID] && instance.PartnerID != event.PartnerID {
			return domain.ResultRejected, "snapshot contains an instance owned by another partner", nil
		}
		if !instance.Active() {
			continue
		}
		if instance.PartnerID != event.PartnerID {
			continue
		}
		if !confirmed[instance.ID] {
			drifted = append(drifted, instance.ID)
		}
	}
	c.audit(ctx, store.AuditEntry{
		Action: "capacity_snapshot_confirmed", PartnerID: event.PartnerID, EventID: event.EventID,
		Details: map[string]string{
			"version":                 event.Snapshot.Version,
			"instance_count":          fmt.Sprintf("%d", len(event.Snapshot.Instances)),
			"tracked_not_in_snapshot": strings.Join(drifted, ","),
		},
	})
	if len(drifted) > 0 {
		c.alert(ctx, obs.Alert{
			Name:      obs.AlertPartnerSyncFailed,
			Severity:  obs.SeverityWarning,
			PartnerID: event.PartnerID,
			EventID:   event.EventID,
			Message:   "tracked instances are missing from the confirmed partner snapshot",
			Details:   map[string]string{"missing_instances": strings.Join(drifted, ",")},
		})
	}
	return domain.ResultApplied, "snapshot confirmation recorded", nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
