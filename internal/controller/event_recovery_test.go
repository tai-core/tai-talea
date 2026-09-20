package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/partner"
	"github.com/tai-core/tai-talea/internal/store"
)

func TestPendingAddRecoversBeforeAndAfterAllocating(t *testing.T) {
	for _, allocated := range []bool{false, true} {
		t.Run(map[bool]string{false: "claimed-only", true: "allocating-written"}[allocated], func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			event := h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now())
			if _, err := h.store.ClaimEvent(ctx, event, domain.ResultPending, ""); err != nil {
				t.Fatal(err)
			}
			if allocated {
				if err := h.store.ApplyTransition(ctx, store.Transition{InstanceID: "container-1", Next: domain.Instance{
					ID: "container-1", PartnerID: "partner-a", Endpoint: h.bootstrap.Endpoint(), LeaseID: "lease-1",
					InstanceState: domain.InstanceAllocating, ServiceState: domain.ServiceNone, LeaseUpdatedAt: event.OccurredAt,
				}}); err != nil {
					t.Fatal(err)
				}
			}
			restarted := buildController(t, h.cfg, h.store, h.router, h.bootstrap, h.adapter, h.metrics, h.recorder, h.clock)
			if _, _, err := restarted.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if row := h.instance("container-1"); row.InstanceState != domain.InstanceIdle {
				t.Fatalf("interrupted add not recovered: %+v", row)
			}
			record, err := h.store.GetEvent(ctx, event.EventID)
			if err != nil || record.Result != domain.ResultApplied {
				t.Fatalf("event not finalized: %+v %v", record, err)
			}
		})
	}
}

func TestPendingRevokeSurvivesRestartAndPreventsRestart(t *testing.T) {
	h := servingHarness(t)
	ctx := context.Background()
	event := h.revokedEvent("container-1", 1, h.clock.Now())
	if _, err := h.store.ClaimEvent(ctx, event, domain.ResultPending, ""); err != nil {
		t.Fatal(err)
	}
	restarted := buildController(t, h.cfg, h.store, h.router, h.bootstrap, h.adapter, h.metrics, h.recorder, h.clock)
	if _, _, err := restarted.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if row := h.instance("container-1"); row.InstanceState != domain.InstanceReleased {
		t.Fatalf("interrupted revoke was lost: %+v", row)
	}
}

func TestRecoverySkipsAnEventStillOwnedByLiveDelivery(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	event := h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now().Add(-time.Hour))
	if _, err := h.store.ClaimEvent(ctx, event, domain.ResultPending, ""); err != nil {
		t.Fatal(err)
	}
	lock := h.ctrl.eventOperation(event.EventID)
	lock.Lock()
	resumed, err := h.ctrl.ResumePendingEvents(ctx)
	lock.Unlock()
	if err != nil || resumed != 0 {
		t.Fatalf("in-flight event replayed: count=%d err=%v", resumed, err)
	}
	record, _ := h.store.GetEvent(ctx, event.EventID)
	if record.Result != domain.ResultPending {
		t.Fatalf("in-flight event discarded based on age: %+v", record)
	}
	if _, err := h.store.GetInstance(ctx, "container-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("recovery mutated instance owned by live delivery", err)
	}
}

func TestBusyPendingEventsDoNotStarveLaterWork(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		event := h.addedEvent("busy", h.bootstrap.Endpoint(), "lease", h.clock.Now())
		event.EventID = fmt.Sprintf("blocked-%03d", i)
		if _, err := h.store.ClaimEvent(ctx, event, domain.ResultPending, ""); err != nil {
			t.Fatal(err)
		}
	}
	ready := h.revokedEvent("unknown", 1, h.clock.Now())
	ready.EventID = "z-ready"
	if _, err := h.store.ClaimEvent(ctx, ready, domain.ResultPending, ""); err != nil {
		t.Fatal(err)
	}
	lock := h.ctrl.instanceOperation("busy")
	lock.Lock()
	defer lock.Unlock()
	for i := 0; i < 2; i++ {
		if _, err := h.ctrl.ResumePendingEvents(ctx); err != nil {
			t.Fatal(err)
		}
	}
	record, _ := h.store.GetEvent(ctx, ready.EventID)
	if record.Result != domain.ResultApplied {
		t.Fatalf("early busy events starved a later revoke: %+v", record)
	}
}

