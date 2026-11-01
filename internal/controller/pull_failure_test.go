package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/obs"
	"github.com/tai-core/tai-talea/internal/partner"
	"github.com/tai-core/tai-talea/internal/store"
)

// commitFails delegates every write to the real store except the one that makes
// a pull round count: the snapshot. That is the shape of the failure this file
// is about - all the events were applied, the authoritative version never moved.
type commitFails struct {
	store.Store
	cause error
}

func (c commitFails) SaveSnapshot(context.Context, store.SnapshotRecord) error {
	return c.cause
}

// A round that cannot commit its snapshot must not be silent.
//
// Plan records its own failures. Every stage after it - applying an event, the
// ownership collision check, Commit itself - used to return its error to a
// caller that logged it at debug and told nobody. The cost is not one lost
// round: the snapshot version stays where it was, so the next round re-diffs the
// same baseline into the same events and fails the same way, for as long as the
// cause lasts. One instance that can never be applied is enough to wedge
// capacity reconciliation permanently, and a wedged control plane looked exactly
// like a healthy one - the only trace was a partner_sync_success_timestamp that
// stopped advancing.
//
// The test forces the failure at Commit because that is the last stage: it also
// proves the recording covers the returns that are written after the sweep, not
// just the one inside the event loop.
func TestAPullRoundThatCannotCommitSaysSo(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if outcome := h.handle(h.addedEvent("container-1", "http://10.0.0.1:8080", "lease-1", h.clock.Now())); outcome.Result != domain.ResultApplied {
		t.Fatalf("offer container-1: %s %s", outcome.Result, outcome.Reason)
	}
	h.recorder.reset()

	cause := errors.New("snapshot write failed")
	puller := partner.NewPuller(
		commitFails{Store: h.store, cause: cause},
		h.metrics,
		obs.NewAlerter(h.metrics, nil, 0, h.recorder),
		nil, 3)

	err := h.ctrl.PullOnce(ctx, PullSchedule{Adapter: h.adapter, Puller: puller})
	if !errors.Is(err, cause) {
		t.Fatalf("pull round returned %v, want the commit failure", err)
	}
	if got := puller.Failures("partner-a"); got != 1 {
		t.Fatalf("the failed round left the failure count at %d, want 1: a round that cannot "+
			"commit is indistinguishable from a partner outage and has to be counted like one", got)
	}
	if !h.recorder.has(obs.AlertPartnerSyncFailed) {
		t.Fatalf("a round that never commits must alert, got %v", h.recorder.names())
	}
}

// The ownership check is the earliest place a round can die after Plan: a
// container the store already attributes to another partner is a configuration
// mistake, and the round refuses the whole snapshot rather than take the row
// over. Refusing is right - the version simply must not advance - but it leaves
// capacity frozen for as long as the mistake stands, so it has to be as visible
// as an unreachable partner.
func TestAPullRoundThatRefusesAForeignInstanceSaysSo(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.store.ApplyTransition(ctx, store.Transition{InstanceID: "container-1", Next: domain.Instance{
		ID: "container-1", PartnerID: "partner-b", Endpoint: "http://10.0.0.9:8080", LeaseID: "lease-9",
		InstanceState: domain.InstanceAllocating, ServiceState: domain.ServiceNone, LeaseUpdatedAt: h.clock.Now(),
	}}); err != nil {
		t.Fatalf("seed an instance another partner owns: %v", err)
	}

	puller := partner.NewPuller(h.store, h.metrics, obs.NewAlerter(h.metrics, nil, 0, h.recorder), nil, 3)
	err := h.ctrl.PullOnce(ctx, PullSchedule{Adapter: h.adapter, Puller: puller})
	if err == nil || !strings.Contains(err.Error(), "belongs to another partner") {
		t.Fatalf("pull round returned %v, want the ownership refusal", err)
	}
	if got := puller.Failures("partner-a"); got != 1 {
		t.Fatalf("the refused round left the failure count at %d, want 1", got)
	}
	if !h.recorder.has(obs.AlertPartnerSyncFailed) {
		t.Fatalf("a snapshot that cannot be applied must alert, got %v", h.recorder.names())
	}
}

// The same round, with a store that commits: nothing may be counted, or the
// counter would sit at 1 forever and the alert would mean nothing.
func TestAPullRoundThatCommitsCountsNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if outcome := h.handle(h.addedEvent("container-1", "http://10.0.0.1:8080", "lease-1", h.clock.Now())); outcome.Result != domain.ResultApplied {
		t.Fatalf("offer container-1: %s %s", outcome.Result, outcome.Reason)
	}
	h.recorder.reset()

	puller := partner.NewPuller(h.store, h.metrics, obs.NewAlerter(h.metrics, nil, 0, h.recorder), nil, 3)
	if err := h.ctrl.PullOnce(ctx, PullSchedule{Adapter: h.adapter, Puller: puller}); err != nil {
		t.Fatalf("pull round: %v", err)
	}
	if got := puller.Failures("partner-a"); got != 0 {
		t.Fatalf("a round that committed left the failure count at %d, want 0", got)
	}
	if h.recorder.has(obs.AlertPartnerSyncFailed) {
		t.Fatalf("a successful round alerted: %v", h.recorder.names())
	}
}
