package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/config"
	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/planner"
)

// A round that waits for its own starts cannot notice a container that became
// ready a few seconds after the others. Measured on a cold start: four Pods
// created together, two of them prepared twenty seconds later, and the round in
// between waited 3 min 40 s for the pair it had dispatched before it could
// assign the other two - 7 min 24 s for a pool that needs 3 min 45 s.
func TestARoundReturnsWhileItsStartIsStillLoading(t *testing.T) {
	h := newHarness(t)
	loading := h.bootstrap.Endpoint()
	gate := &endpointGate{
		Launcher: h.ctrl.launcher,
		hold:     loading,
		entered:  make(chan string, 1),
		release:  make(chan struct{}),
	}
	h.ctrl.launcher = gate
	// The gate parks on the call context; the harness default would release it
	// after three seconds and the test would then be measuring the timeout.
	h.ctrl.cfg.Controller.CallTimeout = config.Duration(60 * time.Second)

	if outcome := h.handle(h.addedEvent("container-1", loading, "lease-1", h.clock.Now())); outcome.Result != domain.ResultApplied {
		t.Fatalf("offer: %s %s", outcome.Result, outcome.Reason)
	}

	done := make(chan error, 1)
	go func() {
		_, err := h.ctrl.Rebalance(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("rebalance: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the round is still waiting for the start it dispatched")
	}

	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the round returned but never dispatched the start")
	}
	if instance := h.instance("container-1"); instance.ServiceState != domain.ServiceStarting {
		t.Fatalf("the dispatched start did not claim the instance: %s", instance.ServiceState)
	}

	close(gate.release)
	h.ctrl.AwaitStarts()
	if instance := h.instance("container-1"); instance.ServiceState != domain.ServiceServing {
		t.Fatalf("the start did not finish: %s (%s)", instance.ServiceState, instance.LastError)
	}
}

// A round that returns early can be re-entered while its start is still in
// flight, so the same container can be offered to applyStarts twice. The
// instance lock already stops the second one from starting anything - it parks
// on the lock and finds the worker SERVING afterwards - but it parks for the
// whole load while holding a start slot. On a pool sized to the slot count that
// is the difference between four workers loading and three, so the instance is
// reserved from dispatch until the start returns.
//
// The duplicate leaves no other trace, which is why this pins the contract of
// the reservation rather than the absence of a second listen: measured against
// the gate, both versions look identical.
func TestOneStartAtATimePerInstance(t *testing.T) {
	h := newHarness(t)
	loading := h.bootstrap.Endpoint()
	gate := &endpointGate{
		Launcher: h.ctrl.launcher,
		hold:     loading,
		entered:  make(chan string, 1),
		release:  make(chan struct{}),
	}
	h.ctrl.launcher = gate
	h.ctrl.cfg.Controller.CallTimeout = config.Duration(60 * time.Second)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	t.Cleanup(release)

	if outcome := h.handle(h.addedEvent("container-1", loading, "lease-1", h.clock.Now())); outcome.Result != domain.ResultApplied {
		t.Fatalf("offer: %s %s", outcome.Result, outcome.Reason)
	}
	action := planner.Action{Kind: planner.ActionAssign, InstanceID: "container-1", Role: domain.RoleRegular}

	ctx := context.Background()
	h.ctrl.applyStarts(ctx, []planner.Action{action})
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first dispatch never reached the container")
	}
	if h.ctrl.reserveStart("container-1") {
		t.Fatal("an instance with a start in flight accepted a second reservation")
	}

	// The same assignment again, while the first start is parked in the gate.
	h.ctrl.applyStarts(ctx, []planner.Action{action})
	time.Sleep(200 * time.Millisecond)
	if got := gate.attemptsFor(loading); got != 1 {
		t.Fatalf("the container was handed %d starts, want 1", got)
	}

	release()
	h.ctrl.AwaitStarts()
	if instance := h.instance("container-1"); instance.ServiceState != domain.ServiceServing {
		t.Fatalf("the start did not finish: %s (%s)", instance.ServiceState, instance.LastError)
	}
	if !h.ctrl.reserveStart("container-1") {
		t.Fatal("the reservation outlived the start it belonged to")
	}
	h.ctrl.releaseStart("container-1")
}
