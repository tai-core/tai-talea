package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/config"
	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/launcher"
	"github.com/tai-core/tai-talea/internal/obs"
	"github.com/tai-core/tai-talea/internal/partner"
	"github.com/tai-core/tai-talea/internal/store"
)

// endpointGate sits where the container answers questions, which is the only
// place a test can say "this worker is still loading" and "that worker is dead"
// at the same time. Everything the control plane believes about a container
// arrives through this interface, so gating it gates the belief.
//
// The harness routes every bootstrap call to one fake, so a second fake would
// not be asked anything: an endpoint has to be made to misbehave here or not at
// all.
type endpointGate struct {
	Launcher
	hold string
	// failing is the endpoint that answers like a container that stopped
	// existing. It is a field rather than a constructor argument because the
	// test needs it to die in the middle of another worker's load.
	mu      sync.Mutex
	failing string
	passed  int
	// attempts counts every Start call per endpoint, including the ones parked
	// inside the gate. A round that dispatched the same container twice would
	// show up here even though the second call never got to run.
	attempts map[string]int

	entered chan string
	release chan struct{}
}

func (g *endpointGate) setFailing(endpoint string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failing = endpoint
}

func (g *endpointGate) isFailing(endpoint string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return endpoint != "" && endpoint == g.failing
}

func (g *endpointGate) Start(ctx context.Context, endpoint string, request launcher.StartRequest) error {
	g.mu.Lock()
	if g.attempts == nil {
		g.attempts = make(map[string]int)
	}
	g.attempts[endpoint]++
	g.mu.Unlock()
	if endpoint != g.hold || g.isFailing(endpoint) {
		return g.Launcher.Start(ctx, endpoint, request)
	}
	select {
	case g.entered <- endpoint:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-g.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	g.mu.Lock()
	g.passed++
	g.mu.Unlock()
	return g.Launcher.Start(ctx, endpoint, request)
}

// heldStartHasPassed reports whether the gated start ever left the gate. The
// assertion below is only meaningful while it has not.
func (g *endpointGate) heldStartHasPassed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.passed > 0
}

// attemptsFor counts how many times the control plane asked this container to
// start, including calls that are still parked inside the gate.
func (g *endpointGate) attemptsFor(endpoint string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.attempts[endpoint]
}

func (g *endpointGate) Health(ctx context.Context, endpoint string) (launcher.Health, error) {
	if g.isFailing(endpoint) {
		return launcher.Health{}, errors.New("dial tcp: connection refused")
	}
	return g.Launcher.Health(ctx, endpoint)
}

func (g *endpointGate) Status(ctx context.Context, endpoint string) (launcher.Status, error) {
	if g.isFailing(endpoint) {
		return launcher.Status{}, errors.New("dial tcp: connection refused")
	}
	return g.Launcher.Status(ctx, endpoint)
}

// observation is a CAPACITY_UPDATED that re-asserts an identity the control
// plane already has, which is what the pull path sends every poll.
func observation(t *testing.T, h *harness, id, endpoint, lease string, at time.Time, source domain.EventSource) domain.CapacityEvent {
	t.Helper()
	spec := h.instance(id).Spec
	return domain.CapacityEvent{
		EventID:    "partner-a:update:" + id,
		PartnerID:  "partner-a",
		Type:       domain.EventCapacityUpdated,
		OccurredAt: at,
		Instance:   &domain.EventInstance{ID: id, Endpoint: endpoint, LeaseID: lease, Spec: &spec},
		Source:     source,
		ReceivedAt: at,
	}
}

