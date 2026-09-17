package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/launcher"
	"github.com/tai-core/tai-talea/internal/obs"
	"github.com/tai-core/tai-talea/internal/routeradapter"
	"github.com/tai-core/tai-talea/internal/store"
)

// ReconcileReport summarises one reconciliation round.
type ReconcileReport struct {
	Checked    int      `json:"checked"`
	Lost       []string `json:"lost,omitempty"`
	Reported   []string `json:"reported,omitempty"`
	Preparing  []string `json:"preparing,omitempty"`
	Reregister []string `json:"reregister,omitempty"`
	Resumed    []string `json:"resumed,omitempty"`
	Recovered  []string `json:"recovered,omitempty"`
	Released   []string `json:"released,omitempty"`
	Drained    []string `json:"drained,omitempty"`
	Abandoned  int      `json:"abandoned_events"`
}

// ReconcileOnce compares the database, the container, SGLang and the Router, and
// moves every instance towards a legal state (§13).
//
// The database is the control intent, never the actual state: a row claiming
// SERVING is only trusted after the Router membership and readiness were
// verified in this round.
func (c *Controller) ReconcileOnce(ctx context.Context) (ReconcileReport, error) {
	report := ReconcileReport{}

	window := 2 * c.cfg.Controller.ReconcileInterval.Duration()
	if window <= 0 {
		window = 20 * time.Second
	}
	abandoned, err := c.store.AbandonStaleEvents(ctx, c.now().Add(-window))
	if err != nil {
		c.log().Error("failed to abandon stale events", "error", err.Error())
	} else {
		report.Abandoned = abandoned
	}

	instances, err := c.store.ListInstances(ctx)
	if err != nil {
		return report, err
	}
	report.Checked = len(instances)

	lostCount := 0
	activeCount := 0
	for _, instance := range instances {
		if !instance.Active() {
			continue
		}
		activeCount++
		reconciled, err := c.store.GetInstance(ctx, instance.ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return report, err
		}
		if err := c.reconcileInstance(ctx, reconciled, &report); err != nil {
			c.log().Error("reconcile instance failed", "instance_id", instance.ID, "error", err.Error())
		}
		refreshed, err := c.store.GetInstance(ctx, instance.ID)
		if err == nil && refreshed.InstanceState == domain.InstanceLost {
			lostCount++
		}
	}

	c.checkLostRate(ctx, lostCount, activeCount)
	return report, nil
}

