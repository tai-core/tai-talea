package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/store"
)

func TestClaimStoresNormalizedReplayPayload(t *testing.T) {
	persistence := newStore(t)
	ctx := context.Background()
	event := domain.CapacityEvent{EventID: "normalized", PartnerID: "authenticated-partner", Type: domain.EventCapacityRevoked,
		OccurredAt: time.Now().UTC(), Instance: &domain.EventInstance{ID: "node"},
		Raw: json.RawMessage(`{"event_id":"normalized","type":"CAPACITY_REVOKED","instance":{"id":"node"}}`)}
	if _, err := persistence.ClaimEvent(ctx, event, domain.ResultPending, ""); err != nil {
		t.Fatal(err)
	}
	records, err := persistence.ListPendingEvents(ctx, 100)
	if err != nil || len(records) != 1 {
		t.Fatalf("pending events: %+v %v", records, err)
	}
	var recovered domain.CapacityEvent
	if err := json.Unmarshal([]byte(records[0].PayloadJSON), &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.PartnerID != event.PartnerID || !recovered.OccurredAt.Equal(event.OccurredAt) {
		t.Fatalf("normalized receiver fields lost: %+v", recovered)
	}
}

func TestPendingUpdateSurvivesStaleWritesAndUsesLatestOnApply(t *testing.T) {
	persistence := newStore(t)
	ctx := context.Background()
	seedServing(t, persistence, "node", domain.RolePrefill)
	row, err := persistence.GetInstance(ctx, "node")
	if err != nil {
		t.Fatal(err)
	}
	stale := row
	queued := row
	queued.PendingUpdate = &domain.InstanceUpdate{Endpoint: "http://10.0.0.2:9001", ServiceEndpoint: "http://10.0.0.2:9002", LeaseID: "next", ObservedAt: time.Now().UTC()}
	if err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: row.ID, Next: queued}); err != nil {
		t.Fatal(err)
	}
	if err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: row.ID, Next: stale}); err != nil {
		t.Fatal(err)
	}
	current, _ := persistence.GetInstance(ctx, row.ID)
	if current.PendingUpdate == nil || current.PendingUpdate.Endpoint != queued.PendingUpdate.Endpoint || current.Endpoint != row.Endpoint {
		t.Fatalf("stale write erased update or replaced live endpoint: %+v", current)
	}
	current.ServiceState = domain.ServiceDraining
	if err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: row.ID, Next: current}); err != nil {
		t.Fatal(err)
	}
	current.ServiceState, current.Role, current.PendingUpdate = domain.ServiceNone, domain.RoleNone, nil
	current.InstanceState = domain.InstancePreparing
	if err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: row.ID, Next: current, ApplyPendingUpdate: true}); err != nil {
		t.Fatal(err)
	}
	current, _ = persistence.GetInstance(ctx, row.ID)
	if current.PendingUpdate != nil || current.Endpoint != queued.PendingUpdate.Endpoint || current.LeaseID != "next" {
		t.Fatalf("queued update not atomically consumed: %+v", current)
	}
}

func TestSchemaV2MigrationPreservesInstances(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v2.db")
	persistence, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	seedIdle(t, persistence, "preserved")
	if _, err := persistence.ClaimEvent(ctx, domain.CapacityEvent{EventID: "interrupted", PartnerID: "partner-a",
		Type: domain.EventCapacityRevoked, OccurredAt: time.Now().UTC(), Instance: &domain.EventInstance{ID: "preserved"}}, domain.ResultAbandoned, "old process interrupted"); err != nil {
		t.Fatal(err)
	}
	if err := persistence.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE capacity_instances DROP COLUMN pending_update_json; PRAGMA user_version = 2;"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	persistence, err = store.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer persistence.Close()
	row, err := persistence.GetInstance(ctx, "preserved")
	if err != nil || row.InstanceState != domain.InstanceIdle || row.PendingUpdate != nil {
		t.Fatalf("v2 migration failed: %+v %v", row, err)
	}
	pending, err := persistence.ListPendingEvents(ctx, 100)
	if err != nil || len(pending) != 0 {
		t.Fatalf("historical abandoned intents must not silently execute during upgrade: %+v %v", pending, err)
	}
}

func TestReclaimCannotCrossLeaseBoundary(t *testing.T) {
	persistence := newStore(t)
	ctx := context.Background()
	seedIdle(t, persistence, "node")
	old, _ := persistence.GetInstance(ctx, "node")
	newer := old
	newer.LeaseID = "replacement-lease"
	newer.LeaseUpdatedAt = time.Now().UTC()
	if err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: old.ID, Next: newer}); err != nil {
		t.Fatal(err)
	}
	err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: old.ID, RequestRelease: true,
		ExpectPartnerID: old.PartnerID, ExpectLeaseID: &old.LeaseID,
		Next: domain.Instance{DrainDeadlineAt: time.Now().Add(time.Minute)}})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale revoke crossed lease boundary: %v", err)
	}
	row, _ := persistence.GetInstance(ctx, old.ID)
	if row.PendingRelease {
		t.Fatal("replacement lease marked for reclaim")
	}
}

func TestStaleIdentityWriteCannotCrossReclaimIntent(t *testing.T) {
	persistence := newStore(t)
	ctx := context.Background()
	seedIdle(t, persistence, "node")
	stale, _ := persistence.GetInstance(ctx, "node")
	if err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: stale.ID, RequestRelease: true,
		Next: domain.Instance{DrainDeadlineAt: time.Now().Add(time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	stale.Endpoint, stale.LeaseID = "http://replacement:9001", "replacement"
	err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: stale.ID, Next: stale})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale update crossed pending reclaim: %v", err)
	}
}

func TestStaleRevokeCannotCancelNewerQueuedIdentity(t *testing.T) {
	persistence := newStore(t)
	ctx := context.Background()
	seedServing(t, persistence, "node", domain.RolePrefill)
	row, _ := persistence.GetInstance(ctx, "node")
	observed := time.Now().UTC()
	row.PendingUpdate = &domain.InstanceUpdate{Endpoint: "http://replacement:9001", LeaseID: "new", ObservedAt: observed.Add(time.Second)}
	if err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: row.ID, Next: row}); err != nil {
		t.Fatal(err)
	}
	err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: row.ID, RequestRelease: true,
		ReleaseObservedAt: observed, Next: domain.Instance{DrainDeadlineAt: observed.Add(time.Minute)}})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale revoke erased newer pending identity: %v", err)
	}
	latest, _ := persistence.GetInstance(ctx, row.ID)
	if latest.PendingRelease || latest.PendingUpdate == nil {
		t.Fatal("stale revoke changed queued intent")
	}
}
