package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/obs"
	"github.com/tai-core/tai-talea/internal/partner"
	"github.com/tai-core/tai-talea/internal/planner"
	"github.com/tai-core/tai-talea/internal/store"
)

// Rebalance runs one PD planner round and applies the resulting actions.
//
// Milestone 1 behaviour (§11):
//   - fixed ratio with manual gears;
//   - max_change_per_round, min_hold and cooldown are honoured;
//   - idle instances are preferred for new role assignments;
//   - capacity is only ever taken from what the partner already offered.
func (c *Controller) Rebalance(ctx context.Context) (planner.Decision, error) {
	instances, err := c.store.ListInstances(ctx)
	if err != nil {
		return planner.Decision{}, err
	}

	input := planner.Input{Now: c.now()}
	var (
		prefillServing int
		decodeServing  int
	)
	for _, instance := range instances {
		if instance.PendingRelease || instance.PendingUpdate != nil {
			continue
		}
		switch {
		case instance.InstanceState == domain.InstanceIdle && instance.Role.Valid() && hasLiveService(instance):
			roleSince := instance.RoleAssignedAt
			if roleSince.IsZero() {
				roleSince = instance.UpdatedAt
			}
			input.Serving = append(input.Serving, planner.Candidate{
				ID:        instance.ID,
				Role:      instance.Role,
				Serving:   instance.ServiceState == domain.ServiceServing,
				RoleSince: roleSince,
				Assigned:  true,
			})
			if instance.Role == domain.RolePrefill {
				prefillServing++
			} else {
				decodeServing++
			}
		case instance.InstanceState == domain.InstanceIdle && !instance.Role.Valid() && assignable(instance):
			input.Idle = append(input.Idle, planner.Candidate{ID: instance.ID})
		}
	}
	sort.Slice(input.Serving, func(i, j int) bool { return input.Serving[i].ID < input.Serving[j].ID })
	sort.Slice(input.Idle, func(i, j int) bool { return input.Idle[i].ID < input.Idle[j].ID })

	// Router in-flight counts are not scheduler queue lengths. Only fresh,
	// complete scheduler samples are passed as measured planner inputs.
	load := c.Telemetry("")
	input.Load.MeasuredAt = load.At
	input.Load.Valid = load.Complete
	input.Load.Advice = load.Advice
	if load.Complete {
		input.Load.QueueLength = *load.Prefill.Queue + *load.Decode.Queue
		input.Load.PrefillPressure = load.Prefill.Pressure
		input.Load.DecodePressure = load.Decode.Pressure
		input.Load.PDLoadImbalance = *load.PressureGap
		input.Load.InputTokenRate = load.Prefill.TokenRate
		input.Load.OutputTokenRate = load.Decode.TokenRate
	}

	decision, err := c.planner.Plan(input)
	if err != nil {
		return planner.Decision{}, err
	}
	c.publishPlanMetrics(prefillServing, decodeServing, decision)

	for _, action := range decision.Actions {
		switch action.Kind {
		case planner.ActionAssign:
			if err := c.StartService(ctx, action.InstanceID, action.Role); err != nil {
				c.log().Warn("planner assign failed",
					"instance_id", action.InstanceID, "role", string(action.Role), "error", err.Error())
				continue
			}
		case planner.ActionReassign:
			// Role conversion is staged through a drain: readiness closes first,
			// the next round assigns the new role once the port is free.
			if err := c.BeginDrain(ctx, action.InstanceID,
				fmt.Sprintf("planner rebalance %s -> %s", action.FromRole, action.Role), c.DefaultDrainGrace()); err != nil {
				c.log().Warn("planner reassign drain failed",
					"instance_id", action.InstanceID, "error", err.Error())
				continue
			}
		case planner.ActionDrain:
			if err := c.BeginDrain(ctx, action.InstanceID, action.Reason, c.DefaultDrainGrace()); err != nil {
				c.log().Warn("planner drain failed", "instance_id", action.InstanceID, "error", err.Error())
				continue
			}
		}
		if c.metrics != nil {
			c.metrics.IncCounter(obs.MetricPlannerChangesTotal,
				"PD planner role changes applied.",
				[]string{"kind", "role"}, "kind", string(action.Kind), "role", string(action.Role))
		}
		c.audit(ctx, auditForAction(action))
	}
	if len(decision.Actions) > 0 {
		c.log().Info("planner round applied", "summary", decision.DecisionSummary())
	}
	c.checkRatioDrift(ctx, decision)
	return decision, nil
}

func auditForAction(action planner.Action) store.AuditEntry {
	return store.AuditEntry{
		Action:     "planner_" + string(action.Kind),
		InstanceID: action.InstanceID,
		Details: map[string]string{
			"role":      string(action.Role),
			"from_role": string(action.FromRole),
			"reason":    action.Reason,
		},
	}
}

