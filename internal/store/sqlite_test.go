package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/store"
)

func newStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tai-talea.db")
	persistence, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { persistence.Close() })
	return persistence
}

func TestClaimEventIsIdempotent(t *testing.T) {
	persistence := newStore(t)
	ctx := context.Background()
	event := domain.CapacityEvent{
		EventID:    "partner-a:evt-1",
		PartnerID:  "partner-a",
		Type:       domain.EventCapacityAdded,
		OccurredAt: time.Now().UTC(),
		Instance:   &domain.EventInstance{ID: "container-1", Endpoint: "http://10.0.0.1:8080"},
		Source:     domain.SourcePush,
		ReceivedAt: time.Now().UTC(),
	}
	claimed, err := persistence.ClaimEvent(ctx, event, domain.ResultPending, "")
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !claimed {
		t.Fatal("the first claim must succeed")
	}
	claimed, err = persistence.ClaimEvent(ctx, event, domain.ResultPending, "")
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimed {
		t.Fatal("a duplicate event id must not be claimable twice")
	}

	record, err := persistence.GetEvent(ctx, event.EventID)
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	if record.Result != domain.ResultPending {
		t.Fatalf("result=%s, want PENDING", record.Result)
	}
	if record.Source != domain.SourcePush {
		t.Fatalf("source=%s, want push", record.Source)
	}

	if err := persistence.CompleteEvent(ctx, event.EventID, domain.ResultApplied, "prepared"); err != nil {
		t.Fatalf("complete event: %v", err)
	}
	record, err = persistence.GetEvent(ctx, event.EventID)
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	if record.Result != domain.ResultApplied || record.Reason != "prepared" {
		t.Fatalf("unexpected completed record: %+v", record)
	}
}

func TestApplyTransitionEnforcesTheDualLayerRules(t *testing.T) {
	persistence := newStore(t)
	ctx := context.Background()
	instance := domain.Instance{
		ID:             "container-1",
		PartnerID:      "partner-a",
		Endpoint:       "http://10.0.0.1:8080",
		LeaseID:        "lease-1",
		InstanceState:  domain.InstanceAllocating,
		ServiceState:   domain.ServiceNone,
		LeaseUpdatedAt: time.Now().UTC(),
	}

	// INSERT path accepts ALLOCATING only.
	if err := persistence.ApplyTransition(ctx, store.Transition{
		InstanceID: instance.ID,
		Next:       domain.Instance{InstanceState: domain.InstanceIdle, ServiceState: domain.ServiceNone},
	}); err == nil {
		t.Fatal("creating a row that is not ALLOCATING must fail")
	}
	if err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: instance.ID, Next: instance}); err != nil {
		t.Fatalf("create ALLOCATING row: %v", err)
	}

	// ALLOCATING -> PREPARING is legal.
	preparing := instance
	preparing.InstanceState = domain.InstancePreparing
	if err := persistence.ApplyTransition(ctx, store.Transition{
		InstanceID:          instance.ID,
		Next:                preparing,
		ExpectInstanceState: domain.InstanceAllocating,
	}); err != nil {
		t.Fatalf("ALLOCATING -> PREPARING: %v", err)
	}

	// PREPARING + STARTING is an illegal combination even though both states exist.
	illegal := preparing
	illegal.ServiceState = domain.ServiceStarting
	illegal.Role = domain.RolePrefill
	err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: instance.ID, Next: illegal})
	if err == nil {
		t.Fatal("PREPARING + STARTING must be refused")
	}
	var violation *domain.Violation
	if !errors.As(err, &violation) || violation.Code != domain.CodeIllegalCombination {
		t.Fatalf("expected an illegal_combination violation, got %v", err)
	}

	// PREPARING -> IDLE then a legal service start.
	idle := preparing
	idle.InstanceState = domain.InstanceIdle
	if err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: instance.ID, Next: idle}); err != nil {
		t.Fatalf("PREPARING -> IDLE: %v", err)
	}
	serving := idle
	serving.ServiceState = domain.ServiceServing
	serving.Role = domain.RolePrefill
	if err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: instance.ID, Next: serving}); err == nil {
		t.Fatal("NONE -> SERVING must be refused because it skips the start pipeline")
	}

	stored, err := persistence.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if stored.InstanceState != domain.InstanceIdle || stored.ServiceState != domain.ServiceNone {
		t.Fatalf("refused transitions must not be persisted, got %s/%s", stored.InstanceState, stored.ServiceState)
	}
}