// A start holds an instance's operation lock for the whole model load. The
// observation that proves the container is still there used to be refused for
// that entire window, so the recorded lease stopped advancing while the partner
// kept reporting the same healthy container, and lease_expiring_soon fired on a
// worker that had done nothing wrong.
func TestObservationLandsWhileTheInstanceIsStarting(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if outcome := h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now())); outcome.Result != domain.ResultApplied {
		t.Fatalf("offer: %s %s", outcome.Result, outcome.Reason)
	}
	if err := h.ctrl.StartService(ctx, "container-1", domain.RoleRegular); err != nil {
		t.Fatalf("start: %v", err)
	}
	before := h.instance("container-1")

	// Hold the lock the way StartService does for a 3 minute load.
	lock := h.ctrl.instanceOperation("container-1")
	lock.Lock()
	defer lock.Unlock()

	// Well past the lease warning window: without the observation landing, the
	// reconciler has every reason to complain.
	h.clock.Advance(2 * h.cfg.Controller.LeaseWarning.Duration())
	observed := h.clock.Now()

	event := observation(t, h, "container-1", h.bootstrap.Endpoint(), "lease-1", observed, domain.SourcePull)
	outcome, err := h.ctrl.HandleCapacityEvent(ctx, event)
	if outcome.Result != domain.ResultApplied {
		t.Fatalf("observation was %s (%s, err=%v); a start in flight must not refuse the proof that the container is alive",
			outcome.Result, outcome.Reason, err)
	}
	if err != nil {
		t.Fatalf("observation: %v", err)
	}

	after := h.instance("container-1")
	if !after.LeaseUpdatedAt.Equal(observed) {
		t.Fatalf("lease observed at %s, want %s", after.LeaseUpdatedAt, observed)
	}
	if !after.LastSeenAt.Equal(observed) {
		t.Fatalf("last seen at %s, want %s", after.LastSeenAt, observed)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatal("the observation moved the row's update clock; that clock is what the stuck-STARTING heuristic measures from")
	}
	if after.ServiceState != before.ServiceState || after.InstanceState != before.InstanceState {
		t.Fatalf("the observation changed state: %s/%s -> %s/%s",
			before.InstanceState, before.ServiceState, after.InstanceState, after.ServiceState)
	}

	h.reconcile()
	if h.recorder.has(obs.AlertLeaseExpiringSoon) {
		t.Fatal("lease_expiring_soon fired for a worker the partner is reporting as alive")
	}
}

// The other half of that trade: an event that would change the identity of a
// running worker still has to wait for whatever owns it, because applying it
// means draining first.
func TestAnIdentityChangeStillWaitsForTheStartToFinish(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if outcome := h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now())); outcome.Result != domain.ResultApplied {
		t.Fatalf("offer: %s %s", outcome.Result, outcome.Reason)
	}
	if err := h.ctrl.StartService(ctx, "container-1", domain.RoleRegular); err != nil {
		t.Fatalf("start: %v", err)
	}

	lock := h.ctrl.instanceOperation("container-1")
	lock.Lock()
	defer lock.Unlock()

	event := observation(t, h, "container-1", "http://10.0.0.9:8080", "lease-1", h.clock.Now(), domain.SourcePush)
	outcome, err := h.ctrl.HandleCapacityEvent(ctx, event)
	if outcome.Result != domain.ResultPending {
		t.Fatalf("an endpoint change during a start was %s, want PENDING", outcome.Result)
	}
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("pending outcome should carry the conflict that keeps it durable, got %v", err)
	}
}

