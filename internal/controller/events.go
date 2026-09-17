package controller

import (
	"context"
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
//   - the caller receives accepted=true with duplicate=true;
//   - the original durable result is echoed back.
func (c *Controller) HandleCapacityEvent(ctx context.Context, event domain.CapacityEvent) (domain.EventOutcome, error) {
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
		c.metrics.RecordEvent(string(record.EventType), string(record.Source), string(domain.ResultDuplicate))
		c.log().Info("duplicate capacity event ignored",
			"event_id", event.EventID, "partner_id", event.PartnerID,
			"type", string(event.Type), "previous_result", string(record.Result))
		return domain.EventOutcome{
			EventID:   event.EventID,
			Accepted:  true,
			Duplicate: true,
			Result:    record.Result,
			Reason:    "event already recorded; lifecycle action not repeated",
		}, nil
	}

	var (
		result = domain.ResultApplied
		reason string
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

	if completeErr := c.store.CompleteEvent(ctx, event.EventID, result, reason); completeErr != nil && err == nil {
		err = completeErr
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

// recordEvent stores an event that could not even be claimed, so the audit
// trail always contains what the partner sent.
func (c *Controller) recordEvent(ctx context.Context, event domain.CapacityEvent, result domain.EventResult, reason string) {
	if _, err := c.store.ClaimEvent(ctx, event, result, reason); err != nil {
		c.log().Error("failed to record rejected event", "event_id", event.EventID, "error", err.Error())
	}
}

func (c *Controller) handleCapacityAdded(ctx context.Context, event domain.CapacityEvent) (domain.EventResult, string, error) {
	if event.Instance == nil {
		return domain.ResultRejected, "", errors.New("CAPACITY_ADDED requires an instance payload")
	}
	existing, err := c.store.GetInstance(ctx, event.Instance.ID)
	switch {
	case err == nil:
		switch existing.InstanceState {
		case domain.InstanceReleased:
			// The container left the control plane and the partner re-offered
			// the same id. M1 treats this as an idempotent no-op with an audit
			// trail instead of resurrecting a released row.
			c.audit(ctx, store.AuditEntry{
				Action: "capacity_add_ignored", InstanceID: existing.ID, PartnerID: event.PartnerID, EventID: event.EventID,
				Details: map[string]string{"reason": "instance already released"},
			})
			return domain.ResultApplied, "instance already released; nothing to do", nil
		case domain.InstanceLost:
			c.audit(ctx, store.AuditEntry{
				Action: "capacity_add_recovers_lost_instance", InstanceID: existing.ID, PartnerID: event.PartnerID,
				EventID: event.EventID, Details: map[string]string{"endpoint": event.Instance.Endpoint},
			})
			return domain.ResultApplied, "lost instance re-offered by partner; recovery scheduled", nil
		default:
			c.audit(ctx, store.AuditEntry{
				Action: "capacity_add_duplicate", InstanceID: existing.ID, PartnerID: event.PartnerID, EventID: event.EventID,
				Details: map[string]string{"instance_state": string(existing.InstanceState)},
			})
			return domain.ResultApplied, "instance already tracked; lifecycle not restarted", nil
		}
	case !errors.Is(err, store.ErrNotFound):
		return domain.ResultFailed, "", err
	}

	now := c.now()
	instance := domain.Instance{
		ID:              event.Instance.ID,
		PartnerID:       event.PartnerID,
		Endpoint:        domain.NormalizeEndpoint(event.Instance.Endpoint),
		ServiceEndpoint: domain.NormalizeEndpoint(event.Instance.ServiceEndpoint),
		LeaseID:         event.Instance.LeaseID,
		InstanceState:   domain.InstanceAllocating,
		ServiceState:    domain.ServiceNone,
		LeaseUpdatedAt:  event.OccurredAt,
		LastSeenAt:      now,
		CreatedAt:       now,
	}
	if event.Instance.Spec != nil {
		instance.Spec = *event.Instance.Spec
	}

	// §7.1: CAPACITY_ADDED -> ALLOCATING -> PREPARING. Both steps are written so
	// the audit trail shows the takeover and the preparation intent separately.
	allocating := instance
	if err := c.transition(ctx, store.Transition{
		InstanceID: instance.ID,
		Next:       allocating,
		Audit: store.AuditEntry{
			Action: "capacity_added", InstanceID: instance.ID, PartnerID: event.PartnerID, EventID: event.EventID,
			Details: map[string]string{
				"endpoint": instance.Endpoint,
				"lease_id": instance.LeaseID,
				"source":   string(event.Source),
			},
		},
	}); err != nil {
		return domain.ResultFailed, "", err
	}

	instance.InstanceState = domain.InstancePreparing
	operation := &store.Operation{
		OperationID: c.operationID(instance.ID, store.OpPrepare),
		InstanceID:  instance.ID,
		Type:        store.OpPrepare,
		Status:      store.OpPending,
		CreatedAt:   now,
	}
	if err := c.transition(ctx, store.Transition{
		InstanceID:          instance.ID,
		Next:                instance,
		ExpectInstanceState: domain.InstanceAllocating,
		Operation:           operation,
		Audit: store.AuditEntry{
			Action: "instance_allocating", InstanceID: instance.ID, PartnerID: event.PartnerID, EventID: event.EventID,
			Details: map[string]string{"operation_id": operation.OperationID},
		},
	}); err != nil {
		return domain.ResultFailed, "", err
	}

	if err := c.runPrepare(ctx, instance.ID, operation.OperationID); err != nil {
		// Preparation failures are already recorded on the instance row; the
		// event itself was accepted and is not replayed.
		return domain.ResultApplied, "preparation failed: " + err.Error(), nil
	}
	return domain.ResultApplied, "instance allocated, prepared and idle", nil
}

func (c *Controller) handleCapacityUpdated(ctx context.Context, event domain.CapacityEvent) (domain.EventResult, string, error) {
	if event.Instance == nil {
		return domain.ResultRejected, "", errors.New("CAPACITY_UPDATED requires an instance payload")
	}
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
	if instance.InstanceState == domain.InstanceReleased {
		c.audit(ctx, store.AuditEntry{
			Action: "capacity_update_ignored", InstanceID: instance.ID, PartnerID: event.PartnerID,
			EventID: event.EventID, Details: map[string]string{"reason": "instance already released"},
		})
		return domain.ResultApplied, "instance already released", nil
	}

	updated := instance
	changed := []string{}
	if endpoint := domain.NormalizeEndpoint(event.Instance.Endpoint); endpoint != "" && endpoint != instance.Endpoint {
		updated.Endpoint = endpoint
		changed = append(changed, "endpoint")
	}
	if service := domain.NormalizeEndpoint(event.Instance.ServiceEndpoint); service != "" &&
		service != instance.ServiceEndpoint {
		updated.ServiceEndpoint = service
		changed = append(changed, "service_endpoint")
	}
	if event.Instance.LeaseID != "" && event.Instance.LeaseID != instance.LeaseID {
		updated.LeaseID = event.Instance.LeaseID
		changed = append(changed, "lease_id")
	}
	if event.Instance.Spec != nil {
		updated.Spec = *event.Instance.Spec
		changed = append(changed, "spec")
	}
	updated.LeaseUpdatedAt = event.OccurredAt
	updated.LastSeenAt = c.now()
	updated.LastError = ""

	if len(changed) == 0 {
		c.audit(ctx, store.AuditEntry{
			Action: "capacity_update_noop", InstanceID: instance.ID, PartnerID: event.PartnerID, EventID: event.EventID,
		})
		return domain.ResultApplied, "no observable change", nil
	}

	// A live worker whose endpoint or lease changed must be drained and
	// re-registered: the Router only knows the previous registration.
	if instance.Serving() && (contains(changed, "endpoint") || contains(changed, "lease_id")) {
		c.audit(ctx, store.AuditEntry{
			Action: "capacity_update_rebuild_required", InstanceID: instance.ID, PartnerID: event.PartnerID,
			EventID: event.EventID, Details: map[string]string{"changed": strings.Join(changed, ",")},
		})
		if err := c.BeginDrain(ctx, instance.ID, "instance identity changed", c.cfg.Controller.DrainGrace.Duration()); err != nil {
			return domain.ResultFailed, "", err
		}
		return domain.ResultApplied, "worker drained for re-registration", nil
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
	if instance.InstanceState == domain.InstanceReleased {
		c.audit(ctx, store.AuditEntry{
			Action: "capacity_revoke_duplicate", InstanceID: instance.ID, PartnerID: event.PartnerID, EventID: event.EventID,
		})
		return domain.ResultApplied, "instance already released", nil
	}

	// A revoke that is older than the newest lease observation must not undo a
	// fresher Pull result (§5).
	if !instance.LeaseUpdatedAt.IsZero() && event.OccurredAt.Before(instance.LeaseUpdatedAt) {
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

	if instance.Serving() {
		if err := c.DrainForRelease(ctx, instance.ID, "capacity revoked by partner", grace); err != nil {
			return domain.ResultFailed, "", err
		}
		return domain.ResultApplied, "readiness closed; draining in-flight requests before release", nil
	}

	// Idle or failed containers are released directly: there is no traffic to
	// drain, and §5 requires IDLE -> RELEASING.
	if err := c.BeginRelease(ctx, instance.ID, "capacity revoked by partner"); err != nil {
		return domain.ResultFailed, "", err
	}
	return domain.ResultApplied, "instance released to partner", nil
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
