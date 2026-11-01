package controller

import (
	"testing"

	"github.com/tai-core/tai-talea/internal/domain"
)

// The aggregated shape has to survive the same lifecycle a PD worker does:
// start, health, register, readiness. This is the seam where a third role would
// break first - the bootstrap rejects an unknown role, and the Router would file
// the worker in the wrong pool - so the assertions are the same ones the PD
// acceptance test makes, with "regular" substituted.
func TestStartServiceAcceptsARegularRole(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))

	if err := h.ctrl.StartService(t.Context(), "container-1", domain.RoleRegular); err != nil {
		t.Fatalf("start service with a regular role: %v", err)
	}

	instance := h.instance("container-1")
	if instance.ServiceState != domain.ServiceServing {
		t.Fatalf("service_state=%s, want SERVING", instance.ServiceState)
	}
	if instance.Role != domain.RoleRegular {
		t.Fatalf("role=%s, want regular", instance.Role)
	}
	if instance.RouterWorkerID == "" {
		t.Fatal("SERVING requires a registered router worker id")
	}

	h.bootstrap.mu.Lock()
	bootstrapRole := h.bootstrap.role
	h.bootstrap.mu.Unlock()
	if bootstrapRole != string(domain.RoleRegular) {
		t.Fatalf("bootstrap was asked to run role %q, want regular", bootstrapRole)
	}

	worker, ok := h.router.workerByURL(h.bootstrap.Endpoint())
	if !ok {
		t.Fatalf("the router must know the worker url %s", h.bootstrap.Endpoint())
	}
	if worker.WorkerType != string(domain.RoleRegular) {
		t.Fatalf("router worker type=%s, want regular", worker.WorkerType)
	}
	if h.router.readinessOf(worker.ID) != "ready" {
		t.Fatalf("readiness=%s, want ready", h.router.readinessOf(worker.ID))
	}
	if h.router.deletes() != 0 {
		t.Fatal("the control plane must never delete a router worker")
	}
}

// A regular worker must not be described as a prefill one to the Router: the
// bootstrap port is where a decode peer pulls KV from, and an aggregated pool
// has no such peer. Sending it would make the Router inject a disaggregation
// hint into an ordinary request.
func TestPrefillBootstrapPortOnlyAppliesToPrefill(t *testing.T) {
	if got := prefillBootstrapPort(domain.RolePrefill, 8998); got != 8998 {
		t.Fatalf("prefill bootstrap port = %d, want 8998", got)
	}
	for _, role := range []domain.Role{domain.RoleDecode, domain.RoleRegular, domain.RoleNone} {
		if got := prefillBootstrapPort(role, 8998); got != 0 {
			t.Fatalf("%q was given bootstrap port %d, want none", role, got)
		}
	}
}