// The driver split exists for this: while one worker loads, another worker that
// dies still has to be marked LOST. Before, the round applied the planner's
// starts and then reconciled, so nothing was probed until the load finished and
// a dead container stayed routable for minutes.
func TestAWorkerThatDiesDuringALoadIsStillMarkedLost(t *testing.T) {
	h := newHarness(t)
	const (
		loading = "http://10.0.0.1:8080"
		dying   = "http://10.0.0.2:8080"
	)
	gate := &endpointGate{
		Launcher: h.ctrl.launcher,
		hold:     loading,
		entered:  make(chan string, 1),
		release:  make(chan struct{}),
	}
	h.ctrl.launcher = gate

	// The worker that will die, brought up normally first.
	if outcome := h.handle(h.addedEvent("container-2", dying, "lease-2", h.clock.Now())); outcome.Result != domain.ResultApplied {
		t.Fatalf("offer container-2: %s %s", outcome.Result, outcome.Reason)
	}
	if err := h.ctrl.StartService(context.Background(), "container-2", domain.RoleRegular); err != nil {
		t.Fatalf("start container-2: %v", err)
	}

	// The worker that will load, left idle so the planner picks it.
	if outcome := h.handle(h.addedEvent("container-1", loading, "lease-1", h.clock.Now())); outcome.Result != domain.ResultApplied {
		t.Fatalf("offer container-1: %s %s", outcome.Result, outcome.Reason)
	}

	// The gate holds the start on the call context, and in the harness that
	// context is the 3 second call timeout. Without this the "load" would be
	// released by a timeout after 3 seconds and the test would prove nothing
	// about a load outliving a reconcile round.
	h.ctrl.cfg.Controller.CallTimeout = config.Duration(60 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ctrl.RunPeriodic(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	released := false
	release := func() {
		if !released {
			released = true
			close(gate.release)
		}
	}
	defer release()

	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the planner never started the idle worker")
	}

	// The load is running and holds the instance lock. Now the other worker
	// stops answering.
	gate.setFailing(dying)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h.instance("container-2").InstanceState == domain.InstanceLost {
			// The load must still be in flight, or the assertion above proves
			// nothing: check that the gate never let it through.
			if gate.heldStartHasPassed() {
				t.Fatal("the load finished before the assertion; the window had already closed")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("container-2 is %s, not LOST: a load in flight still stopped the reconciler",
		h.instance("container-2").InstanceState)
}

// A pull round is a sweep, and a worker that is mid-operation must not stop it.
//
// The case that costs a card: one worker is starting, and another has already
// disappeared. The snapshot re-asserts the first, which cannot be applied while
// the start owns it, and the disappearance is only seen by the loop that runs
// after. Stopping at the first unappliable instance left the dead one registered
// with the Router for as long as the load lasted, so the Router kept sending it
// traffic.
func TestPullSweepDoesNotStopAtAnInstanceThatIsMidOperation(t *testing.T) {
	const (
		starting = "http://10.0.0.1:8080"
		gone     = "http://10.0.0.2:8080"
	)
	// The partner reports the first worker under a new lease, which is a change
	// and therefore needs the instance's lock; it no longer reports the second.
	h := newHarness(t, partner.CapacityInstance{ID: "starting", Endpoint: starting, LeaseID: "lease-9"})
	ctx := context.Background()

	if outcome := h.handle(h.addedEvent("starting", starting, "lease-1", h.clock.Now())); outcome.Result != domain.ResultApplied {
		t.Fatalf("offer starting: %s %s", outcome.Result, outcome.Reason)
	}
	if outcome := h.handle(h.addedEvent("gone", gone, "lease-2", h.clock.Now())); outcome.Result != domain.ResultApplied {
		t.Fatalf("offer gone: %s %s", outcome.Result, outcome.Reason)
	}

	// The start holds the lock for minutes; the pull must work around it.
	lock := h.ctrl.instanceOperation("starting")
	lock.Lock()
	defer lock.Unlock()

	puller := partner.NewPuller(h.store, h.metrics, nil, nil, 3)
	if err := h.ctrl.PullOnce(ctx, PullSchedule{Adapter: h.adapter, Puller: puller}); err != nil {
		t.Fatalf("pull round: %v", err)
	}

	// The first absent snapshot only starts the revocation grace: the
	// platform list can omit a live instance for minutes after a STOP/START,
	// and one miss must not fire the reclaim that deletes the container.
	if gone := h.instance("gone"); gone.InstanceState != domain.InstanceIdle && !gone.PendingRelease {
		t.Fatalf("the first miss already condemned the vanished worker: state %s/%s, pending_release=%v",
			gone.InstanceState, gone.ServiceState, gone.PendingRelease)
	}

	// Absence that outlasts the grace is a real revocation, and the sweep
	// still has to work around the held lock of the sibling worker.
	h.clock.Advance(h.ctrl.RevocationGrace() + time.Minute)
	if err := h.ctrl.PullOnce(ctx, PullSchedule{Adapter: h.adapter, Puller: puller}); err != nil {
		t.Fatalf("pull round after the grace: %v", err)
	}

	revoked := h.instance("gone")
	if revoked.InstanceState != domain.InstanceReleased && !revoked.PendingRelease {
		t.Fatalf("the vanished worker was not reclaimed: state %s/%s, pending_release=%v",
			revoked.InstanceState, revoked.ServiceState, revoked.PendingRelease)
	}
}