// hasLiveService reports whether the instance carries a service state that the
// planner should keep managing.
func hasLiveService(instance domain.Instance) bool {
	switch instance.ServiceState {
	case domain.ServiceStarting, domain.ServiceHealthy, domain.ServiceRegistering,
		domain.ServiceServing, domain.ServiceDraining:
		return true
	}
	return false
}

// assignable reports whether an idle instance can take a role right now.
func assignable(instance domain.Instance) bool {
	if instance.ServiceState != domain.ServiceNone && instance.ServiceState != domain.ServiceFailed {
		return false
	}
	if instance.ServiceState == domain.ServiceFailed && instance.StartAttempts >= 1 {
		// Failed starts are retried by the reconciler, not raced by the planner.
		return false
	}
	return true
}

func (c *Controller) publishPlanMetrics(prefill, decode int, decision planner.Decision) {
	if c.metrics == nil {
		return
	}
	total := prefill + decode
	current := 0.0
	if total > 0 {
		current = float64(prefill) / float64(total)
	}
	c.metrics.SetGauge(obs.MetricPDRatioCurrent,
		"Observed Prefill share of serving instances.", nil, current)
	c.metrics.SetGauge(obs.MetricPDRatioTarget,
		"Planner target Prefill share of serving instances.", nil, decision.TargetRatio)
	c.metrics.SetGauge("capacity_serving_instances",
		"Instances currently holding a PD role.", nil, float64(total))
}

// checkRatioDrift alerts when the observed ratio stays away from the target for
// several consecutive rounds (§14: 实际 P/D 比例长期偏离目标).
func (c *Controller) checkRatioDrift(ctx context.Context, decision planner.Decision) {
	tolerance := c.cfg.Alerts.RatioTolerance
	if tolerance <= 0 {
		return
	}
	drift := decision.CurrentRatio - decision.TargetRatio
	if drift < 0 {
		drift = -drift
	}
	// A ratio can only be matched to within one instance, so the measurement
	// granularity is included in the tolerance.
	granularity := 0.0
	if total := decision.ServingCount + decision.IdleCount; total > 0 {
		granularity = 1 / float64(total)
	}
	if drift <= tolerance+granularity {
		c.mu.Lock()
		c.ratioStreak = 0
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	c.ratioStreak++
	streak := c.ratioStreak
	c.mu.Unlock()
	if streak < 3 {
		return
	}
	c.alert(ctx, obs.Alert{
		Name:     obs.AlertPDRatioDrift,
		Severity: obs.SeverityWarning,
		Message:  "observed P/D ratio has been off target for several planner rounds",
		Details: map[string]string{
			"gear":           decision.Gear,
			"target_prefill": fmt.Sprintf("%d", decision.TargetPrefill),
			"target_decode":  fmt.Sprintf("%d", decision.TargetDecode),
			"serving_count":  fmt.Sprintf("%d", decision.ServingCount),
			"drift":          fmt.Sprintf("%.3f", drift),
			"rounds":         fmt.Sprintf("%d", streak),
		},
	})
}

// RunPeriodic drives reconcile, rebalance and gauge publishing until the
// context is cancelled. Partner pull runs in its own loop so a slow partner
// cannot delay container reconciliation.
func (c *Controller) RunPeriodic(ctx context.Context) {
	go c.telemetry.Run(ctx)
	interval := c.cfg.Controller.ReconcileInterval.Duration()
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := c.ReconcileOnce(ctx); err != nil {
				c.log().Error("reconcile round failed", "error", err.Error())
				c.alert(ctx, obs.Alert{
					Name:     obs.AlertReconcileFailed,
					Severity: obs.SeverityCritical,
					Message:  "reconcile round failed",
					Details:  map[string]string{"error": err.Error()},
				})
			}
			if _, err := c.Rebalance(ctx); err != nil {
				c.log().Error("planner round failed", "error", err.Error())
			}
			if err := c.PublishGauges(ctx); err != nil {
				c.log().Warn("publishing gauges failed", "error", err.Error())
			}
		}
	}
}

// PullSchedule binds one partner adapter to its pull reconciler.
type PullSchedule struct {
	Adapter  partner.PartnerAdapter
	Puller   *partner.Puller
	Interval time.Duration
}

// RunPull executes the Pull reconciliation loop for one partner (§6.2 steps 3-5).
func (c *Controller) RunPull(ctx context.Context, schedule PullSchedule) {
	interval := schedule.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.PullOnce(ctx, schedule); err != nil {
				// Plan already recorded the failure metric and alert.
				c.log().Debug("pull round incomplete", "partner_id", schedule.Adapter.PartnerID(), "error", err.Error())
			}
		}
	}
}

// PullOnce performs one full pull round: plan, apply every generated event and
// only then commit the snapshot as the new authoritative version.
func (c *Controller) PullOnce(ctx context.Context, schedule PullSchedule) error {
	return c.pullAuthoritative(ctx, schedule)
}
