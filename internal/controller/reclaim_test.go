package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/config"
	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/launcher"
	"github.com/tai-core/tai-talea/internal/obs"
	"github.com/tai-core/tai-talea/internal/partner"
	"github.com/tai-core/tai-talea/internal/routeradapter"
	"github.com/tai-core/tai-talea/internal/store"
)

type stopRecorder struct {
	Launcher
	fail   bool
	forces []bool
}

func (l *stopRecorder) Stop(ctx context.Context, endpoint string, request launcher.StopRequest) (launcher.StopResult, error) {
	l.forces = append(l.forces, request.Force)
	if l.fail {
		return launcher.StopResult{}, errors.New("stop temporarily unavailable")
	}
	return l.Launcher.Stop(ctx, endpoint, request)
}

type unavailableLoad struct{ RouterAdapter }

func (r unavailableLoad) GetLoads(context.Context) ([]routeradapter.WorkerLoad, error) {
	return nil, errors.New("load endpoint unavailable")
}

func servingHarness(t *testing.T) *harness {
	h := newHarness(t)
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
	if err := h.ctrl.StartService(context.Background(), "container-1", domain.RolePrefill); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestReclaimSurvivesRestartAndStopFailureWithoutRestartingService(t *testing.T) {
	h := servingHarness(t)
	ctx := context.Background()
	stale := h.instance("container-1")
	for i := 0; i < 2; i++ {
		if err := h.ctrl.RequestReclaim(ctx, stale.ID, "operator", time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if !h.instance(stale.ID).PendingRelease {
		t.Fatal("intent must be durable before any remote calls")
	}
	if _, stops, _ := h.bootstrap.counts(); stops != 0 {
		t.Fatal("request handler must not call stop")
	}
	if err := h.store.ApplyTransition(ctx, store.Transition{InstanceID: stale.ID, Next: stale}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale serving write must not erase reclaim intent: %v", err)
	}
	entries, _ := h.store.ListAudit(ctx, stale.ID, 100)
	count := 0
	for _, entry := range entries {
		if entry.Action == "instance_reclaim_requested" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("duplicate request produced %d intent audit entries", count)
	}
	h.cfg.Controller.OperationRetry = config.Duration(30 * time.Second)
	h.ctrl = buildController(t, h.cfg, h.store, h.router, h.bootstrap, h.adapter, h.metrics, h.recorder, h.clock)
	stop := &stopRecorder{Launcher: h.ctrl.launcher, fail: true}
	h.ctrl.launcher = stop
	if err := h.ctrl.StartService(ctx, stale.ID, domain.RolePrefill); err == nil {
		t.Fatal("reclaimed node must refuse start")
	}
	if _, _, err := h.ctrl.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	failed := h.instance(stale.ID)
	if failed.ServiceState != domain.ServiceDraining || !failed.PendingRelease || failed.LastError == "" {
		t.Fatalf("failed stop lost reclaim intent: %+v", failed)
	}
	if _, err := h.ctrl.Rebalance(ctx); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if len(stop.forces) != 1 {
		t.Fatal("must honor stop retry backoff")
	}
	stop.fail = false
	h.clock.Advance(31 * time.Second)
	h.reconcile()
	finished := h.instance(stale.ID)
	if finished.InstanceState != domain.InstanceReleased || finished.ServiceState != domain.ServiceNone || finished.PendingRelease {
		t.Fatalf("reclaim did not finish: %+v", finished)
	}
	if starts, _, _ := h.bootstrap.counts(); starts != 1 {
		t.Fatalf("reclaim restarted service %d times", starts)
	}
	if h.router.readinessOf(stale.RouterWorkerID) != "unavailable" {
		t.Fatal("reclaimed worker became routable")
	}
}

func TestReclaimWaitsForUnknownLoadUntilDeadline(t *testing.T) {
	for _, mode := range []string{"missing", "negative", "error"} {
		t.Run(mode, func(t *testing.T) {
			h := servingHarness(t)
			switch mode {
			case "missing":
				h.router.Reset()
			case "negative":
				h.router.setLoad(h.bootstrap.Endpoint(), -1)
			case "error":
				h.ctrl.router = unavailableLoad{h.ctrl.router}
			}
			stop := &stopRecorder{Launcher: h.ctrl.launcher}
			h.ctrl.launcher = stop
			if err := h.ctrl.RequestReclaim(context.Background(), "container-1", "operator", time.Minute); err != nil {
				t.Fatal(err)
			}
			h.reconcile()
			if len(stop.forces) != 0 {
				t.Fatal("unknown load must not be treated as zero")
			}
			h.clock.Advance(time.Minute)
			h.reconcile()
			if len(stop.forces) != 1 || !stop.forces[0] {
				t.Fatalf("deadline must force stop: %v", stop.forces)
			}
			if h.instance("container-1").InstanceState != domain.InstanceReleased {
				t.Fatal("node was not released")
			}
		})
	}
}

type refusedReadiness struct {
	RouterAdapter
	fail bool
}

func (r *refusedReadiness) SetReadiness(ctx context.Context, id string, ready bool) (routeradapter.ReadinessTransition, error) {
	if r.fail {
		return routeradapter.ReadinessTransition{}, errors.New("readiness temporarily unavailable")
	}
	return r.RouterAdapter.SetReadiness(ctx, id, ready)
}

func TestReclaimRetriesReadinessFailure(t *testing.T) {
	h := servingHarness(t)
	router := &refusedReadiness{RouterAdapter: h.ctrl.router, fail: true}
	h.ctrl.router = router
	if err := h.ctrl.RequestReclaim(context.Background(), "container-1", "operator", time.Minute); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if _, stops, _ := h.bootstrap.counts(); stops != 0 {
		t.Fatal("cannot stop before readiness is closed")
	}
	if !h.instance("container-1").PendingRelease {
		t.Fatal("readiness failure lost intent")
	}
	if _, err := h.ctrl.Rebalance(context.Background()); err != nil {
		t.Fatal(err)
	}
	router.fail = false
	h.reconcile()
	if h.instance("container-1").InstanceState != domain.InstanceReleased {
		t.Fatal("reclaim did not recover")
	}
}

type delayedStart struct {
	Launcher
	entered chan struct{}
	resume  chan struct{}
}

func (l *delayedStart) Start(ctx context.Context, endpoint string, request launcher.StartRequest) error {
	close(l.entered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.resume:
	}
	return l.Launcher.Start(ctx, endpoint, request)
}

func TestReclaimDuringStartCannotReleaseBeforeTheStartCallReturns(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
	l := &delayedStart{Launcher: h.ctrl.launcher, entered: make(chan struct{}), resume: make(chan struct{})}
	h.ctrl.launcher = l
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.ctrl.StartService(ctx, "container-1", domain.RolePrefill) }()
	select {
	case <-l.entered:
	case <-ctx.Done():
		t.Fatal("start did not enter")
	}
	if err := h.ctrl.RequestReclaim(ctx, "container-1", "operator", time.Minute); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if _, stops, _ := h.bootstrap.counts(); stops != 0 {
		t.Fatal("stop raced with the start call")
	}
	close(l.resume)
	if err := <-done; err == nil {
		t.Fatal("start must observe the reclaim request")
	}
	h.clock.Advance(time.Minute)
	h.reconcile()
	if h.instance("container-1").InstanceState != domain.InstanceReleased {
		t.Fatal("node not released after start settled")
	}
	starts, stops, _ := h.bootstrap.counts()
	if starts != 1 || stops != 1 {
		t.Fatalf("unexpected process lifecycle: starts=%d stops=%d", starts, stops)
	}
}

// A container that stopped answering can never satisfy /bootstrap/stop, so a
// reclaim that insisted on draining it first would retry the stop forever and
// keep the partner's quota pinned. Once its grace expired the drain completes,
// the service is recorded as stopped and the container goes back.
func TestReclaimOfAnUnreachableContainerReleasesWithoutStopping(t *testing.T) {
	h := servingHarness(t)
	ctx := t.Context()
	id := "container-1"

	h.bootstrap.SetDown(true)
	h.reconcile()
	if got := h.instance(id); got.InstanceState != domain.InstanceLost {
		t.Fatalf("an unreachable container must be LOST first: %+v", got)
	}
	if err := h.ctrl.RequestReclaim(ctx, id, "operator", time.Minute); err != nil {
		t.Fatal(err)
	}
	// Recording the intent is all the request handler does; the reconciler owns
	// the network calls, so nothing has been stopped yet.
	if h.instance(id).InstanceState == domain.InstanceReleased {
		t.Fatal("the request handler must not release on its own")
	}
	_, stopsBefore, _ := h.bootstrap.counts()

	h.clock.Advance(time.Second)
	h.reconcile()

	got := h.instance(id)
	if got.InstanceState != domain.InstanceReleased {
		t.Fatalf("unreachable container was not released: %+v (last_error=%q)", got, got.LastError)
	}
	if _, stopsAfter, _ := h.bootstrap.counts(); stopsAfter != stopsBefore {
		t.Fatalf("an unreachable container must not be asked to stop: %d -> %d", stopsBefore, stopsAfter)
	}
	if !h.adapter.Released(id) {
		t.Fatal("the container was not handed back to the partner")
	}
}

// absentReleaseAdapter answers every release with partner.ErrInstanceNotFound,
// which is what the platform says about a container it has already recycled.
type absentReleaseAdapter struct {
	partner.PartnerAdapter
	calls int
}

func (a *absentReleaseAdapter) ReleaseInstance(_ context.Context, instanceID string) error {
	a.calls++
	return fmt.Errorf("release %s: %w", instanceID, partner.ErrInstanceNotFound)
}

// A container can vanish from the platform before the control plane gives it
// back: the poll stops reporting it, the console deletes it, the platform
// recycles it. The release call then answers "I do not have that container",
// which is not a failure — it is the release postcondition, already true. A
// controller that retried it would keep the row in RELEASING and the partner's
// quota charged forever, so the release has to complete on the spot, once.
func TestReleaseOfAVanishedContainerCompletesWithoutRetrying(t *testing.T) {
	h := servingHarness(t)
	ctx := t.Context()
	id := "container-1"

	adapter := &absentReleaseAdapter{PartnerAdapter: h.adapter}
	h.ctrl = buildController(t, h.cfg, h.store, h.router, h.bootstrap, adapter, h.metrics, h.recorder, h.clock)

	if err := h.ctrl.RequestReclaim(ctx, id, "capacity revoked", time.Minute); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Minute)
	h.reconcile()
	// A second pass proves nothing is left queued for retry.
	h.reconcile()

	got := h.instance(id)
	if got.InstanceState != domain.InstanceReleased {
		t.Fatalf("a container the partner no longer has must still be released: %+v", got)
	}
	if got.PendingRelease || !got.DrainDeadlineAt.IsZero() {
		t.Fatalf("release intent survived the release: %+v", got)
	}
	if got.LastError != "" {
		t.Fatalf("a vanished container is not an error: %q", got.LastError)
	}
	if adapter.calls != 1 {
		t.Fatalf("the release call must not be retried, called %d times", adapter.calls)
	}
	if open, err := h.store.ListOpenOperations(ctx, 50); err != nil {
		t.Fatal(err)
	} else {
		for _, operation := range open {
			if operation.InstanceID == id {
				t.Fatalf("release operation left open: %+v", operation)
			}
		}
	}
	if h.recorder.has(obs.AlertOperationRetryExhausted) {
		t.Fatal("a vanished container must not raise a retry alert")
	}

	// The audit has to say which of the two releases happened: "the partner
	// took it back" or "the partner had already lost it".
	entries, err := h.store.ListAudit(ctx, id, 200)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, entry := range entries {
		actions = append(actions, entry.Action)
	}
	found := false
	for _, action := range actions {
		if action == "instance_release_absent_from_partner" {
			found = true
		}
		if action == "instance_released" {
			t.Fatalf("a vanished container must not be audited as a normal release: %v", actions)
		}
	}
	if !found {
		t.Fatalf("missing the absent-from-partner audit entry: %v", actions)
	}
}