func TestApplyTransitionCompareAndSwap(t *testing.T) {
	persistence := newStore(t)
	ctx := context.Background()
	instance := domain.Instance{
		ID: "container-1", PartnerID: "partner-a", Endpoint: "http://10.0.0.1:8080",
		LeaseID: "lease-1", InstanceState: domain.InstanceAllocating, ServiceState: domain.ServiceNone,
	}
	if err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: instance.ID, Next: instance}); err != nil {
		t.Fatalf("create: %v", err)
	}
	preparing := instance
	preparing.InstanceState = domain.InstancePreparing
	if err := persistence.ApplyTransition(ctx, store.Transition{
		InstanceID:          instance.ID,
		Next:                preparing,
		ExpectInstanceState: domain.InstanceAllocating,
	}); err != nil {
		t.Fatalf("first transition: %v", err)
	}
	// The same expectation now fails because another writer already moved the row.
	idle := preparing
	idle.InstanceState = domain.InstanceIdle
	err := persistence.ApplyTransition(ctx, store.Transition{
		InstanceID:          instance.ID,
		Next:                idle,
		ExpectInstanceState: domain.InstanceAllocating,
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale expectation must return ErrConflict, got %v", err)
	}
}

func TestOperationsAndAuditCommitWithTheTransition(t *testing.T) {
	persistence := newStore(t)
	ctx := context.Background()
	instance := domain.Instance{
		ID: "container-1", PartnerID: "partner-a", Endpoint: "http://10.0.0.1:8080",
		LeaseID: "lease-1", InstanceState: domain.InstanceAllocating, ServiceState: domain.ServiceNone,
	}
	operation := &store.Operation{
		OperationID: "op-1", InstanceID: instance.ID, Type: store.OpPrepare,
		Status: store.OpPending, CreatedAt: time.Now().UTC(),
	}
	if err := persistence.ApplyTransition(ctx, store.Transition{
		InstanceID: instance.ID,
		Next:       instance,
		Operation:  operation,
		Audit: store.AuditEntry{
			Action: "capacity_added", InstanceID: instance.ID, PartnerID: "partner-a",
			Details: map[string]string{"endpoint": instance.Endpoint},
		},
	}); err != nil {
		t.Fatalf("apply transition: %v", err)
	}

	open, err := persistence.ListOpenOperations(ctx, 10)
	if err != nil {
		t.Fatalf("list operations: %v", err)
	}
	if len(open) != 1 || open[0].OperationID != "op-1" || open[0].Status != store.OpPending {
		t.Fatalf("unexpected open operations: %+v", open)
	}
	if err := persistence.CompleteOperation(ctx, "op-1", store.OpSucceeded, "", time.Time{}); err != nil {
		t.Fatalf("complete operation: %v", err)
	}
	open, err = persistence.ListOpenOperations(ctx, 10)
	if err != nil {
		t.Fatalf("list operations: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("a succeeded operation must not be resumable, got %+v", open)
	}

	entries, err := persistence.ListAudit(ctx, instance.ID, 10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != "capacity_added" {
		t.Fatalf("unexpected audit entries: %+v", entries)
	}
	if entries[0].Details["endpoint"] != instance.Endpoint {
		t.Fatalf("audit details were not preserved: %+v", entries[0].Details)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	persistence := newStore(t)
	ctx := context.Background()
	payload, err := json.Marshal(map[string]any{"instances": []map[string]string{{"id": "a"}}})
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	snapshot := store.SnapshotRecord{
		PartnerID:     "partner-a",
		Version:       "version-1",
		PayloadJSON:   string(payload),
		ObservedAt:    time.Now().UTC(),
		Status:        store.SnapshotStatusOK,
		InstanceCount: 1,
	}
	if err := persistence.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	loaded, found, err := persistence.GetSnapshot(ctx, "partner-a")
	if err != nil || !found {
		t.Fatalf("get snapshot: found=%v err=%v", found, err)
	}
	if loaded.Version != "version-1" || loaded.InstanceCount != 1 {
		t.Fatalf("unexpected snapshot: %+v", loaded)
	}

	// A second save must replace, never accumulate.
	snapshot.Version = "version-2"
	snapshot.InstanceCount = 2
	if err := persistence.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("replace snapshot: %v", err)
	}
	loaded, _, err = persistence.GetSnapshot(ctx, "partner-a")
	if err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if loaded.Version != "version-2" || loaded.InstanceCount != 2 {
		t.Fatalf("snapshot was not replaced: %+v", loaded)
	}

	if _, found, err := persistence.GetSnapshot(ctx, "partner-missing"); err != nil || found {
		t.Fatalf("unknown partner must not report a snapshot: found=%v err=%v", found, err)
	}
}

func TestAbandonStaleEvents(t *testing.T) {
	persistence := newStore(t)
	ctx := context.Background()
	old := domain.CapacityEvent{
		EventID: "old", PartnerID: "partner-a", Type: domain.EventCapacityAdded,
		OccurredAt: time.Now().Add(-time.Hour).UTC(),
		Instance:   &domain.EventInstance{ID: "container-old"},
		Source:     domain.SourcePush,
		ReceivedAt: time.Now().Add(-time.Hour).UTC(),
	}
	if _, err := persistence.ClaimEvent(ctx, old, domain.ResultPending, ""); err != nil {
		t.Fatalf("claim: %v", err)
	}
	fresh := old
	fresh.EventID = "fresh"
	fresh.ReceivedAt = time.Now().UTC()
	if _, err := persistence.ClaimEvent(ctx, fresh, domain.ResultPending, ""); err != nil {
		t.Fatalf("claim: %v", err)
	}

	abandoned, err := persistence.AbandonStaleEvents(ctx, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if abandoned != 1 {
		t.Fatalf("abandoned=%d, want 1", abandoned)
	}
	record, err := persistence.GetEvent(ctx, "old")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if record.Result != domain.ResultAbandoned {
		t.Fatalf("result=%s, want ABANDONED", record.Result)
	}
	record, err = persistence.GetEvent(ctx, "fresh")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if record.Result != domain.ResultPending {
		t.Fatalf("a fresh event must stay pending, got %s", record.Result)
	}
}

func TestCountsExcludeReleasedAndIdleServices(t *testing.T) {
	persistence := newStore(t)
	ctx := context.Background()

	seedReleased(t, persistence, "released")
	seedServing(t, persistence, "serving", domain.RolePrefill)
	seedIdle(t, persistence, "idle")

	instances, err := persistence.CountInstancesByState(ctx)
	if err != nil {
		t.Fatalf("count instances: %v", err)
	}
	total := 0
	for _, count := range instances {
		total += count.Count
	}
	if total != 2 {
		t.Fatalf("released instances must be excluded, got total=%d (%+v)", total, instances)
	}
	services, err := persistence.CountServicesByRoleState(ctx)
	if err != nil {
		t.Fatalf("count services: %v", err)
	}
	if len(services) != 1 || services[0].Role != "prefill" || services[0].State != string(domain.ServiceServing) {
		t.Fatalf("unexpected service counts: %+v", services)
	}
}

// newInstanceRow walks a fresh container from ALLOCATING to IDLE, which is the
// only legal entry into the control plane.
func newInstanceRow(t *testing.T, persistence *store.SQLiteStore, id string) domain.Instance {
	t.Helper()
	ctx := context.Background()
	base := domain.Instance{
		ID: id, PartnerID: "partner-a", Endpoint: "http://" + id + ":8080", LeaseID: "lease-" + id,
		InstanceState: domain.InstanceAllocating, ServiceState: domain.ServiceNone,
		LeaseUpdatedAt: time.Now().UTC(),
	}
	if err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: id, Next: base}); err != nil {
		t.Fatalf("seed %s ALLOCATING: %v", id, err)
	}
	preparing := base
	preparing.InstanceState = domain.InstancePreparing
	if err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: id, Next: preparing}); err != nil {
		t.Fatalf("seed %s PREPARING: %v", id, err)
	}
	idle := preparing
	idle.InstanceState = domain.InstanceIdle
	if err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: id, Next: idle}); err != nil {
		t.Fatalf("seed %s IDLE: %v", id, err)
	}
	return idle
}

