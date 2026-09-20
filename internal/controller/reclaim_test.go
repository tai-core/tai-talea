package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/config"
	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/launcher"
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
