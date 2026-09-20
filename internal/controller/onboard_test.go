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