func TestRecoveryAppliesOlderAddBeforeLaterRevoke(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	add := h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now())
	add.EventID = "z-add"
	revoke := h.revokedEvent("container-1", 1, h.clock.Now().Add(time.Second))
	revoke.EventID = "a-revoke"
	// Receipt order and lexical id order both put revoke first. Recovery uses
	// event time so the accepted later revocation cannot be lost as "absent".
	for _, event := range []domain.CapacityEvent{revoke, add} {
		if _, err := h.store.ClaimEvent(ctx, event, domain.ResultPending, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.ctrl.ResumePendingEvents(ctx); err != nil {
		t.Fatal(err)
	}
	row, err := h.store.GetInstance(ctx, "container-1")
	if !errors.Is(err, store.ErrNotFound) && (err != nil || row.InstanceState != domain.InstanceReleased) {
		t.Fatalf("pending revoke applied before older add and lost: %+v %v", row, err)
	}
}

func TestDelayedAddCannotUndoRevokeOfUnknownInstance(t *testing.T) {
	h := newHarness(t)
	now := h.clock.Now()
	h.handle(h.revokedEvent("container-1", 1, now.Add(time.Second)))
	if outcome := h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", now)); outcome.Accepted {
		t.Fatalf("delayed old add resurrected revoked capacity: %+v", outcome)
	}
	if _, err := h.store.GetInstance(context.Background(), "container-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("revoked unknown capacity was created", err)
	}
}

func TestUpdateCannotChangeIdentityDuringReclaim(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
	if err := h.ctrl.RequestReclaim(ctx, "container-1", "operator", time.Minute); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Second)
	outcome := h.handle(domain.CapacityEvent{EventID: "too-late-update", PartnerID: "partner-a", Type: domain.EventCapacityUpdated,
		OccurredAt: h.clock.Now(), Instance: &domain.EventInstance{ID: "container-1", Endpoint: "http://replacement:9001", LeaseID: "new-lease"}})
	if outcome.Accepted {
		t.Fatalf("metadata changed underneath pending reclaim: %+v", outcome)
	}
	row := h.instance("container-1")
	if row.Endpoint != h.bootstrap.Endpoint() || row.LeaseID != "lease-1" || !row.PendingRelease {
		t.Fatalf("reclaim identity overwritten: %+v", row)
	}
}

type replaceAfterReclaimStore struct {
	store.Store
	hook func()
}

func (s *replaceAfterReclaimStore) ApplyTransition(ctx context.Context, transition store.Transition) error {
	err := s.Store.ApplyTransition(ctx, transition)
	if err == nil && transition.RequestRelease && s.hook != nil {
		hook := s.hook
		s.hook = nil
		hook()
	}
	return err
}

func TestRevokeDoesNotDrainNewLeaseAdmittedAfterOldRelease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
	h.ctrl.store = &replaceAfterReclaimStore{Store: h.store, hook: func() {
		if err := h.ctrl.BeginRelease(ctx, "container-1", "concurrent reconciler"); err != nil {
			t.Fatal(err)
		}
		if err := h.ctrl.EnrollPrepared(ctx, domain.Instance{ID: "container-1", PartnerID: "partner-a",
			Endpoint: h.bootstrap.Endpoint(), LeaseID: "replacement"}); err != nil {
			t.Fatal(err)
		}
	}}
	if result := h.handle(h.revokedEvent("container-1", 1, h.clock.Now())); !result.Accepted {
		t.Fatal(result)
	}
	row := h.instance("container-1")
	if row.LeaseID != "replacement" || row.InstanceState != domain.InstancePreparing || row.PendingRelease {
		t.Fatalf("old revoke touched replacement lease: %+v", row)
	}
}

