package controller

import (
	"testing"

	"github.com/tai-core/tai-talea/internal/domain"
)

// Plan P1-8: one failed probe used to condemn a container as LOST on the spot,
// so a dropped packet cost the whole re-preparation pipeline. A LOST verdict
// now waits for a streak of consecutive unreachable rounds; a container that
// answers again in between gets its streak cleared and keeps its state.
func TestUnreachableContainerNeedsAStreakBeforeLost(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("node", h.bootstrap.Endpoint(), "lease", h.clock.Now()))
	h.bootstrap.SetDown(true)

	for round := 1; round < h.ctrl.LostFailureThreshold(); round++ {
		report := h.reconcile()
		if got := h.instance("node").InstanceState; got == domain.InstanceLost {
			t.Fatalf("round %d: condemned below the streak threshold: %+v", round, report)
		}
	}
	report := h.reconcile()
	if got := h.instance("node").InstanceState; got != domain.InstanceLost {
		t.Fatalf("a container dead for %d rounds was not marked LOST: %+v", h.ctrl.LostFailureThreshold(), report)
	}
}

// A container that comes back before the streak completes keeps its state,
// and a fresh miss afterwards starts a new streak instead of inheriting the
// old one.
func TestContainerThatReturnsBeforeTheStreakCompletesIsNotLost(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("node", h.bootstrap.Endpoint(), "lease", h.clock.Now()))

	h.bootstrap.SetDown(true)
	h.reconcile()
	h.reconcile() // streak = 2 of 3: not yet condemned

	h.bootstrap.SetDown(false)
	report := h.reconcile()
	if got := h.instance("node").InstanceState; got == domain.InstanceLost {
		t.Fatalf("a container that returned before the streak completed was lost: %+v", report)
	}
	if got := h.instance("node").InstanceState; got != domain.InstanceIdle {
		t.Fatalf("the returned container is not IDLE: state=%s", got)
	}

	// The streak was cleared: one fresh miss must not inherit the old count.
	h.bootstrap.SetDown(true)
	h.reconcile()
	if got := h.instance("node").InstanceState; got == domain.InstanceLost {
		t.Fatalf("a new streak inherited the old failures")
	}
}
