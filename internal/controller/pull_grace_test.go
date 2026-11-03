package controller

import (
	"context"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/partner"
)

// A container that reappears inside the revocation grace must have its
// absence clock thrown away, not paused (plan G4).
//
// The platform list can omit a live instance for minutes after a STOP/START;
// the measured window was about three. A clock that merely paused would let
// absence accumulated before the reappearance shortcut the grace of a later
// absence, so the second outage could fire CAPACITY_REVOKED - whose reclaim
// ends in deleting the container - sooner than the operator was promised.
func TestPullForgivesAnInstanceThatComesBackWithinTheGrace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	puller := partner.NewPuller(h.store, h.metrics, nil, nil, 3)
	schedule := PullSchedule{Adapter: h.adapter, Puller: puller}

	// The instance is present, so no clock exists.
	if err := h.ctrl.PullOnce(ctx, schedule); err != nil {
		t.Fatal(err)
	}
	if _, present := h.ctrl.pullMissingSince.Load("container-1"); present {
		t.Fatal("a visible instance carried an absence clock")
	}

	// First absence: the clock starts, nothing is revoked.
	h.clock.Advance(time.Minute)
	if err := h.adapter.Replace(nil); err != nil {
		t.Fatal(err)
	}
	if err := h.ctrl.PullOnce(ctx, schedule); err != nil {
		t.Fatal(err)
	}
	since, present := h.ctrl.pullMissingSince.Load("container-1")
	if !present {
		t.Fatal("the first absent snapshot did not start the grace")
	}
	_ = since

	// The container comes back within the grace: the clock must be forgotten.
	h.clock.Advance(time.Minute)
	if err := h.adapter.Replace([]partner.CapacityInstance{{ID: "container-1",
		Endpoint: "http://10.0.0.1:8080", LeaseID: "lease-1",
		Spec: domain.InstanceSpec{GPU: "H100", GPUCount: 1, ModelSupport: []string{testModelID}}}}); err != nil {
		t.Fatal(err)
	}
	if err := h.ctrl.PullOnce(ctx, schedule); err != nil {
		t.Fatal(err)
	}
	if _, present := h.ctrl.pullMissingSince.Load("container-1"); present {
		t.Fatal("a reappearance did not forgive the absence")
	}

	// A second absence long after the first one: even though the total time
	// since the first miss exceeds the grace, this miss starts its own clock
	// and must not revoke.
	h.clock.Advance(h.ctrl.RevocationGrace() + time.Minute)
	if err := h.adapter.Replace(nil); err != nil {
		t.Fatal(err)
	}
	if err := h.ctrl.PullOnce(ctx, schedule); err != nil {
		t.Fatal(err)
	}
	if row := h.instance("container-1"); row.PendingRelease || row.InstanceState == domain.InstanceReleased {
		t.Fatalf("a stale clock revoked a freshly-missing instance: state %s/%s, pending_release=%v",
			row.InstanceState, row.ServiceState, row.PendingRelease)
	}

	// Only an absence that outlasts the grace is believed.
	h.clock.Advance(h.ctrl.RevocationGrace() + time.Minute)
	if err := h.ctrl.PullOnce(ctx, schedule); err != nil {
		t.Fatal(err)
	}
	row := h.instance("container-1")
	if !row.PendingRelease && row.InstanceState != domain.InstanceReleased {
		t.Fatalf("absence past the grace did not revoke: state %s/%s, pending_release=%v",
			row.InstanceState, row.ServiceState, row.PendingRelease)
	}
}
