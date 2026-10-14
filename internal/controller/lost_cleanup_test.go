package controller

import (
	"context"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/store"
)

// lostRow stages an instance that stopped answering, the way the reconciler
// leaves one: the identity is intact, the service state is retained, and the
// container is unreachable.
func lostRow(t *testing.T, h *harness, role domain.Role) domain.Instance {
	t.Helper()
	h.handle(h.addedEvent("node", h.bootstrap.Endpoint(), "lease", h.clock.Now()))
	if err := h.ctrl.StartService(t.Context(), "node", role); err != nil {
		t.Fatalf("start: %v", err)
	}
	lost := h.instance("node")
	lost.InstanceState = domain.InstanceLost
	lost.LastError = "container endpoint is unreachable"
	restage(t, h, lost)
	return h.instance("node")
}

// A container that never answers again parks in LOST with the preparation
// pipeline's budget spent, and nothing retries it. Describing that as
// "recovering" is the lie that keeps a pool short: the planner holds back the
// capacity request while waiting for a worker that is never coming back.
func TestLostRowWithASpentPrepareBudgetIsStrandedNotRecovering(t *testing.T) {
	h := newHarness(t)
	lost := lostRow(t, h, domain.RolePrefill)

	// Same row, still on its way back: the preparation pipeline has attempts.
	recovering, err := h.ctrl.Rebalance(t.Context())
	if err != nil {
		t.Fatalf("planner round: %v", err)
	}
	if recovering.RecoveringCount != 1 || recovering.StrandedCount != 0 {
		t.Fatalf("an unexhausted LOST row must read as recovering: %+v", recovering)
	}

	// Now the pipeline gives up, which is what runPrepare does when the health
	// probe never answers: the row stays LOST and the operation fails.
	exhausted := lost
	exhausted.PrepareAttempts = h.ctrl.cfg.Controller.PrepareMaxAttempts
	restage(t, h, exhausted)
	if err := h.ctrl.store.ApplyTransition(context.Background(), store.Transition{
		InstanceID: "node",
		Next:       exhausted,
		Operation: &store.Operation{
			OperationID: lifecycleOperationID(exhausted, store.OpPrepare), InstanceID: "node",
			Type: store.OpPrepare, Status: store.OpFailed, Attempt: exhausted.PrepareAttempts,
			CreatedAt: h.clock.Now(),
		},
	}); err != nil {
		t.Fatalf("stage the failed prepare operation: %v", err)
	}

	stranded, err := h.ctrl.Rebalance(t.Context())
	if err != nil {
		t.Fatalf("planner round: %v", err)
	}
	if stranded.StrandedCount != 1 {
		t.Fatalf("a LOST row nothing will retry must read as stranded: recovering=%d stranded=%d",
			stranded.RecoveringCount, stranded.StrandedCount)
	}
	if stranded.RecoveringCount != 0 {
		t.Fatalf("recovering_count = %d for a dead end", stranded.RecoveringCount)
	}
	if !containsNote(stranded.Notes, "operator has to requeue or release") {
		t.Fatalf("the round must say what the operator has to do, got %v", stranded.Notes)
	}
}

// The automatic cleanup already exists and this is what proves it: the moment
// the partner stops listing a container, the pull path revokes it, and the
// reclaim runs through the LOST-aware drain - which must not spend a call on a
// container that does not answer.
func TestRevokedContainerCleansUpALostRow(t *testing.T) {
	h := newHarness(t)
	lostRow(t, h, domain.RolePrefill)

	_, stopsBefore, _ := h.bootstrap.counts()
	h.handle(h.revokedEvent("node", 0, h.clock.Now()))
	if outcome := h.handle(h.revokedEvent("node", 0, h.clock.Now())); outcome.Result != domain.ResultApplied {
		t.Fatalf("a repeated revoke must be idempotent, got %s", outcome.Result)
	}

	// The drain has nothing to wait for and the release has nothing to ask: the
	// endpoint is mute, so a /bootstrap/stop would only burn the retry budget.
	for round := 0; round < 4; round++ {
		h.clock.Advance(time.Minute)
		h.reconcile()
	}
	final := h.instance("node")
	if final.InstanceState != domain.InstanceReleased {
		t.Fatalf("a LOST row whose container is gone must be released, got %s/%s (error %q)",
			final.InstanceState, final.ServiceState, final.LastError)
	}
	if _, stopsAfter, _ := h.bootstrap.counts(); stopsAfter != stopsBefore {
		t.Fatalf("a mute container was asked to stop %d time(s)", stopsAfter-stopsBefore)
	}
	if final.PendingRelease {
		t.Fatal("the reclaim intent outlived the release")
	}
	// A released row is out of the pool: neither serving nor idle, and not
	// reported as capacity in either limbo bucket.
	decision, err := h.ctrl.Rebalance(t.Context())
	if err != nil {
		t.Fatalf("planner round: %v", err)
	}
	if decision.RecoveringCount != 0 || decision.StrandedCount != 0 || decision.ServingCount != 0 {
		t.Fatalf("a released row leaked into the plan: %+v", decision)
	}
}

// A reclaim also has to work while the container is still listed but mute:
// that is the case the platform has not cleaned up for us, and the operator's
// verb has to be enough on its own.
func TestReclaimOfAMuteContainerDoesNotHangInDraining(t *testing.T) {
	h := newHarness(t)
	lostRow(t, h, domain.RoleDecode)

	if err := h.ctrl.RequestReclaim(t.Context(), "node", "operator gave the container back", time.Minute); err != nil {
		t.Fatalf("request reclaim: %v", err)
	}
	for round := 0; round < 4; round++ {
		h.clock.Advance(time.Minute)
		h.reconcile()
	}
	final := h.instance("node")
	if final.InstanceState != domain.InstanceReleased {
		t.Fatalf("the reclaim never finished: %s/%s pending_release=%t error=%q",
			final.InstanceState, final.ServiceState, final.PendingRelease, final.LastError)
	}
	if final.ServiceState == domain.ServiceDraining {
		t.Fatal("the reclaim is stuck in DRAINING, waiting for a container that cannot answer")
	}
}