func (c *Controller) reconcileInstance(ctx context.Context, instance domain.Instance, report *ReconcileReport) error {
	if err := c.checkLeaseExpiry(ctx, instance); err != nil {
		c.log().Warn("lease expiry check failed", "instance_id", instance.ID, "error", err.Error())
	}

	// Containers already given back need only the release to be retried.
	if instance.InstanceState == domain.InstanceReleasing {
		if err := c.BeginRelease(ctx, instance.ID, "resume release after interruption"); err != nil {
			c.log().Warn("release retry failed", "instance_id", instance.ID, "error", err.Error())
		} else {
			report.Released = append(report.Released, instance.ID)
		}
		return nil
	}

	reachable := true
	if domain.NormalizeEndpoint(instance.Endpoint) != "" {
		if _, err := c.Probe(ctx, instance); err != nil {
			reachable = false
		}
	}
	if !reachable {
		if instance.InstanceState != domain.InstanceLost {
			if err := c.markLost(ctx, instance, "container endpoint is unreachable"); err != nil {
				return err
			}
			report.Lost = append(report.Lost, instance.ID)
		}
		return nil
	}
	if instance.InstanceState == domain.InstanceLost {
		// The container came back. Never jump straight to SERVING: re-enter the
		// preparation pipeline so the environment is validated again.
		next := instance
		next.InstanceState = domain.InstancePreparing
		next.LastError = ""
		if err := c.transition(ctx, store.Transition{
			InstanceID: instance.ID,
			Next:       next,
			Audit: store.AuditEntry{
				Action: "instance_recovered", InstanceID: instance.ID, PartnerID: instance.PartnerID,
				Details: map[string]string{"from": string(domain.InstanceLost), "to": string(domain.InstancePreparing)},
			},
		}); err != nil {
			return err
		}
		report.Recovered = append(report.Recovered, instance.ID)
		instance = next
	}

	switch instance.InstanceState {
	case domain.InstancePreparing:
		if pending, err := c.operationRetryDue(ctx, instance.ID, store.OpPrepare); err != nil {
			return err
		} else if pending {
			if err := c.runPrepare(ctx, instance.ID, c.operationID(instance.ID, store.OpPrepare)); err != nil {
				report.Preparing = append(report.Preparing, instance.ID)
			}
		}
		return nil
	case domain.InstanceIdle:
	default:
		return nil
	}

	switch instance.ServiceState {
	case domain.ServiceNone:
		return nil
	case domain.ServiceFailed:
		if instance.StartAttempts >= c.cfg.Controller.StartMaxAttempts {
			return nil
		}
		if due, err := c.operationRetryDue(ctx, instance.ID, store.OpStart); err != nil {
			return err
		} else if due && instance.Role.Valid() {
			report.Resumed = append(report.Resumed, instance.ID)
			return c.StartService(ctx, instance.ID, instance.Role)
		}
	case domain.ServiceStarting:
		// A STARTING instance older than a few rounds is stuck: nothing owns
		// the operation any more, so restart the supervised attempt.
		stuck := c.now().Sub(instance.UpdatedAt) > 3*c.cfg.Controller.ReconcileInterval.Duration()
		if stuck {
			if err := c.failStart(ctx, instance, &store.Operation{
				OperationID: c.operationID(instance.ID, store.OpStart),
				InstanceID:  instance.ID,
				Type:        store.OpStart,
				CreatedAt:   c.now(),
			}, "start operation was interrupted", errors.New("no owner for STARTING state")); err != nil {
				return err
			}
			report.Reported = append(report.Reported, instance.ID)
		}
	case domain.ServiceHealthy, domain.ServiceRegistering, domain.ServiceServing:
		action, err := c.settleService(ctx, instance)
		if err != nil {
			return err
		}
		if action == "reregistered" {
			report.Reregister = append(report.Reregister, instance.ID)
		}
	case domain.ServiceDraining:
		if instance.DrainDeadlineAt.IsZero() {
			next := instance
			next.DrainDeadlineAt = c.now().Add(c.cfg.Controller.DrainGrace.Duration())
			if err := c.transition(ctx, store.Transition{InstanceID: instance.ID, Next: next}); err != nil {
				return err
			}
			return nil
		}
		if err := c.FinishDrain(ctx, instance); err != nil {
			return err
		}
		report.Drained = append(report.Drained, instance.ID)
	}
	return nil
}

