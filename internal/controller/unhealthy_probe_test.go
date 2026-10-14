package controller

import (
	"testing"

	"github.com/tai-core/tai-talea/internal/domain"
)

// The container bootstrap answers "degraded" - HTTP 200 with status
// "degraded" - whenever a single /health_generate probe misses, which happens
// during warm-up and under a burst of load. Healthy() only accepts "ok", so
// treating the first miss as proof of a broken model ends a service that was
// serving traffic, and the next attempt reloads every GPU on the node. A miss
// must only take the instance out of rotation; the model is condemned only
// after the failure persists past health_failure_threshold.
func TestOneMissedProbeOnlyLeavesTheRotation(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("node", h.bootstrap.Endpoint(), "lease", h.clock.Now()))
	if err := h.ctrl.StartService(t.Context(), "node", domain.RolePrefill); err != nil {
		t.Fatal(err)
	}
	if h.instance("node").ServiceState != domain.ServiceServing {
		t.Fatal("not serving before the probe starts missing")
	}
	workerID := h.instance("node").RouterWorkerID

	h.bootstrap.setHealthStatus("degraded")
	report, err := h.ctrl.ReconcileOnce(t.Context())
	if err != nil {
		t.Fatalf("a single missed probe must not fail the round: %v", err)
	}
	if len(report.Unhealthy) != 1 || report.Unhealthy[0] != "node" {
		t.Fatalf("a missed probe must be reported as unhealthy: %+v", report)
	}
	if state := h.instance("node").ServiceState; state != domain.ServiceServing {
		t.Fatalf("one missed probe ended the service: %s", state)
	}
	if h.router.readinessOf(workerID) != "unavailable" {
		t.Fatal("an instance that misses its probe must leave the rotation")
	}

	// A probe that answers again clears the streak, so the next miss counts
	// from one rather than inheriting the old total.
	h.bootstrap.setHealthStatus("")
	if _, err = h.ctrl.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.ctrl.healthFailures.Load("node"); ok {
		t.Fatal("a recovered probe must clear the failure streak")
	}
}

func TestAHealthFailureThatPersistsCondemnsTheModel(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("node", h.bootstrap.Endpoint(), "lease", h.clock.Now()))
	if err := h.ctrl.StartService(t.Context(), "node", domain.RolePrefill); err != nil {
		t.Fatal(err)
	}

	threshold := h.ctrl.HealthFailureThreshold()
	if threshold < 2 {
		t.Fatalf("this test needs a threshold above one, got %d", threshold)
	}

	h.bootstrap.setHealthStatus("degraded")
	for round := 1; round <= threshold; round++ {
		if _, err := h.ctrl.ReconcileOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if round < threshold && h.instance("node").ServiceState != domain.ServiceServing {
			t.Fatalf("round %d condemned the model before the threshold", round)
		}
	}
	if state := h.instance("node").ServiceState; state != domain.ServiceFailed {
		t.Fatalf("expected FAILED after %d missed probes, got %s", threshold, state)
	}
}
