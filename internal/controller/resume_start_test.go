package controller

import (
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/launcher"
	"github.com/tai-core/tai-talea/internal/store"
)

// A control-plane restart loses the in-memory owner of a STARTING instance
// while the bootstrap it dispatched is still loading the model. The reconcile
// pass that notices the missing owner must resume the wait, not condemn the
// start as "interrupted" - the fake-FAILED of 2026-11-02.
func TestRestartedControlPlaneResumesLoadingStartInsteadOfFakingFailed(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
	node := h.instance("container-1")
	node.Role, node.ServiceState = domain.RolePrefill, domain.ServiceStarting
	if err := h.store.ApplyTransition(t.Context(), store.Transition{InstanceID: node.ID, Next: node}); err != nil {
		t.Fatal(err)
	}
	// The store stamps updated_at with wall time; age the instance through the
	// test clock so the "stuck" heuristic fires (older than 3 rounds).
	h.clock.Advance(time.Minute)
	// The bootstrap is still supervising the service it was told to start.
	h.bootstrap.mu.Lock()
	h.bootstrap.phase, h.bootstrap.role = launcher.PhaseRunning, string(domain.RolePrefill)
	h.bootstrap.mu.Unlock()

	report := h.reconcile()
	if len(report.Resumed) != 1 {
		t.Fatalf("resume was not reported: %+v", report)
	}
	next := h.instance("container-1")
	if next.ServiceState == domain.ServiceFailed {
		t.Fatalf("a loading start was condemned as interrupted: %+v", next)
	}
	if next.ServiceState != domain.ServiceServing {
		t.Fatalf("loading start was not driven to SERVING: %+v", next)
	}
	if startCalls, _, _ := h.bootstrap.counts(); startCalls != 0 {
		t.Fatalf("resume re-dispatched a start the bootstrap already runs: %d starts", startCalls)
	}
}

// When the bootstrap supervises nothing, the interrupted start is real and the
// old behavior stands: fail it, and let the FAILED requeue path take over.
func TestInterruptedStartWithNothingRunningStillFails(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
	node := h.instance("container-1")
	node.Role, node.ServiceState = domain.RolePrefill, domain.ServiceStarting
	if err := h.store.ApplyTransition(t.Context(), store.Transition{InstanceID: node.ID, Next: node}); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Minute)
	h.bootstrap.mu.Lock()
	h.bootstrap.phase = launcher.PhaseIdle
	h.bootstrap.mu.Unlock()

	report := h.reconcile()
	if got := h.instance("container-1").ServiceState; got != domain.ServiceFailed {
		t.Fatalf("an interrupted start was not failed: state=%s", got)
	}
	if len(report.Reported) != 1 {
		t.Fatalf("interrupted start was not reported: %+v", report)
	}
}
