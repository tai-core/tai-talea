package controller

import (
	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/routeradapter"
	"testing"
)

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
