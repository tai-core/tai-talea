package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/config"
	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/launcher"
	"github.com/tai-core/tai-talea/internal/partner"
	"github.com/tai-core/tai-talea/internal/routeradapter"
	"github.com/tai-core/tai-talea/internal/store"
)

func TestLostServingWorkerClosesReadinessAndRecoversLegally(t *testing.T) {
	h := servingHarness(t)
	id := "container-1"
	workerID := h.instance(id).RouterWorkerID
	h.bootstrap.SetDown(true)
	h.reconcile()
	if h.instance(id).InstanceState != domain.InstanceLost {
		t.Fatal("unreachable container must be LOST")
	}
	if h.router.readinessOf(workerID) != "unavailable" {
		t.Fatal("LOST worker is still routable")
	}
	h.bootstrap.SetDown(false)
	h.reconcile()
	if h.instance(id).InstanceState != domain.InstanceIdle {
		t.Fatalf("recovered service could not pass preparation: %+v", h.instance(id))
	}
	h.reconcile()
	if h.instance(id).ServiceState != domain.ServiceServing {
		t.Fatalf("healthy recovered process was not adopted: %+v", h.instance(id))
	}
}

type failingReleaseAdapter struct {
	partner.PartnerAdapter
	calls int
}

func (a *failingReleaseAdapter) ReleaseInstance(context.Context, string) error {
	a.calls++
	return errors.New("partner release temporarily unavailable")
}

func TestReleaseRetriesRespectBackoffAcrossReconciliation(t *testing.T) {
	h := servingHarness(t)
	h.ctrl.cfg.Controller.OperationRetry = config.Duration(time.Minute)
	adapter := &failingReleaseAdapter{PartnerAdapter: h.adapter}
	h.ctrl.partners = &staticRegistry{adapter: adapter}
	if err := h.ctrl.RequestReclaim(t.Context(), "container-1", "operator", time.Minute); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	h.reconcile()
	if adapter.calls != 1 {
		t.Fatalf("release retried before delay: calls=%d", adapter.calls)
	}
	h.clock.Advance(time.Minute)
	h.reconcile()
	if adapter.calls != 2 {
		t.Fatalf("release did not retry after delay: calls=%d", adapter.calls)
	}
}

func TestStartRetryKeepsLatestBackoffAfterMultipleFailures(t *testing.T) {
	h := newHarness(t)
	h.ctrl.cfg.Controller.OperationRetry = config.Duration(time.Minute)
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
	h.bootstrap.mu.Lock()
	h.bootstrap.failStart = true
	h.bootstrap.mu.Unlock()
	_ = h.ctrl.StartService(t.Context(), "container-1", domain.RolePrefill)
	h.clock.Advance(time.Minute)
	h.reconcile()
	h.reconcile()
	starts, _, _ := h.bootstrap.counts()
	if starts != 2 {
		t.Fatalf("older retry bypassed newest backoff: starts=%d", starts)
	}
}

type failedStopResult struct{ Launcher }

func (l failedStopResult) Stop(context.Context, string, launcher.StopRequest) (launcher.StopResult, error) {
	return launcher.StopResult{Phase: launcher.PhaseFailed, Detail: "process remains alive"}, nil
}

func TestReclaimRequiresConfirmedProcessExit(t *testing.T) {
	h := servingHarness(t)
	h.ctrl.launcher = failedStopResult{h.ctrl.launcher}
	if err := h.ctrl.RequestReclaim(t.Context(), "container-1", "operator", time.Minute); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	node := h.instance("container-1")
	if node.InstanceState == domain.InstanceReleased || !node.PendingRelease || node.ServiceState != domain.ServiceDraining {
		t.Fatalf("live process released after unsuccessful stop: %+v", node)
	}
}

func TestReclaimClosesMembershipWhoseIDWasNotPersisted(t *testing.T) {
	h := servingHarness(t)
	node := h.instance("container-1")
	workerID := node.RouterWorkerID
	node.RouterWorkerID = ""
	if err := h.store.ApplyTransition(t.Context(), store.Transition{InstanceID: node.ID, Next: node}); err != nil {
		t.Fatal(err)
	}
	h.router.setLoad(node.ServiceURL(), 2)
	if err := h.ctrl.RequestReclaim(t.Context(), node.ID, "operator", time.Minute); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if h.router.readinessOf(workerID) != "unavailable" {
		t.Fatal("unpersisted router membership still accepts new requests")
	}
	if _, stops, _ := h.bootstrap.counts(); stops != 0 {
		t.Fatal("unpersisted membership's outstanding requests were ignored")
	}
}

