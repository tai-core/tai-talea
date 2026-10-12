package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/launcher"
	"github.com/tai-core/tai-talea/internal/store"
)

// restage writes a row back through the same state machine the controller uses,
// so the staged states below are ones the control plane could really produce.
// Only a counter moves, which is why every transition here is a legal no-op.
func restage(t *testing.T, h *harness, next domain.Instance) {
	t.Helper()
	if err := h.ctrl.store.ApplyTransition(t.Context(), store.Transition{InstanceID: next.ID, Next: next}); err != nil {
		t.Fatalf("stage %s/%s attempts=%d: %v", next.InstanceState, next.ServiceState, next.StartAttempts, err)
	}
}

// parkInstance drives one instance to the state an exhausted start budget
// leaves behind: IDLE / FAILED with the role still assigned and every attempt
// spent. It returns the parked row.
//
// The failure is shaped like the real one rather than like a health blip: the
// process is gone and /bootstrap/start answers an error, so each round spends
// an attempt through StartService. A container that merely reports an unhealthy
// service takes the other route (settleService), which repairs membership and
// deliberately does not spend the budget.
func parkInstance(t *testing.T, h *harness) domain.Instance {
	t.Helper()
	h.handle(h.addedEvent("node", h.bootstrap.Endpoint(), "lease", h.clock.Now()))
	if err := h.ctrl.StartService(t.Context(), "node", domain.RoleDecode); err != nil {
		t.Fatalf("first start: %v", err)
	}
	h.bootstrap.mu.Lock()
	h.bootstrap.phase = launcher.PhaseFailed
	h.bootstrap.servicePID = 0
	h.bootstrap.failStart = true
	h.bootstrap.mu.Unlock()

	budget := h.ctrl.cfg.Controller.StartMaxAttempts
	for round := 0; round <= budget+2; round++ {
		h.clock.Advance(time.Minute)
		if _, err := h.ctrl.ReconcileOnce(t.Context()); err != nil {
			t.Fatalf("reconcile round %d: %v", round, err)
		}
	}
	return h.instance("node")
}

// TestExhaustedStartBudgetStrandsTheInstance pins the deadlock rather than the
// fix, because the deadlock is what an operator has to recognise: the
// reconciler stops retrying at the cap, and the planner cannot pick the
// instance up either - a parked row still holds a valid role, so it is not
// idle, while FAILED is not a live service, so it is not serving. Both halves
// of the state machine close at once and nothing is left to move it.
//
// If this test ever fails because the planner learned to adopt such an
// instance, that is good news: RequeueService becomes a convenience instead of
// the only way out, and this test should be rewritten rather than deleted.
func TestExhaustedStartBudgetStrandsTheInstance(t *testing.T) {
	h := newHarness(t)
	parked := parkInstance(t, h)

	if parked.InstanceState != domain.InstanceIdle || parked.ServiceState != domain.ServiceFailed {
		t.Fatalf("expected a parked IDLE/FAILED row, got %s/%s", parked.InstanceState, parked.ServiceState)
	}
	if parked.StartAttempts != h.ctrl.cfg.Controller.StartMaxAttempts {
		t.Fatalf("start attempts = %d, want the budget spent (%d)",
			parked.StartAttempts, h.ctrl.cfg.Controller.StartMaxAttempts)
	}
	if !parked.Role.Valid() {
		t.Fatalf("a parked instance keeps its role, otherwise the planner would own the recovery: %+v", parked)
	}

	// The reconciler has stopped trying.
	for round := 0; round < 3; round++ {
		h.clock.Advance(time.Minute)
		if _, err := h.ctrl.ReconcileOnce(t.Context()); err != nil {
			t.Fatalf("reconcile after parking: %v", err)
		}
	}
	if got := h.instance("node").StartAttempts; got != h.ctrl.cfg.Controller.StartMaxAttempts {
		t.Fatalf("the control plane retried past the cap: attempts=%d", got)
	}

	// And the planner does not see it at all: neither as serving capacity nor
	// as idle capacity.
	decision, err := h.ctrl.Rebalance(t.Context())
	if err != nil {
		t.Fatalf("planner round: %v", err)
	}
	if decision.ServingCount != 0 || decision.IdleCount != 0 {
		t.Fatalf("the planner can see a parked instance: serving=%d idle=%d",
			decision.ServingCount, decision.IdleCount)
	}
	// Nothing changed, so the role is still there and still unusable.
	if got := h.instance("node").Role; got != domain.RoleDecode {
		t.Fatalf("role = %q after a planner round, want the original assignment", got)
	}
}