func TestEventIDAndInstanceOwnershipAreBound(t *testing.T) {
	h := newHarness(t)
	event := h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now())
	h.handle(event)
	for _, kind := range []domain.EventType{domain.EventCapacityAdded, domain.EventCapacityUpdated, domain.EventCapacityRevoked} {
		foreign := event
		foreign.EventID, foreign.PartnerID, foreign.Type = "foreign:"+string(kind), "partner-other", kind
		if outcome := h.handle(foreign); outcome.Accepted || outcome.Result != domain.ResultRejected {
			t.Fatalf("foreign %s accepted: %+v", kind, outcome)
		}
	}
	foreignDuplicate := event
	foreignDuplicate.PartnerID = "partner-other"
	if outcome := h.handle(foreignDuplicate); outcome.Accepted || outcome.Duplicate {
		t.Fatalf("event id revealed another partner result: %+v", outcome)
	}
	conflict := event
	payload := *event.Instance
	payload.LeaseID = "different-lease"
	conflict.Instance = &payload
	if outcome := h.handle(conflict); outcome.Accepted {
		t.Fatalf("event id reused for different payload: %+v", outcome)
	}
	retry := event
	retry.OccurredAt = retry.OccurredAt.Add(time.Minute)
	if outcome := h.handle(retry); !outcome.Accepted || !outcome.Duplicate {
		t.Fatalf("same intent with new retry timestamp rejected: %+v", outcome)
	}
	if row := h.instance("container-1"); row.PartnerID != "partner-a" || row.LeaseID != "lease-1" || row.PendingRelease {
		t.Fatalf("ownership changed: %+v", row)
	}
}

func TestReofferRequiresFreshLeaseAndCannotDuplicateActiveEndpoint(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first := h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now())
	h.handle(first)
	if err := h.ctrl.BeginRelease(ctx, "container-1", "test"); err != nil {
		t.Fatal(err)
	}
	sameLease := first
	sameLease.EventID = "same-old-lease"
	h.handle(sameLease)
	if h.instance("container-1").InstanceState != domain.InstanceReleased {
		t.Fatal("old lease resurrected")
	}
	h.clock.Advance(time.Second)
	newLease := h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-2", h.clock.Now())
	newLease.EventID = "new-lease"
	if result := h.handle(newLease); result.Result != domain.ResultApplied {
		t.Fatal(result)
	}
	if row := h.instance("container-1"); row.LeaseID != "lease-2" || row.InstanceState != domain.InstanceIdle || row.PendingRelease {
		t.Fatalf("new lease not prepared: %+v", row)
	}
	if result := h.handle(h.addedEvent("container-2", h.bootstrap.Endpoint(), "other", h.clock.Now())); result.Accepted {
		t.Fatalf("same endpoint admitted twice: %+v", result)
	}
	if _, err := h.store.GetInstance(ctx, "container-2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("duplicate endpoint created row", err)
	}
}

func TestRevokeFailedServicePersistsAndCompletesReclaim(t *testing.T) {
	h := servingHarness(t)
	ctx := context.Background()
	row := h.instance("container-1")
	row.ServiceState = domain.ServiceFailed
	if err := h.store.ApplyTransition(ctx, store.Transition{InstanceID: row.ID, Next: row}); err != nil {
		t.Fatal(err)
	}
	if result := h.handle(h.revokedEvent(row.ID, 1, h.clock.Now())); !result.Accepted {
		t.Fatal(result)
	}
	h.reconcile()
	if h.instance(row.ID).InstanceState != domain.InstanceReleased {
		t.Fatal("failed service revoke not reclaimed")
	}
}

