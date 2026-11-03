package controller

import (
	"context"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/obs"
	"github.com/tai-core/tai-talea/internal/partner"
	"github.com/tai-core/tai-talea/internal/store"
)

// Plan P1-11: an invalid load input while instances serve used to be silent -
// the planner kept deciding with no eyes, and nothing in the logs, metrics or
// alerts said so. It must alert on the transition, at a bounded rate, and go
// quiet the moment the input is valid again.
func TestLoadInputInvalidWhileServingAlertsAtABoundedRate(t *testing.T) {
	h := newHarness(t,
		partner.CapacityInstance{ID: "container-1", Endpoint: "http://10.0.0.1:8080", LeaseID: "lease-1"},
	)
	h.handle(h.addedEvent("container-1", "http://10.0.0.1:8080", "lease-1", h.clock.Now()))
	node := h.instance("container-1")
	// SERVING is only reachable through the legal edges; walk them like
	// settleService does.
	node.Role = domain.RolePrefill
	for _, state := range []domain.ServiceState{domain.ServiceStarting, domain.ServiceHealthy, domain.ServiceRegistering, domain.ServiceServing} {
		node.ServiceState = state
		if err := h.store.ApplyTransition(t.Context(), store.Transition{InstanceID: node.ID, Next: node}); err != nil {
			t.Fatal(err)
		}
	}
	// The telemetry collector never sampled successfully: the harness's
	// container endpoints are unreachable fakes, so the planner's load input
	// is invalid for as long as this instance serves.

	if _, err := h.ctrl.Rebalance(context.Background()); err != nil {
		t.Fatalf("rebalance: %v", err)
	}
	if !h.recorder.has(obs.AlertLoadUnavailable) {
		t.Fatalf("invalid load input while serving did not alert: %v", h.recorder.names())
	}

	// An immediate second round is inside the alert cooldown: no duplicate.
	h.recorder.reset()
	if _, err := h.ctrl.Rebalance(context.Background()); err != nil {
		t.Fatalf("second rebalance: %v", err)
	}
	if h.recorder.has(obs.AlertLoadUnavailable) {
		t.Fatalf("load alert re-fired inside its cooldown window: %v", h.recorder.names())
	}

	// Past the cooldown the alert fires again - the input is still invalid.
	h.clock.Advance(time.Minute)
	if _, err := h.ctrl.Rebalance(context.Background()); err != nil {
		t.Fatalf("third rebalance: %v", err)
	}
	if !h.recorder.has(obs.AlertLoadUnavailable) {
		t.Fatalf("persistent invalid input stopped alerting after the cooldown: %v", h.recorder.names())
	}
}

// Without serving instances there is no decision to make, so an invalid load
// input is expected, not an incident.
func TestLoadInputInvalidWithoutServingStaysQuiet(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
	if _, err := h.ctrl.Rebalance(context.Background()); err != nil {
		t.Fatalf("rebalance: %v", err)
	}
	h.ctrl.AwaitStarts()
	if h.recorder.has(obs.AlertLoadUnavailable) {
		t.Fatalf("alerted with nothing serving: %v", h.recorder.names())
	}
}
