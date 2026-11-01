package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/launcher"
	"github.com/tai-core/tai-talea/internal/planner"
)

// gateLauncher holds every Start call until the test opens the gate and records
// how many were in flight together. StartService blocks for the whole model
// load, so this is the only way to observe the shape of a cold start without
// waiting minutes for real weights.
type gateLauncher struct {
	Launcher
	mu       sync.Mutex
	entered  chan string
	release  chan struct{}
	inFlight int
	peak     int
}

func (l *gateLauncher) Start(ctx context.Context, endpoint string, request launcher.StartRequest) error {
	l.mu.Lock()
	l.inFlight++
	if l.inFlight > l.peak {
		l.peak = l.inFlight
	}
	l.mu.Unlock()

	select {
	case l.entered <- endpoint:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-l.release:
	case <-ctx.Done():
		return ctx.Err()
	}

	l.mu.Lock()
	l.inFlight--
	l.mu.Unlock()
	return l.Launcher.Start(ctx, endpoint, request)
}

func (l *gateLauncher) peakInFlight() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.peak
}

// threeIdle gives a harness three idle instances. The rows come from capacity
// events, not from the harness defaults: an instance exists in the store only
// after the partner offered it, and the store refuses two active instances on
// one endpoint, so each one gets its own address.
//
// The addresses are not reachable, which is deliberate. The gate intercepts the
// start call before it becomes a request, so nothing here depends on a working
// bootstrap - and a start that does reach the wire fails immediately instead of
// waiting out a health timeout.
func threeIdle(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	for _, offer := range []struct{ id, lease, endpoint string }{
		{"container-1", "lease-1", "http://10.0.0.1:8080"},
		{"container-2", "lease-2", "http://10.0.0.2:8080"},
		{"container-3", "lease-3", "http://10.0.0.3:8080"},
	} {
		outcome := h.handle(h.addedEvent(offer.id, offer.endpoint, offer.lease, h.clock.Now()))
		if outcome.Result != domain.ResultApplied {
			t.Fatalf("offer of %s was %s: %s", offer.id, outcome.Result, outcome.Reason)
		}
	}
	return h
}

func assignAll(ids ...string) []planner.Action {
	actions := make([]planner.Action, 0, len(ids))
	for _, id := range ids {
		actions = append(actions, planner.Action{Kind: planner.ActionAssign, InstanceID: id, Role: domain.RoleRegular})
	}
	return actions
}

// A cold pool has to load its workers together. Applied one after another, four
// workers at 3 min 40 s each is 15 minutes before the deployment is whole, and
// every round in between reports a pool that is only a quarter up.
func TestAssignsLoadAtTheSameTime(t *testing.T) {
	h := threeIdle(t)
	gate := &gateLauncher{
		Launcher: h.ctrl.launcher,
		entered:  make(chan string, 3),
		release:  make(chan struct{}),
	}
	h.ctrl.launcher = gate

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ctrl.applyStarts(context.Background(), assignAll("container-1", "container-2", "container-3"))
	}()

	for index := 0; index < 3; index++ {
		select {
		case <-gate.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 3 starts entered; they are still being applied one at a time", index)
		}
	}
	close(gate.release)
	<-done

	if peak := gate.peakInFlight(); peak != 3 {
		t.Fatalf("peak concurrent starts = %d, want 3", peak)
	}
}

// The limit is what keeps a large pool from loading every worker at once: the
// workers of one machine share its page cache and host memory bandwidth, so
// more parallelism there makes each load slower without making the pool ready
// sooner.
func TestStartParallelismIsBounded(t *testing.T) {
	h := threeIdle(t)
	h.ctrl.cfg.Controller.MaxConcurrentStarts = 2
	gate := &gateLauncher{
		Launcher: h.ctrl.launcher,
		entered:  make(chan string, 3),
		release:  make(chan struct{}),
	}
	h.ctrl.launcher = gate

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ctrl.applyStarts(context.Background(), assignAll("container-1", "container-2", "container-3"))
	}()

	for index := 0; index < 2; index++ {
		select {
		case <-gate.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 2 starts entered with a limit of 2", index)
		}
	}
	select {
	case endpoint := <-gate.entered:
		t.Fatalf("a third start entered while the limit is 2: %s", endpoint)
	case <-time.After(300 * time.Millisecond):
	}

	// Drain one slot and the third must take it.
	gate.release <- struct{}{}
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the third start never entered after a slot was freed")
	}
	close(gate.release)
	<-done

	if peak := gate.peakInFlight(); peak > 2 {
		t.Fatalf("peak concurrent starts = %d, above the limit of 2", peak)
	}
}
