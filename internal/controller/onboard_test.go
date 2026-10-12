package controller

import (
	"github.com/tai-core/tai-talea/internal/domain"
	"testing"
	"time"
)

func TestSSHReofferReleasedNodeAndRestartEnrollment(t *testing.T) {
	h := servingHarness(t)
	ctx := t.Context()
	if err := h.ctrl.RequestReclaim(ctx, "container-1", "test", 0); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if got := h.instance("container-1"); got.InstanceState != domain.InstanceReleased {
		t.Fatal(got)
	}
	input := domain.Instance{ID: "container-1", PartnerID: "partner-a", LeaseID: "ssh-new-lease", Endpoint: h.bootstrap.Endpoint()}
	if err := h.ctrl.EnrollPrepared(ctx, input); err != nil {
		t.Fatal(err)
	}
	if err := h.ctrl.EnrollPrepared(ctx, input); err != nil {
		t.Fatal("repeated enrollment", err)
	}
	queued := h.instance(input.ID)
	if queued.PendingRelease || queued.Role != "" || queued.InstanceState != domain.InstancePreparing {
		t.Fatal(queued)
	}
	h.clock.Advance(time.Second)
	h.reconcile()
	if h.instance(input.ID).InstanceState != domain.InstanceIdle {
		t.Fatal("did not prepare")
	}
	if err := h.ctrl.StartService(ctx, input.ID, domain.RolePrefill); err != nil {
		t.Fatal(err)
	}
	if h.instance(input.ID).ServiceState != domain.ServiceServing {
		t.Fatal("did not serve")
	}
	input.LeaseID = "ssh-other"
	if err := h.ctrl.EnrollPrepared(ctx, input); err == nil {
		t.Fatal("overwrote active node")
	}
	input.ID = "duplicate-endpoint"
	if err := h.ctrl.EnrollPrepared(ctx, input); err == nil {
		t.Fatal("duplicate active endpoint accepted")
	}
}

// The poll creates the row as soon as the platform reports the container, which
// is before the installation has run, so its first endpoint is whatever the
// platform publishes. The installation then measures the container's own
// address from the inside and verifies the control node can reach it — that is
// the address PD disaggregation needs, because the transfer handshake has no
// published port.
func TestEnrollmentAdoptsTheAddressMeasuredInsideTheContainer(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	id := "talea-node-1"
	published := "http://203.0.113.9:30028"

	h.handle(h.addedEvent(id, published, "cci-"+id, h.clock.Now()))
	if got := h.instance(id); got.Endpoint != published {
		t.Fatalf("unexpected published endpoint: %+v", got)
	}

	measured := domain.Instance{
		ID: id, PartnerID: "partner-a", LeaseID: "cci-" + id,
		Endpoint: "http://172.19.192.134:9001", ServiceEndpoint: "http://172.19.192.134:9002",
	}
	if err := h.ctrl.EnrollPrepared(ctx, measured); err != nil {
		t.Fatal(err)
	}
	if err := h.ctrl.EnrollPrepared(ctx, measured); err != nil {
		t.Fatal("repeated enrollment", err)
	}

	got := h.instance(id)
	if got.Endpoint != "http://172.19.192.134:9001" || got.ServiceEndpoint != "http://172.19.192.134:9002" {
		t.Fatalf("verified address not adopted: %+v", got)
	}
	if got.InstanceState != domain.InstancePreparing {
		t.Fatalf("adoption must re-enter preparation: %+v", got)
	}
}
