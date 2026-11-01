package controller

import (
	"testing"

	"github.com/tai-core/tai-talea/internal/domain"
)

// The container bootstrap answers "degraded" - HTTP 200 with status
// "degraded" - whenever a single /health_generate probe misses, which happens
// during warm-up and under a burst of load. That probe is itself a generation,
// so it queues behind the batch it is measuring: a miss is what a *busy*
// instance looks like.
//
// Leaving the rotation on the first miss therefore removes instances in
// proportion to load. Measured on kunhaoyuan at 192 concurrent: 1,098 requests
// refused with no_available_workers in two windows, both of which sit inside a
// cluster of "model health probe failed" rounds - all four instances out of the
// rotation at once, so the Router had nothing routable at all. One miss must
// leave the instance in rotation; readiness closes at
// readiness_failure_threshold, and the model is condemned only after the
// failure persists past health_failure_threshold.
func TestOneMissedProbeKeepsTheInstanceInRotation(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("node", h.bootstrap.Endpoint(), "lease", h.clock.Now()))
	if err := h.ctrl.StartService(t.Context(), "node", domain.RolePrefill); err != nil {
		t.Fatal(err)
	}
	if h.instance("node").ServiceState != domain.ServiceServing {
		t.Fatal("not serving before the probe starts missing")
	}
	workerID := h.instance("node").RouterWorkerID
	if h.ctrl.ReadinessFailureThreshold() < 2 {
		t.Fatalf("this test needs a readiness threshold above one, got %d", h.ctrl.ReadinessFailureThreshold())
	}

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
	if got := h.router.readinessOf(workerID); got != "ready" {
		t.Fatalf("readiness=%s, want ready: one queued probe must not remove a busy instance from the rotation", got)
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

func TestConsecutiveMissesTakeTheInstanceOutOfRotation(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("node", h.bootstrap.Endpoint(), "lease", h.clock.Now()))
	if err := h.ctrl.StartService(t.Context(), "node", domain.RolePrefill); err != nil {
		t.Fatal(err)
	}
	workerID := h.instance("node").RouterWorkerID

	threshold := h.ctrl.ReadinessFailureThreshold()
	if threshold < 2 {
		t.Fatalf("this test needs a readiness threshold above one, got %d", threshold)
	}

	h.bootstrap.setHealthStatus("degraded")
	for round := 1; round <= threshold; round++ {
		if _, err := h.ctrl.ReconcileOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		want := "unavailable"
		if round < threshold {
			want = "ready"
		}
		if got := h.router.readinessOf(workerID); got != want {
			t.Fatalf("round %d: readiness=%s, want %s", round, got, want)
		}
	}

	// Removal from rotation is the cheap consequence; the model itself is only
	// condemned at health_failure_threshold, which is deliberately higher.
	if threshold < h.ctrl.HealthFailureThreshold() {
		if state := h.instance("node").ServiceState; state != domain.ServiceServing {
			t.Fatalf("service_state=%s after %d misses, want SERVING: removal is not condemnation", state, threshold)
		}
	}
}

// A miss while the instance has work in flight is the probe queueing behind that
// work, not the model failing. The thresholds alone cannot tell the difference,
// and counting these misses condemns healthy models: a load test at 192 in
// flight reloaded two instances that were serving normally, and the reload - not
// the load - is what the callers behind them saw as 503s and 50-second first
// tokens. The streak therefore only advances when the instance had nothing to
// do; the test below is the same loop with an idle worker, and it must still
// condemn.
func TestAProbeMissWhileBusyIsNotEvidenceAgainstTheModel(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("node", h.bootstrap.Endpoint(), "lease", h.clock.Now()))
	if err := h.ctrl.StartService(t.Context(), "node", domain.RolePrefill); err != nil {
		t.Fatal(err)
	}
	workerID := h.instance("node").RouterWorkerID

	// Every probe misses for the whole test, and the Router reports the instance
	// generating throughout: exactly the shape of a worker under load.
	h.bootstrap.setHealthStatus("degraded")
	if !h.router.setLoad(h.instance("node").ServiceURL(), 4) {
		t.Fatal("the Router fake lost the worker it just registered")
	}

	rounds := h.ctrl.HealthFailureThreshold() * 3
	for round := 1; round <= rounds; round++ {
		if _, err := h.ctrl.ReconcileOnce(t.Context()); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if state := h.instance("node").ServiceState; state != domain.ServiceServing {
			t.Fatalf("round %d: a busy instance was condemned after %d missed probes: %s",
				round, round, state)
		}
	}
	if got := h.router.readinessOf(workerID); got != "ready" {
		t.Fatalf("readiness=%s, want ready: a busy instance must stay routable", got)
	}
	if _, ok := h.ctrl.healthFailures.Load("node"); ok {
		t.Fatal("a miss while the instance was busy must not start a failure streak")
	}

	// Once the work is gone the same miss does count, which is the check that
	// the gate is a gate and not a way of ignoring the probe altogether.
	if !h.router.setLoad(h.instance("node").ServiceURL(), 0) {
		t.Fatal("could not clear the load")
	}
	if _, err := h.ctrl.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.ctrl.healthFailures.Load("node"); !ok {
		t.Fatal("an idle instance that cannot generate must start a streak")
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
