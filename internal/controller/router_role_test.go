package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/routeradapter"
)

// A membership that is registered but never reports the requested role used to
// spend the whole call budget on a 100ms poll and then fail with the transport
// deadline it happened to trip over - "Get http://.../workers/<id>: context
// deadline exceeded", which names an endpoint and hides the cause. Both halves
// are pinned here: the wait must back off, and the error must name the role and
// what the Router kept reporting instead.
func TestMembershipWaitBacksOffAndNamesTheRoleItNeverSaw(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("node", h.bootstrap.Endpoint(), "lease", h.clock.Now()))
	// The Router accepts the registration and then keeps answering with the
	// other role, which is what a replacement it refuses to apply looks like.
	h.router.typeReportedByGet = string(domain.RoleDecode)

	before := h.router.getsByID
	started := time.Now()
	err := h.ctrl.StartService(t.Context(), "node", domain.RolePrefill)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a membership that never acquires the role cannot start")
	}
	message := err.Error()
	if !strings.Contains(message, "did not acquire role prefill") ||
		!strings.Contains(message, `lists it as "decode"`) {
		t.Fatalf("error must name the role and what the Router reported: %s", message)
	}
	if elapsed < 2*time.Second {
		t.Fatalf("gave up after %s, before the call budget expired", elapsed)
	}
	// CallTimeout is 3s in the harness; a fixed 100ms cadence reads thirty times.
	if polls := h.router.getsByID - before; polls > 10 {
		t.Fatalf("polled %d times; the cadence must back off", polls)
	}
}

func TestRoleRecoveryClosesIdleStaleMembershipBeforeReplacement(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("node", h.bootstrap.Endpoint(), "lease", h.clock.Now()))
	old, err := h.ctrl.router.RegisterWorker(t.Context(), routeradapter.RegisterRequest{WorkerURL: h.bootstrap.Endpoint(), WorkerType: domain.RoleDecode, ModelID: testModelID})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.ctrl.StartService(t.Context(), "node", domain.RolePrefill); err != nil {
		t.Fatal(err)
	}
	worker, found := h.router.workerByURL(h.bootstrap.Endpoint())
	if !found || worker.WorkerType != "prefill" || worker.ID == old.WorkerID || h.router.deletes() != 1 {
		t.Fatalf("wrong membership: %+v", worker)
	}
	if h.instance("node").ServiceState != domain.ServiceServing {
		t.Fatal("not serving after repair")
	}
	// Normal reconcile reuses the verified membership, without deleting/readding.
	before := h.router.registerCalls
	if _, err = h.ctrl.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if h.router.registerCalls != before || h.router.deletes() != 1 {
		t.Fatal("healthy membership churned")
	}
}
func TestRoleRecoveryRefusesBusyOrUnknownStaleMembership(t *testing.T) {
	for _, load := range []int64{1, -1} {
		t.Run(string(rune(load+65)), func(t *testing.T) {
			h := newHarness(t)
			h.handle(h.addedEvent("node", h.bootstrap.Endpoint(), "lease", h.clock.Now()))
			old, err := h.ctrl.router.RegisterWorker(t.Context(), routeradapter.RegisterRequest{WorkerURL: h.bootstrap.Endpoint(), WorkerType: domain.RoleDecode, ModelID: testModelID})
			if err != nil {
				t.Fatal(err)
			}
			h.router.setLoad(h.bootstrap.Endpoint(), load)
			if err = h.ctrl.StartService(t.Context(), "node", domain.RolePrefill); err == nil {
				t.Fatal("busy mismatch marked serving")
			}
			if h.router.deletes() != 0 || h.router.readinessOf(old.WorkerID) != "unavailable" {
				t.Fatal("must close readiness and wait for known zero load")
			}
			before, _, _ := h.bootstrap.counts()
			h.router.setLoad(h.bootstrap.Endpoint(), 0)
			if _, err = h.ctrl.ReconcileOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			after, _, _ := h.bootstrap.counts()
			if h.instance("node").ServiceState != domain.ServiceServing || before != after {
				t.Fatal("must repair running process without duplicate start")
			}
		})
	}
}

// A Router drops a membership through its own asynchronous workflow: the
// DELETE is accepted at once and the entry disappears seconds later. The
// control plane used to wait with call_timeout, so the only thing it could
// ever observe was its own deadline - the delete was accepted, the entry
// stayed listed, and every round reproduced the identical failure while the
// instance stayed unregistered. The wait is bounded by router.removal_timeout
// instead, which covers the workflow's budget.
func TestRoleRecoveryOutlastsTheRoutersAsynchronousRemoval(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("node", h.bootstrap.Endpoint(), "lease", h.clock.Now()))
	if _, err := h.ctrl.router.RegisterWorker(t.Context(), routeradapter.RegisterRequest{
		WorkerURL: h.bootstrap.Endpoint(), WorkerType: domain.RoleDecode, ModelID: testModelID,
	}); err != nil {
		t.Fatal(err)
	}
	// Longer than the harness call timeout (3s), well under removal_timeout.
	h.router.removeDelay = 5 * time.Second

	started := time.Now()
	if err := h.ctrl.StartService(t.Context(), "node", domain.RolePrefill); err != nil {
		t.Fatalf("role recovery must outlast call_timeout: %v", err)
	}
	if elapsed := time.Since(started); elapsed < h.router.removeDelay {
		t.Fatalf("returned after %s; the wait must cover the Router's workflow "+
			"rather than stop at call_timeout", elapsed)
	}
	worker, found := h.router.workerByURL(h.bootstrap.Endpoint())
	if !found || worker.WorkerType != "prefill" {
		t.Fatalf("wrong membership after the wait: %+v", worker)
	}
}