type unroutableMembership struct {
	RouterAdapter
	healthy  bool
	draining bool
}

func (r unroutableMembership) GetWorker(ctx context.Context, id string) (routeradapter.Worker, error) {
	worker, err := r.RouterAdapter.GetWorker(ctx, id)
	worker.Healthy = r.healthy
	if r.draining {
		worker.Metadata = map[string]string{"__pd_state": "draining"}
	}
	return worker, err
}

func TestRouterGatesPreventFalseServingDuringStartAndReconcile(t *testing.T) {
	for _, gate := range []string{"unhealthy", "pd_draining"} {
		t.Run(gate, func(t *testing.T) {
			h := newHarness(t)
			h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
			h.ctrl.router = unroutableMembership{RouterAdapter: h.ctrl.router, healthy: gate != "unhealthy", draining: gate == "pd_draining"}
			if err := h.ctrl.StartService(t.Context(), "container-1", domain.RolePrefill); err == nil {
				t.Fatal("unroutable worker accepted as serving")
			}
			h.reconcile()
			if h.instance("container-1").ServiceState == domain.ServiceServing {
				t.Fatal("reconcile accepted an unroutable worker")
			}
		})
	}
}

func TestHealthyStateResumesThroughRegistrationAfterRestart(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
	for _, state := range []domain.ServiceState{domain.ServiceStarting, domain.ServiceHealthy} {
		node := h.instance("container-1")
		node.Role, node.ServiceState = domain.RolePrefill, state
		if err := h.store.ApplyTransition(t.Context(), store.Transition{InstanceID: node.ID, Next: node}); err != nil {
			t.Fatal(err)
		}
	}
	h.bootstrap.mu.Lock()
	h.bootstrap.phase, h.bootstrap.role = launcher.PhaseRunning, string(domain.RolePrefill)
	h.bootstrap.mu.Unlock()
	h.reconcile()
	if h.instance("container-1").ServiceState != domain.ServiceServing {
		t.Fatalf("HEALTHY startup was not resumed: %+v", h.instance("container-1"))
	}
}

func TestIdentityUpdateDrainsOldEndpointAndSurvivesRestart(t *testing.T) {
	h := servingHarness(t)
	old := h.instance("container-1")
	h.router.setLoad(old.ServiceURL(), 2)
	h.clock.Advance(time.Second)
	event := domain.CapacityEvent{EventID: "identity-update", PartnerID: old.PartnerID,
		Type: domain.EventCapacityUpdated, OccurredAt: h.clock.Now(),
		Instance: &domain.EventInstance{ID: old.ID, ServiceEndpoint: "http://replacement:9002", LeaseID: "lease-2"}}
	h.handle(event)
	pending := h.instance(old.ID)
	if pending.PendingUpdate == nil || pending.ServiceURL() != old.ServiceURL() || pending.LeaseID != old.LeaseID {
		t.Fatalf("running identity was replaced before drain: %+v", pending)
	}
	h.ctrl = buildController(t, h.cfg, h.store, h.router, h.bootstrap, h.adapter, h.metrics, h.recorder, h.clock)
	if _, _, err := h.ctrl.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, stops, _ := h.bootstrap.counts(); stops != 0 {
		t.Fatal("recovery stopped old worker while still busy")
	}
	h.router.setLoad(old.ServiceURL(), 0)
	h.reconcile()
	next := h.instance(old.ID)
	if next.PendingUpdate != nil || next.ServiceURL() != "http://replacement:9002" || next.LeaseID != "lease-2" || next.InstanceState != domain.InstancePreparing {
		t.Fatalf("new identity was not applied after stop: %+v", next)
	}
	h.reconcile()
	if err := h.ctrl.StartService(t.Context(), old.ID, domain.RolePrefill); err != nil {
		t.Fatal(err)
	}
	if h.router.readinessOf(old.RouterWorkerID) != "unavailable" {
		t.Fatal("old endpoint became routable again")
	}
	if _, found := h.router.workerByURL("http://replacement:9002"); !found {
		t.Fatal("replacement endpoint was not registered")
	}
}

