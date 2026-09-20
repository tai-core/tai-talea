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
	Checked       int      `json:"checked"`
	Lost          []string `json:"lost,omitempty"`
	Reported      []string `json:"reported,omitempty"`
	Preparing     []string `json:"preparing,omitempty"`
	Reregister    []string `json:"reregister,omitempty"`
	Resumed       []string `json:"resumed,omitempty"`
	Recovered     []string `json:"recovered,omitempty"`
	Released      []string `json:"released,omitempty"`
	Drained       []string `json:"drained,omitempty"`
	ResumedEvents int      `json:"resumed_events"`
}

// ReconcileOnce compares the database, the container, SGLang and the Router, and
// moves every instance towards a legal state (§13).
//
// The database is the control intent, never the actual state: a row claiming
// SERVING is only trusted after the Router membership and readiness were
// verified in this round.
func (c *Controller) ReconcileOnce(ctx context.Context) (ReconcileReport, error) {
	report := ReconcileReport{}

	resumedEvents, err := c.ResumePendingEvents(ctx)
	if err != nil {
		c.log().Error("failed to resume pending events", "error", err.Error())
	} else {
		report.ResumedEvents = resumedEvents
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
	if instance.PendingRelease {
		return c.reconcileReclaim(ctx, instance, report)
	}
	if instance.PendingUpdate != nil && instance.ServiceState != domain.ServiceDraining {
		return c.BeginDrain(ctx, instance.ID, "apply pending capacity update", c.DefaultDrainGrace())
	}
	if err := c.checkLeaseExpiry(ctx, instance); err != nil {
		c.log().Warn("lease expiry check failed", "instance_id", instance.ID, "error", err.Error())
	}

	// Containers already given back need only the release to be retried.
	if instance.InstanceState == domain.InstanceReleasing {
		if due, err := c.operationRetryDue(ctx, instance.ID, store.OpRelease); err != nil {
			return err
		} else if !due {
			return nil
		}
		if err := c.BeginRelease(ctx, instance.ID, "resume release after interruption"); err != nil {
			c.log().Warn("release retry failed", "instance_id", instance.ID, "error", err.Error())
		} else {
			report.Released = append(report.Released, instance.ID)
		}
		return nil
	}

	reachable := true
	var observedHealth launcher.Health
	if domain.NormalizeEndpoint(instance.Endpoint) != "" {
		var probeErr error
		observedHealth, probeErr = c.Probe(ctx, instance)
		if probeErr != nil {
			reachable = false
		}
	}
	if !reachable {
		lock := c.instanceOperation(instance.ID)
		if !lock.TryLock() {
			return nil
		}
		defer lock.Unlock()
		latest, err := c.store.GetInstance(ctx, instance.ID)
		if err != nil {
			return err
		}
		if !latest.UpdatedAt.Equal(instance.UpdatedAt) {
			return nil
		}
		if err := c.markLost(ctx, instance, "container endpoint is unreachable"); err != nil {
			return err
		}
		if instance.InstanceState != domain.InstanceLost {
			report.Lost = append(report.Lost, instance.ID)
		}
		return nil
	}
	if observedHealth.Phase == launcher.PhaseFailed && instance.InstanceState == domain.InstanceIdle && instance.ServiceState != domain.ServiceFailed && instance.ServiceState != domain.ServiceDraining && instance.Role.Valid() {
		lock := c.instanceOperation(instance.ID)
		if !lock.TryLock() {
			return nil
		}
		defer lock.Unlock()
		latest, err := c.store.GetInstance(ctx, instance.ID)
		if err != nil {
			return err
		}
		if latest.PendingRelease || !latest.UpdatedAt.Equal(instance.UpdatedAt) {
			return nil
		}
		if instance.RouterWorkerID != "" {
			call, cancel := c.callContext(ctx)
			_, err := c.router.SetReadiness(call, instance.RouterWorkerID, false)
			cancel()
			if err != nil && !errors.Is(err, routeradapter.ErrWorkerNotFound) {
				return err
			}
		}
		return c.failStart(ctx, instance, &store.Operation{OperationID: c.operationID(instance.ID, store.OpStart), InstanceID: instance.ID, Type: store.OpStart, CreatedAt: c.now()}, "sglang process exited", errors.New(observedHealth.Detail))
	}
	if instance.InstanceState == domain.InstanceLost {
		if instance.ServiceState == domain.ServiceDraining {
			return c.FinishDrain(ctx, instance)
		}
		lock := c.instanceOperation(instance.ID)
		if !lock.TryLock() {
			return nil
		}
		// The container came back. Never jump straight to SERVING: re-enter the
		// preparation pipeline so the environment is validated again.
		next := instance
		if next.ServiceState != domain.ServiceNone {
			next.ServiceState = domain.ServiceFailed
		}
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
			lock.Unlock()
			return err
		}
		lock.Unlock()
		report.Recovered = append(report.Recovered, instance.ID)
		instance = next
	}

	switch instance.InstanceState {
	case domain.InstancePreparing:
		lock := c.instanceOperation(instance.ID)
		if !lock.TryLock() {
			return nil
		}
		defer lock.Unlock()
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
		// A registration failure can leave a healthy model process running.
		// Repair its membership instead of sending a duplicate Start (which the
		// bootstrap correctly rejects). Probe above already verified health.
		call, cancel := c.callContext(ctx)
		status, statusErr := c.launcher.Status(call, instance.Endpoint)
		cancel()
		if statusErr == nil && status.Phase == launcher.PhaseRunning && instance.Role.Valid() {
			_, err := c.settleService(ctx, instance)
			return err
		}
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
		lock := c.instanceOperation(instance.ID)
		if !lock.TryLock() {
			return nil
		}
		defer lock.Unlock()
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
		if due, err := c.operationRetryDue(ctx, instance.ID, store.OpDrain); err != nil {
			return err
		} else if !due {
			return nil
		}
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
	lock := c.instanceOperation(instance.ID)
	if !lock.TryLock() {
		return "ok", nil
	}
	defer lock.Unlock()
	current, err := c.store.GetInstance(ctx, instance.ID)
	if err != nil {
		return "", err
	}
	if current.PendingRelease || current.PendingUpdate != nil || current.InstanceState != domain.InstanceIdle ||
		(current.ServiceState != domain.ServiceFailed && current.ServiceState != domain.ServiceHealthy &&
			current.ServiceState != domain.ServiceRegistering && current.ServiceState != domain.ServiceServing) {
		return "ok", nil
	}
	instance = current
	call, cancel := c.callContext(ctx)
	status, statusErr := c.launcher.Status(call, instance.Endpoint)
	cancel()
	if statusErr != nil {
		return "", statusErr
	}
	if status.Phase != launcher.PhaseRunning {
		if err := c.closeReadiness(ctx, instance); err != nil {
			return "", err
		}
		return "", c.failStart(ctx, instance, &store.Operation{OperationID: c.operationID(instance.ID, store.OpStart),
			InstanceID: instance.ID, Type: store.OpStart}, "sglang process is not running", fmt.Errorf("bootstrap phase %s", status.Phase))
	}
	if status.Role != string(instance.Role) || (status.ModelID != "" && status.ModelID != c.cfg.Controller.ModelID) {
		return "reregistered", c.beginDrain(ctx, instance.ID, "runtime role or model differs from assignment", c.DefaultDrainGrace(), false)
	}
	health, healthErr := c.Probe(ctx, instance)
	if healthErr != nil || !health.Healthy() {
		if err := c.closeReadiness(ctx, instance); err != nil {
			return "", err
		}
		return "", c.failStart(ctx, instance, &store.Operation{OperationID: lifecycleOperationID(instance, store.OpStart),
			InstanceID: instance.ID, Type: store.OpStart}, "running model is not healthy", fmt.Errorf("status=%s error=%v", health.Status, healthErr))
	}
	if instance.ServiceState == domain.ServiceFailed {
		// Re-enter each legal state using the already running process. No model
		// launch is needed, but FAILED must not jump directly to SERVING.
		for _, state := range []domain.ServiceState{domain.ServiceStarting, domain.ServiceHealthy, domain.ServiceRegistering} {
			instance.ServiceState = state
			if err := c.transition(ctx, store.Transition{InstanceID: instance.ID, Next: instance}); err != nil {
				return "", err
			}
		}
	}
	// A control-plane restart can leave the row at HEALTHY before the
	// registration intent was committed. Follow the legal edge first.
	if instance.ServiceState == domain.ServiceHealthy {
		instance.ServiceState = domain.ServiceRegistering
		if err := c.transition(ctx, store.Transition{InstanceID: instance.ID, Next: instance}); err != nil {
			return "", err
		}
	}
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
		if err := c.beginDrain(ctx, instance.ID, "service without role", 0, false); err != nil {
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
	previousWorkerID := instance.RouterWorkerID
	instance.RouterWorkerID = registration.WorkerID
	generation, ready, err := c.ensureReadiness(ctx, registration.WorkerID)
	if err != nil {
		return "", c.failStart(ctx, instance, &store.Operation{OperationID: c.operationID(instance.ID, store.OpRegister),
			InstanceID: instance.ID, Type: store.OpRegister}, "router readiness failed during reconcile", err)
	}
	if !ready {
		return "", fmt.Errorf("worker %s readiness is not routable after reconcile", registration.WorkerID)
	}

	if instance.ServiceState == domain.ServiceServing &&
		previousWorkerID == registration.WorkerID &&
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
	if instance.ServiceState == domain.ServiceServing && previousWorkerID != registration.WorkerID {
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
	instance, err := c.store.GetInstance(ctx, instanceID)
	if err != nil {
		return false, err
	}
	operationID := lifecycleOperationID(instance, kind)
	if kind == store.OpDrain {
		operationID = drainOperationID(instance)
	}
	if operation, err := c.store.GetOperation(ctx, operationID); err == nil {
		return operation.NextRetryAt.IsZero() || !c.now().Before(operation.NextRetryAt), nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	operations, err := c.store.ListOpenOperations(ctx, 200)
	if err != nil {
		return false, err
	}
	// Legacy versions generated a new operation id on every attempt. The most
	// recent attempt owns the retry delay, not the first row in the queue.
	for index := len(operations) - 1; index >= 0; index-- {
		operation := operations[index]
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
			if !instance.PendingRelease && instance.InstanceState == domain.InstancePreparing {
				resumed = append(resumed, operation.OperationID)
				if err := c.runPrepare(ctx, instance.ID, operation.OperationID); err != nil {
					c.log().Warn("prepare resume failed", "instance_id", instance.ID, "error", err.Error())
				}
			}
		case store.OpStart:
			if !instance.PendingRelease && instance.PendingUpdate == nil && instance.StartAttempts < c.cfg.Controller.StartMaxAttempts && instance.InstanceState == domain.InstanceIdle && instance.Role.Valid() &&
				(instance.ServiceState == domain.ServiceFailed || instance.ServiceState == domain.ServiceNone) {
				if due, err := c.operationRetryDue(ctx, instance.ID, store.OpStart); err != nil || !due {
					continue
				}
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
	resumedEvents, err := c.ResumePendingEvents(ctx)
	if err != nil {
		return ReconcileReport{}, nil, err
	}
	resumed, err := c.ResumeOpenOperations(ctx)
	if err != nil {
		return ReconcileReport{}, nil, err
	}
	report, err := c.ReconcileOnce(ctx)
	if err != nil {
		return report, resumed, err
	}
	report.ResumedEvents += resumedEvents
	c.log().Info("control plane recovery finished",
		"checked", report.Checked,
		"lost", len(report.Lost),
		"recovered", len(report.Recovered),
		"reregistered", len(report.Reregister),
		"resumed_operations", len(resumed),
		"resumed_events", report.ResumedEvents)
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
