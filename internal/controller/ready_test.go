package controller

import (
	"context"
	"testing"
)

// Plan P1-7: a control plane whose startup recovery failed must not report
// ready. main defers the ready flag to the first reconcile round that
// completes without error; that hook fires exactly once.
func TestFirstSuccessfulRoundFiresTheReadinessHookOnce(t *testing.T) {
	h := newHarness(t)
	fired := 0
	h.ctrl.OnFirstSuccessfulRound = func() { fired++ }

	h.ctrl.reconcileDriver(context.Background())
	if fired != 1 {
		t.Fatalf("first successful round fired the hook %d times, want 1", fired)
	}
	h.ctrl.reconcileDriver(context.Background())
	h.ctrl.reconcileDriver(context.Background())
	if fired != 1 {
		t.Fatalf("later rounds re-fired the hook: %d times", fired)
	}
}
