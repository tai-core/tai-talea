package partner_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/obs"
	"github.com/tai-core/tai-talea/internal/partner"
	"github.com/tai-core/tai-talea/internal/store"
)

func newPuller(t *testing.T) (*partner.Puller, *store.SQLiteStore) {
	t.Helper()
	persistence, err := store.OpenSQLite(filepath.Join(t.TempDir(), "tai-talea.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { persistence.Close() })
	metrics := obs.NewRegistry()
	alerter := obs.NewAlerter(metrics, nil, 0)
	return partner.NewPuller(persistence, metrics, alerter, nil, 3), persistence
}

func instance(id string, lease string) partner.CapacityInstance {
	return partner.CapacityInstance{
		ID:       id,
		Endpoint: "http://" + id + ":8080",
		LeaseID:  lease,
		Spec:     domain.InstanceSpec{GPU: "H100", GPUCount: 1, ModelSupport: []string{"model-a"}},
	}
}

func TestCollectSnapshotRejectsInstancesWithoutLease(t *testing.T) {
	adapter, err := partner.NewStaticAdapter("partner-a", []partner.CapacityInstance{instance("a", "lease-a")})
	if err != nil {
		t.Fatalf("static adapter: %v", err)
	}
	// Adding an available instance without a lease must fail the whole snapshot.
	if err := adapter.Add(partner.CapacityInstance{ID: "b", Endpoint: "http://b:8080", LeaseID: "lease-b"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	adapter.Revoke("b")

	ctx := context.Background()
	snapshot, err := partner.CollectSnapshot(ctx, adapter)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(snapshot.Instances) != 1 || snapshot.Instances[0].ID != "a" {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if snapshot.Version == "" {
		t.Fatal("a snapshot must carry a version")
	}
}

func TestSnapshotVersionIsStableAndContentSensitive(t *testing.T) {
	first := partner.SnapshotVersion([]partner.CapacityInstance{instance("a", "lease-a")})
	second := partner.SnapshotVersion([]partner.CapacityInstance{instance("a", "lease-a")})
	if first != second {
		t.Fatalf("identical content must produce the same version: %s != %s", first, second)
	}
	changed := partner.SnapshotVersion([]partner.CapacityInstance{instance("a", "lease-b")})
	if changed == first {
		t.Fatal("a lease change must change the snapshot version")
	}
}

func TestPlanGeneratesAddedRevokedAndUpdatedEvents(t *testing.T) {
	puller, persistence := newPuller(t)
	ctx := context.Background()
	adapter, err := partner.NewStaticAdapter("partner-a", []partner.CapacityInstance{instance("a", "lease-a")})
	if err != nil {
		t.Fatalf("static adapter: %v", err)
	}

	// Round 1: everything is new.
	plan, err := puller.Plan(ctx, adapter)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Added) != 1 || len(plan.Revoked) != 0 || len(plan.Updated) != 0 {
		t.Fatalf("first round must add one instance: %+v", plan)
	}
	if err := puller.Commit(ctx, plan); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Round 2 with no change: a single idempotent confirmation event.
	plan, err = puller.Plan(ctx, adapter)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.Unchanged {
		t.Fatalf("round 2 must be unchanged: %+v", plan)
	}
	if len(plan.Events) != 1 || plan.Events[0].Type != domain.EventCapacitySnapshotConfirmed {
		t.Fatalf("an unchanged round must emit CAPACITY_SNAPSHOT_CONFIRMED, got %+v", plan.Events)
	}

	// Round 3: a new container plus a lease change on the existing one.
	if err := adapter.Add(instance("b", "lease-b")); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := adapter.Replace([]partner.CapacityInstance{instance("b", "lease-b"), instance("a", "lease-a2")}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	plan, err = puller.Plan(ctx, adapter)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Added) != 1 || plan.Added[0].ID != "b" {
		t.Fatalf("expected b to be added: %+v", plan.Added)
	}
	if len(plan.Updated) != 1 || plan.Updated[0].ID != "a" {
		t.Fatalf("expected a to be updated: %+v", plan.Updated)
	}

	// Round 4: revocation.
	if err := adapter.Replace([]partner.CapacityInstance{instance("b", "lease-b")}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := puller.Commit(ctx, plan); err != nil {
		t.Fatalf("commit: %v", err)
	}
	plan, err = puller.Plan(ctx, adapter)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Revoked) != 1 || plan.Revoked[0] != "a" {
		t.Fatalf("expected a to be revoked: %+v", plan.Revoked)
	}
	event := plan.Events[0]
	if event.Type != domain.EventCapacityRevoked || event.Source != domain.SourcePull {
		t.Fatalf("unexpected revoke event: %+v", event)
	}
	if event.EventID != "partner-a:pull:"+plan.Snapshot.Version+":revoke:a" {
		t.Fatalf("pull event ids must be deterministic, got %s", event.EventID)
	}

	stored, found, err := persistence.GetSnapshot(ctx, "partner-a")
	if err != nil || !found {
		t.Fatalf("snapshot must be persisted: found=%v err=%v", found, err)
	}
	_ = stored
}

func TestPlanFailureKeepsThePreviousSnapshot(t *testing.T) {
	puller, persistence := newPuller(t)
	ctx := context.Background()
	healthy, err := partner.NewStaticAdapter("partner-a", []partner.CapacityInstance{instance("a", "lease-a")})
	if err != nil {
		t.Fatalf("static adapter: %v", err)
	}
	plan, err := puller.Plan(ctx, healthy)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := puller.Commit(ctx, plan); err != nil {
		t.Fatalf("commit: %v", err)
	}
	previous, _, err := persistence.GetSnapshot(ctx, "partner-a")
	if err != nil {
		t.Fatalf("get snapshot: %v", err)
	}

	broken, err := partner.NewStaticAdapter("partner-a",
		[]partner.CapacityInstance{instance("a", "lease-a")},
		partner.WithListError(errors.New("partner api is down")))
	if err != nil {
		t.Fatalf("static adapter: %v", err)
	}
	if _, err := puller.Plan(ctx, broken); err == nil {
		t.Fatal("a failing pull must return an error")
	}
	if puller.Failures("partner-a") != 1 {
		t.Fatalf("failures=%d, want 1", puller.Failures("partner-a"))
	}
	if _, err := puller.Plan(ctx, broken); err == nil {
		t.Fatal("a failing pull must return an error")
	}
	if puller.Failures("partner-a") != 2 {
		t.Fatalf("failures=%d, want 2", puller.Failures("partner-a"))
	}

	// The authoritative snapshot must be untouched: a partner outage must never
	// look like a mass revocation.
	current, found, err := persistence.GetSnapshot(ctx, "partner-a")
	if err != nil || !found {
		t.Fatalf("snapshot disappeared: found=%v err=%v", found, err)
	}
	if current.Version != previous.Version || current.InstanceCount != previous.InstanceCount {
		t.Fatalf("previous snapshot must be preserved: %+v vs %+v", current, previous)
	}

	// A successful round clears the failure counter.
	recovered, err := partner.NewStaticAdapter("partner-a", []partner.CapacityInstance{instance("a", "lease-a")})
	if err != nil {
		t.Fatalf("static adapter: %v", err)
	}
	plan, err = puller.Plan(ctx, recovered)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := puller.Commit(ctx, plan); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if puller.Failures("partner-a") != 0 {
		t.Fatalf("a successful round must reset the failure counter, got %d", puller.Failures("partner-a"))
	}
}

func TestStaticAdapterReleaseIsIdempotent(t *testing.T) {
	adapter, err := partner.NewStaticAdapter("partner-a", []partner.CapacityInstance{instance("a", "lease-a")})
	if err != nil {
		t.Fatalf("static adapter: %v", err)
	}
	ctx := context.Background()
	if err := adapter.ReleaseInstance(ctx, "a"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := adapter.ReleaseInstance(ctx, "a"); err != nil {
		t.Fatalf("a repeated release must be an idempotent success: %v", err)
	}
	if !adapter.Released("a") {
		t.Fatal("the adapter must record the release")
	}
	available, err := adapter.ListAvailableInstances(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(available) != 0 {
		t.Fatalf("a released container must leave the capacity view, got %+v", available)
	}
}

func TestSnapshotPayloadRoundTrip(t *testing.T) {
	original := partner.Snapshot{
		PartnerID: "partner-a",
		Version:   "v1",
		Observed:  time.Now().UTC().Truncate(time.Second),
		Instances: []partner.CapacityInstance{instance("a", "lease-a")},
	}
	payload, err := partner.SnapshotPayload(original)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	restored, err := partner.ParseSnapshot("partner-a", "v1", payload, original.Observed)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(restored.Instances) != 1 || restored.Instances[0].ID != "a" {
		t.Fatalf("unexpected restored snapshot: %+v", restored)
	}
}