// settleService verifies the Router view of a service that should be serving and
// repairs it. It returns "ok" when nothing changed, otherwise "reregistered".
func (c *Controller) settleService(ctx context.Context, instance domain.Instance) (string, error) {
	if !instance.Role.Valid() {
		// An unassigned role with a live service state is illegal: drain and
		// return the instance to idle instead of guessing a role.
		c.alert(ctx, obs.Alert{
			Name:       obs.AlertIllegalStateTransition,
			Severity:   obs.SeverityCritical,
			InstanceID: instance.ID,
			PartnerID:  instance.PartnerID,
			Message:    "service state present without an assigned PD role; draining",
			Details:    map[string]string{"service_state": string(instance.ServiceState)},
		})
		if err := c.BeginDrain(ctx, instance.ID, "service without role", 0); err != nil {
			return "", err
		}
		return "reregistered", nil
	}

	registration, err := c.register(ctx, instance, instance.Role)
	if err != nil {
		return "", c.failStart(ctx, instance, &store.Operation{
			OperationID: c.operationID(instance.ID, store.OpRegister),
			InstanceID:  instance.ID,
			Type:        store.OpRegister,
			CreatedAt:   c.now(),
		}, "router registration failed during reconcile", err)
	}
	generation, ready, err := c.ensureReadiness(ctx, registration.WorkerID)
	if err != nil {
		return "", err
	}
	if !ready {
		return "", fmt.Errorf("worker %s readiness is not routable after reconcile", registration.WorkerID)
	}

	if instance.ServiceState == domain.ServiceServing &&
		instance.RouterWorkerID == registration.WorkerID &&
		instance.ReadinessGeneration == generation {
		return "ok", nil
	}

	next := instance
	next.RouterWorkerID = registration.WorkerID
	next.ReadinessGeneration = generation
	next.ServiceState = domain.ServiceServing
	next.LastError = ""
	next.LastSeenAt = c.now()
	if instance.ServiceState != domain.ServiceServing {
		next.StartAttempts = 0
	}
	action := "reregistered"
	auditAction := "service_reregistered"
	if instance.ServiceState == domain.ServiceServing && instance.RouterWorkerID != registration.WorkerID {
		auditAction = "service_membership_repaired"
	}
	if err := c.transition(ctx, store.Transition{
		InstanceID: instance.ID,
		Next:       next,
		Audit: store.AuditEntry{
			Action: auditAction, InstanceID: instance.ID, PartnerID: instance.PartnerID,
			Details: map[string]string{
				"router":                 c.cfg.Router.Name,
				"role":                   string(instance.Role),
				"router_worker_id":       registration.WorkerID,
				"readiness_generation":   fmt.Sprintf("%d", generation),
				"previous_service_state": string(instance.ServiceState),
			},
		},
	}); err != nil {
		return "", err
	}
	c.log().Info("service membership reconciled",
		"instance_id", instance.ID, "role", string(instance.Role),
		"router_worker_id", registration.WorkerID, "generation", generation)
	return action, nil
}

func (c *Controller) operationRetryDue(ctx context.Context, instanceID string, kind store.OperationType) (bool, error) {
	operations, err := c.store.ListOpenOperations(ctx, 200)
	if err != nil {
		return false, err
	}
	for _, operation := range operations {
		if operation.InstanceID != instanceID || operation.Type != kind {
			continue
		}
		if operation.NextRetryAt.IsZero() || !c.now().Before(operation.NextRetryAt) {
			return true, nil
		}
		return false, nil
	}
	// No open operation: the instance was restored from a previous process
	// lifetime, so the retry budget is available immediately.
	return true, nil
}

func (c *Controller) checkLeaseExpiry(ctx context.Context, instance domain.Instance) error {
	warning := c.cfg.Controller.LeaseWarning.Duration()
	if warning <= 0 || instance.LeaseUpdatedAt.IsZero() {
		return nil
	}
	if c.now().Sub(instance.LeaseUpdatedAt) < warning {
		return nil
	}
	c.alert(ctx, obs.Alert{
		Name:       obs.AlertLeaseExpiringSoon,
		Severity:   obs.SeverityWarning,
		InstanceID: instance.ID,
		PartnerID:  instance.PartnerID,
		Message:    "partner lease observation is older than the warning window",
		Details: map[string]string{
			"lease_id":          instance.LeaseID,
			"lease_observed_at": instance.LeaseUpdatedAt.Format(time.RFC3339),
			"warning_seconds":   fmt.Sprintf("%d", int(warning.Seconds())),
		},
	})
	return nil
}

func (c *Controller) checkLostRate(ctx context.Context, lost, active int) {
	if active == 0 {
		return
	}
	threshold := c.cfg.Controller.LostRateThreshold
	if threshold <= 0 {
		return
	}
	rate := float64(lost) / float64(active)
	if c.metrics != nil {
		c.metrics.SetGauge("capacity_lost_rate",
			"Share of active instances currently in the LOST state.", nil, rate)
	}
	if rate <= threshold {
		return
	}
	c.alert(ctx, obs.Alert{
		Name:     obs.AlertInstanceLostRateHigh,
		Severity: obs.SeverityCritical,
		Message:  "LOST instance rate exceeded the configured threshold",
		Details: map[string]string{
			"lost":      fmt.Sprintf("%d", lost),
			"active":    fmt.Sprintf("%d", active),
			"rate":      fmt.Sprintf("%.3f", rate),
			"threshold": fmt.Sprintf("%.3f", threshold),
		},
	})
}

