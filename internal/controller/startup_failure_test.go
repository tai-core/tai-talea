package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/launcher"
)

type crashingLauncher struct {
	Launcher
	probes int
}

func (l *crashingLauncher) Health(context.Context, string) (launcher.Health, error) {
	l.probes++
	if l.probes == 1 {
		return launcher.Health{Status: "degraded", Phase: launcher.PhaseRunning}, nil
	}
	return launcher.Health{
		Status: "failed", Phase: launcher.PhaseFailed,
		Detail: "sglang exited unexpectedly with code 3",
	}, nil
}

func TestStartRecordsLateProcessFailureWithoutWaitingForHealthTimeout(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
	crash := &crashingLauncher{Launcher: h.ctrl.launcher}
	h.ctrl.launcher = crash
	// The test clock never advances, so the configured start deadline cannot
	// expire. The parent deadline only bounds a regression that keeps polling.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := h.ctrl.StartService(ctx, "container-1", domain.RolePrefill)
	if err == nil || !strings.Contains(err.Error(), "exited unexpectedly with code 3") {
		t.Fatalf("start error=%v, want the bootstrap's process failure", err)
	}
	if crash.probes != 2 {
		t.Fatalf("health probes=%d, want to stop at the first failed phase", crash.probes)
	}
	instance := h.instance("container-1")
	if instance.ServiceState != domain.ServiceFailed || !strings.Contains(instance.LastError, "code 3") {
		t.Fatalf("failure was not persisted: %+v", instance)
	}
	if _, found := h.router.workerByURL(h.bootstrap.Endpoint()); found {
		t.Fatal("a crashed service must not be registered with the router")
	}
}

type failedHealthLauncher struct {
	Launcher
	failed bool
}

func (l *failedHealthLauncher) Health(ctx context.Context, endpoint string) (launcher.Health, error) {
	if l.failed {
		return launcher.Health{Status: "failed", Phase: launcher.PhaseFailed, Version: testBootstrapVersion, Detail: "test crash"}, nil
	}
	return l.Launcher.Health(ctx, endpoint)
}
func TestCrashedModelDoesNotMakeReachableContainerLost(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("node", h.bootstrap.Endpoint(), "lease", h.clock.Now()))
	if err := h.ctrl.StartService(t.Context(), "node", domain.RoleDecode); err != nil {
		t.Fatal(err)
	}
	wrapped := &failedHealthLauncher{Launcher: h.ctrl.launcher, failed: true}
	h.ctrl.launcher = wrapped
	h.bootstrap.mu.Lock()
	h.bootstrap.phase = launcher.PhaseFailed
	h.bootstrap.servicePID = 0
	h.bootstrap.mu.Unlock()
	_, _ = h.ctrl.ReconcileOnce(t.Context())
	n := h.instance("node")
	if n.InstanceState != domain.InstanceIdle || n.ServiceState != domain.ServiceFailed {
		t.Fatalf("%+v", n)
	}
	if h.router.readinessOf(n.RouterWorkerID) != "unavailable" {
		t.Fatal("crashed process still routable")
	}
	wrapped.failed = false
	h.clock.Advance(time.Minute)
	_, _ = h.ctrl.ReconcileOnce(t.Context())
	if h.instance("node").ServiceState != domain.ServiceServing {
		t.Fatal("model was not automatically restarted")
	}
}