// TestRequeueRestoresTheStartBudget is the exit an operator needs after the
// escalation alert fired. It has to leave the container, the placement and the
// assigned role alone: drain stops the model and clears the role, release and
// reclaim give the machine back, and none of those is what "try again" means.
func TestRequeueRestoresTheStartBudget(t *testing.T) {
	h := newHarness(t)
	parked := parkInstance(t, h)

	if err := h.ctrl.RequeueService(t.Context(), "node", "operator requeued the start budget"); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	requeued := h.instance("node")
	if requeued.StartAttempts != 0 {
		t.Fatalf("start attempts = %d after a requeue, want 0", requeued.StartAttempts)
	}
	if requeued.ServiceState != domain.ServiceFailed || requeued.InstanceState != domain.InstanceIdle {
		t.Fatalf("a requeue must not invent a live service: %s/%s", requeued.InstanceState, requeued.ServiceState)
	}
	if requeued.Role != parked.Role {
		t.Fatalf("role changed from %q to %q; a requeue keeps the assignment", parked.Role, requeued.Role)
	}
	if requeued.LastError != "" {
		t.Fatalf("the stale failure reason survived: %q", requeued.LastError)
	}

	// The point of the verb: the next round actually retries, without waiting
	// for a retry gate that was never written.
	h.bootstrap.mu.Lock()
	h.bootstrap.failStart = false
	h.bootstrap.phase = launcher.PhaseRunning
	h.bootstrap.servicePID = 4242
	h.bootstrap.mu.Unlock()
	if _, err := h.ctrl.ReconcileOnce(t.Context()); err != nil {
		t.Fatalf("reconcile after requeue: %v", err)
	}
	if got := h.instance("node").ServiceState; got != domain.ServiceServing {
		t.Fatalf("service_state = %s after a requeue and a healthy reconcile, want SERVING", got)
	}
}

// TestRequeueIsNarrowOnPurpose keeps the verb from becoming a second drain or a
// way to hammer a broken launch by hand.
func TestRequeueIsNarrowOnPurpose(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	// A healthy serving instance: the verb has nothing to restore.
	h.handle(h.addedEvent("node", h.bootstrap.Endpoint(), "lease", h.clock.Now()))
	if err := h.ctrl.StartService(ctx, "node", domain.RolePrefill); err != nil {
		t.Fatal(err)
	}
	err := h.ctrl.RequeueService(ctx, "node", "operator")
	if err == nil || !strings.Contains(err.Error(), "IDLE/FAILED") {
		t.Fatalf("requeue of a serving instance = %v, want a refusal", err)
	}

	// Parked but with attempts left: the reconciler is already retrying it, so
	// an operator spending the budget early would only be a manual hammer.
	first := parkInstance(t, h)
	midway := first
	midway.StartAttempts = h.ctrl.cfg.Controller.StartMaxAttempts - 1
	restage(t, h, midway)
	err = h.ctrl.RequeueService(ctx, "node", "operator")
	if err == nil || !strings.Contains(err.Error(), "still has") {
		t.Fatalf("requeue with attempts left = %v, want a refusal", err)
	}

	// No role: the planner owns that instance, and the reconciler has no role
	// to start. IDLE/FAILED with no role is a legal combination, which is why
	// this has to be refused explicitly rather than by the state machine.
	roleless := midway
	roleless.StartAttempts = 9
	roleless.Role = domain.RoleNone
	restage(t, h, roleless)
	err = h.ctrl.RequeueService(ctx, "node", "operator")
	if err == nil || !strings.Contains(err.Error(), "no assigned role") {
		t.Fatalf("requeue without a role = %v, want a refusal", err)
	}
}