// ResumeOpenOperations replays the durable operation queue after a restart.
func (c *Controller) ResumeOpenOperations(ctx context.Context) ([]string, error) {
	operations, err := c.store.ListOpenOperations(ctx, 200)
	if err != nil {
		return nil, err
	}
	var resumed []string
	for _, operation := range operations {
		if !operation.NextRetryAt.IsZero() && c.now().Before(operation.NextRetryAt) {
			continue
		}
		instance, err := c.store.GetInstance(ctx, operation.InstanceID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				_ = c.store.CompleteOperation(ctx, operation.OperationID, store.OpFailed, "instance row is gone", time.Time{})
				continue
			}
			return resumed, err
		}
		switch operation.Type {
		case store.OpPrepare:
			if instance.InstanceState == domain.InstancePreparing {
				resumed = append(resumed, operation.OperationID)
				if err := c.runPrepare(ctx, instance.ID, operation.OperationID); err != nil {
					c.log().Warn("prepare resume failed", "instance_id", instance.ID, "error", err.Error())
				}
			}
		case store.OpStart:
			if instance.InstanceState == domain.InstanceIdle && instance.Role.Valid() &&
				(instance.ServiceState == domain.ServiceFailed || instance.ServiceState == domain.ServiceNone) {
				resumed = append(resumed, operation.OperationID)
				if err := c.StartService(ctx, instance.ID, instance.Role); err != nil {
					c.log().Warn("start resume failed", "instance_id", instance.ID, "error", err.Error())
				}
			}
		case store.OpDrain:
			if instance.ServiceState == domain.ServiceDraining {
				resumed = append(resumed, operation.OperationID)
				if err := c.FinishDrain(ctx, instance); err != nil {
					c.log().Warn("drain resume failed", "instance_id", instance.ID, "error", err.Error())
				}
			}
		case store.OpRelease:
			if instance.InstanceState == domain.InstanceReleasing {
				resumed = append(resumed, operation.OperationID)
				if err := c.BeginRelease(ctx, instance.ID, "resume release"); err != nil {
					c.log().Warn("release resume failed", "instance_id", instance.ID, "error", err.Error())
				}
			}
		default:
			// REGISTER and RETIRE are reconciled through the instance state
			// itself, so the operation row is simply closed.
			_ = c.store.CompleteOperation(ctx, operation.OperationID, store.OpSucceeded, "handled by reconcile", time.Time{})
		}
	}
	return resumed, nil
}

// Recover runs the restart sequence. It is called once before the periodic loop.
func (c *Controller) Recover(ctx context.Context) (ReconcileReport, []string, error) {
	resumed, err := c.ResumeOpenOperations(ctx)
	if err != nil {
		return ReconcileReport{}, nil, err
	}
	report, err := c.ReconcileOnce(ctx)
	if err != nil {
		return report, resumed, err
	}
	c.log().Info("control plane recovery finished",
		"checked", report.Checked,
		"lost", len(report.Lost),
		"recovered", len(report.Recovered),
		"reregistered", len(report.Reregister),
		"resumed_operations", len(resumed),
		"abandoned_events", report.Abandoned)
	return report, resumed, nil
}

// VerifyContainerState is a helper used by tests and the admin API to prove the
// control plane never trusts the database alone.
func (c *Controller) VerifyContainerState(ctx context.Context, instanceID string) (launcher.Status, error) {
	instance, err := c.store.GetInstance(ctx, instanceID)
	if err != nil {
		return launcher.Status{}, err
	}
	callCtx, cancel := c.callContext(ctx)
	defer cancel()
	return c.launcher.Status(callCtx, instance.Endpoint)
}

// RouterReadiness exposes the Router readiness record for one instance.
func (c *Controller) RouterReadiness(ctx context.Context, instanceID string) (routeradapter.ReadinessRecord, error) {
	instance, err := c.store.GetInstance(ctx, instanceID)
	if err != nil {
		return routeradapter.ReadinessRecord{}, err
	}
	callCtx, cancel := c.callContext(ctx)
	defer cancel()
	return c.router.GetReadiness(callCtx, instance.RouterWorkerID)
}