func seedIdle(t *testing.T, persistence *store.SQLiteStore, id string) {
	t.Helper()
	newInstanceRow(t, persistence, id)
}

func seedServing(t *testing.T, persistence *store.SQLiteStore, id string, role domain.Role) {
	t.Helper()
	ctx := context.Background()
	instance := newInstanceRow(t, persistence, id)
	instance.Role = role
	for _, state := range []domain.ServiceState{
		domain.ServiceStarting, domain.ServiceHealthy, domain.ServiceRegistering, domain.ServiceServing,
	} {
		instance.ServiceState = state
		if err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: id, Next: instance}); err != nil {
			t.Fatalf("seed %s service %s: %v", id, state, err)
		}
	}
}

func seedReleased(t *testing.T, persistence *store.SQLiteStore, id string) {
	t.Helper()
	ctx := context.Background()
	instance := newInstanceRow(t, persistence, id)
	instance.InstanceState = domain.InstanceReleasing
	if err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: id, Next: instance}); err != nil {
		t.Fatalf("seed %s RELEASING: %v", id, err)
	}
	instance.InstanceState = domain.InstanceReleased
	if err := persistence.ApplyTransition(ctx, store.Transition{InstanceID: id, Next: instance}); err != nil {
		t.Fatalf("seed %s RELEASED: %v", id, err)
	}
}