func TestMetadataHeartbeatRefreshesLeaseAndRejectsOlderUpdate(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
	old := h.instance("container-1")
	h.clock.Advance(time.Minute)
	heartbeat := domain.CapacityEvent{EventID: "heartbeat", PartnerID: old.PartnerID,
		Type: domain.EventCapacityUpdated, OccurredAt: h.clock.Now(), Instance: &domain.EventInstance{ID: old.ID}}
	h.handle(heartbeat)
	if !h.instance(old.ID).LeaseUpdatedAt.Equal(heartbeat.OccurredAt) {
		t.Fatal("unchanged metadata did not refresh lease observation")
	}
	stale := heartbeat
	stale.EventID, stale.OccurredAt = "stale-metadata", old.LeaseUpdatedAt
	stale.Instance = &domain.EventInstance{ID: old.ID, Endpoint: "http://stale:9001"}
	outcome := h.handle(stale)
	if outcome.Result != domain.ResultRejected || h.instance(old.ID).Endpoint != old.Endpoint {
		t.Fatalf("stale update overwrote current endpoint: %+v", outcome)
	}
}

func TestReconcileRestartsAnExternallyStoppedProcess(t *testing.T) {
	h := servingHarness(t)
	h.bootstrap.mu.Lock()
	h.bootstrap.phase = launcher.PhaseStopped
	h.bootstrap.servicePID = 0
	h.bootstrap.mu.Unlock()
	h.reconcile()
	node := h.instance("container-1")
	if node.ServiceState != domain.ServiceFailed || h.router.readinessOf(node.RouterWorkerID) != "unavailable" {
		t.Fatalf("stopped process still advertised: %+v", node)
	}
	h.reconcile()
	if h.instance(node.ID).ServiceState != domain.ServiceServing {
		t.Fatalf("stopped process did not restart: %+v", h.instance(node.ID))
	}
}

func TestReconcileRejectsRuntimeRoleDriftAndDrainsIt(t *testing.T) {
	h := servingHarness(t)
	h.bootstrap.mu.Lock()
	h.bootstrap.role = string(domain.RoleDecode)
	h.bootstrap.mu.Unlock()
	h.reconcile()
	node := h.instance("container-1")
	if node.ServiceState != domain.ServiceDraining || h.router.readinessOf(node.RouterWorkerID) != "unavailable" {
		t.Fatalf("runtime role drift was not isolated: %+v", node)
	}
}

type alreadyRunningLauncher struct{ Launcher }

func (l alreadyRunningLauncher) Start(context.Context, string, launcher.StartRequest) error {
	return launcher.ErrAlreadyRunning
}

func TestStartDoesNotAdoptAnAlreadyRunningWrongRole(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
	h.bootstrap.mu.Lock()
	h.bootstrap.phase = launcher.PhaseRunning
	h.bootstrap.role = string(domain.RoleDecode)
	h.bootstrap.servicePID = 4242
	h.bootstrap.mu.Unlock()
	h.ctrl.launcher = alreadyRunningLauncher{h.ctrl.launcher}
	if err := h.ctrl.StartService(t.Context(), "container-1", domain.RolePrefill); err == nil {
		t.Fatal("wrong role process was adopted")
	}
	if h.router.workerCount() != 0 {
		t.Fatal("wrong role process was registered")
	}
}

func TestReconcileDoesNotInterruptAnOwnedStart(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
	node := h.instance("container-1")
	node.ServiceState, node.Role = domain.ServiceStarting, domain.RolePrefill
	if err := h.store.ApplyTransition(t.Context(), store.Transition{InstanceID: node.ID, Next: node}); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Minute)
	lock := h.ctrl.instanceOperation(node.ID)
	lock.Lock()
	defer lock.Unlock()
	h.reconcile()
	if h.instance(node.ID).ServiceState != domain.ServiceStarting {
		t.Fatal("reconcile overwrote a running start operation")
	}
}

func TestPlannerReassignmentHonorsDrainGrace(t *testing.T) {
	h := servingHarness(t)
	h.router.setLoad(h.bootstrap.Endpoint(), 3)
	// A single P worker needs conversion to the configured one-worker D target.
	decision, err := h.ctrl.Rebalance(t.Context())
	if err != nil || len(decision.Actions) != 1 {
		t.Fatalf("expected role change: %+v %v", decision, err)
	}
	h.reconcile()
	if _, stops, _ := h.bootstrap.counts(); stops != 0 {
		t.Fatal("planner immediately killed outstanding requests")
	}
}