func TestPullRepairsUnchangedSnapshotDriftAndABA(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	puller := partner.NewPuller(h.store, h.metrics, nil, nil, 3)
	schedule := PullSchedule{Adapter: h.adapter, Puller: puller}
	if err := h.ctrl.PullOnce(ctx, schedule); err != nil {
		t.Fatal(err)
	}
	original := h.instance("container-1")
	changed := original
	changed.Endpoint = "http://10.0.0.9:8080"
	if err := h.store.ApplyTransition(ctx, store.Transition{InstanceID: changed.ID, Next: changed}); err != nil {
		t.Fatal(err)
	}
	// The upstream snapshot is byte-for-byte unchanged; current intent drifted.
	if err := h.ctrl.PullOnce(ctx, schedule); err != nil {
		t.Fatal(err)
	}
	if row := h.instance(original.ID); row.Endpoint != original.Endpoint {
		t.Fatalf("unchanged snapshot failed to repair drift: %+v", row)
	}
	for _, endpoint := range []string{"http://10.0.0.2:8080", original.Endpoint} {
		if err := h.adapter.Replace([]partner.CapacityInstance{{ID: original.ID, Endpoint: endpoint,
			LeaseID: original.LeaseID, Spec: original.Spec}}); err != nil {
			t.Fatal(err)
		}
		if err := h.ctrl.PullOnce(ctx, schedule); err != nil {
			t.Fatal(err)
		}
		if h.instance(original.ID).Endpoint != endpoint {
			t.Fatal("snapshot A -> B -> A change suppressed by event deduplication")
		}
	}
	// A node introduced only by Push is also revoked by an unchanged full view.
	extra := h.addedEvent("extra", "http://10.0.0.7:8080", "extra-lease", h.clock.Now())
	h.handle(extra)
	if err := h.ctrl.PullOnce(ctx, schedule); err != nil {
		t.Fatal(err)
	}
	if h.instance("extra").InstanceState != domain.InstanceReleased {
		t.Fatal("unchanged authoritative snapshot did not revoke drift")
	}
}

func TestPullClearsRemovedServiceEndpoint(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	schedule := PullSchedule{Adapter: h.adapter, Puller: partner.NewPuller(h.store, h.metrics, nil, nil, 3)}
	if err := h.ctrl.PullOnce(ctx, schedule); err != nil {
		t.Fatal(err)
	}
	row := h.instance("container-1")
	row.ServiceEndpoint = "http://old-service:9002"
	if err := h.store.ApplyTransition(ctx, store.Transition{InstanceID: row.ID, Next: row}); err != nil {
		t.Fatal(err)
	}
	if err := h.ctrl.PullOnce(ctx, schedule); err != nil {
		t.Fatal(err)
	}
	if h.instance(row.ID).ServiceEndpoint != "" {
		t.Fatal("full snapshot omission must restore endpoint fallback")
	}
}

func TestQueuedIdentityReservesEndpointsAgainstAdmissions(t *testing.T) {
	h := servingHarness(t)
	row := h.instance("container-1")
	row.PendingUpdate = &domain.InstanceUpdate{Endpoint: "http://reserved:9001", ServiceEndpoint: "http://reserved:9002", LeaseID: "next", ObservedAt: h.clock.Now()}
	if err := h.store.ApplyTransition(t.Context(), store.Transition{InstanceID: row.ID, Next: row}); err != nil {
		t.Fatal(err)
	}
	if outcome := h.handle(h.addedEvent("new-node", "http://reserved:9001", "other", h.clock.Now())); outcome.Accepted {
		t.Fatal("Push stole queued endpoint")
	}
	err := h.ctrl.EnrollPrepared(t.Context(), domain.Instance{ID: "ssh-node", PartnerID: row.PartnerID, Endpoint: "http://other:9001", ServiceEndpoint: "http://reserved:9002", LeaseID: "ssh-new"})
	if err == nil {
		t.Fatal("SSH enrollment stole queued service endpoint")
	}
}
