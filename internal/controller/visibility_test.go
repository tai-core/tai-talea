package controller

import (
	"strings"
	"testing"

	"github.com/tai-core/tai-talea/internal/domain"
)

// A parked worker used to be invisible to the planner: it keeps its role, so it
// is not idle capacity, and FAILED is not a live service, so it is not serving
// capacity either. The pool then looked as if the machine had never been
// delivered, and the 2026-10-13 round reacted by rebalancing the surviving
// worker until both roles were gone. This is the visibility half of that fix.
func TestParkedWorkerIsVisibleToThePlanner(t *testing.T) {
	h := newHarness(t)
	// No cooldown: the budget is spent and nothing will try again on its own.
	parked := parkInstance(t, h)
	if parked.ServiceState != domain.ServiceFailed {
		t.Fatalf("expected a failed service, got %s", parked.ServiceState)
	}

	decision, err := h.ctrl.Rebalance(t.Context())
	if err != nil {
		t.Fatalf("planner round: %v", err)
	}
	if decision.StrandedCount != 1 {
		t.Fatalf("stranded_count = %d, want the parked worker reported", decision.StrandedCount)
	}
	if decision.RecoveringCount != 0 {
		t.Fatalf("recovering_count = %d while nothing will retry the instance", decision.RecoveringCount)
	}
	// It is still not capacity: the same round that reports it must not count
	// it as something that can serve traffic.
	if decision.ServingCount != 0 || decision.IdleCount != 0 {
		t.Fatalf("a parked worker leaked into the capacity counts: serving=%d idle=%d",
			decision.ServingCount, decision.IdleCount)
	}
}

// With a cooldown configured the same row is on its way back, and that is the
// state in which the planner must not answer a shortfall by buying capacity.
func TestReArmableWorkerIsReportedAsRecovering(t *testing.T) {
	h := newHarness(t)
	withCooldown(h, parkedFor, 3)
	parkInstance(t, h)

	decision, err := h.ctrl.Rebalance(t.Context())
	if err != nil {
		t.Fatalf("planner round: %v", err)
	}
	if decision.RecoveringCount != 1 {
		t.Fatalf("recovering_count = %d, want the re-armable worker reported", decision.RecoveringCount)
	}
	if decision.StrandedCount != 0 {
		t.Fatalf("stranded_count = %d while a re-arm is coming", decision.StrandedCount)
	}
	if !containsNote(decision.Notes, "not counted as capacity") {
		t.Fatalf("the round must say the worker is not being counted, got %v", decision.Notes)
	}
}

// A container that stopped answering is the other half of the blind spot. Its
// service state is retained - the control plane does not know the model is
// dead, only that it cannot reach it - so without an explicit case it lands in
// no bucket at all and the pool silently shrinks by one.
func TestLostWorkerIsVisibleToThePlanner(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("node", h.bootstrap.Endpoint(), "lease", h.clock.Now()))
	if err := h.ctrl.StartService(t.Context(), "node", domain.RolePrefill); err != nil {
		t.Fatalf("start: %v", err)
	}

	serving := h.instance("node")
	lost := serving
	lost.InstanceState = domain.InstanceLost
	lost.LastError = "container endpoint is unreachable"
	restage(t, h, lost)

	decision, err := h.ctrl.Rebalance(t.Context())
	if err != nil {
		t.Fatalf("planner round: %v", err)
	}
	if decision.RecoveringCount != 1 {
		t.Fatalf("recovering_count = %d, want the unreachable worker reported (notes %v)",
			decision.RecoveringCount, decision.Notes)
	}
	if decision.ServingCount != 0 {
		t.Fatalf("an unreachable worker was counted as serving: %d", decision.ServingCount)
	}
}

func containsNote(notes []string, fragment string) bool {
	for _, note := range notes {
		if strings.Contains(note, fragment) {
			return true
		}
	}
	return false
}
