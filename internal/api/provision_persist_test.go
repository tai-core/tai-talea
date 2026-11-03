package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/config"
	"github.com/tai-core/tai-talea/internal/partner"
	"github.com/tai-core/tai-talea/internal/store"
)

// persistedServer wires a server whose store is a real SQLite database and
// whose partner lookup returns the given fake. The tracker global is pointed
// at the store, which is what api.New does for a SQLite-backed deployment.
func persistedServer(t *testing.T, provisioner partner.PartnerAdapter) (*Server, *store.SQLiteStore) {
	t.Helper()
	resetProvisions(t)
	persistence, err := store.OpenSQLite(filepath.Join(t.TempDir(), "tai-talea.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { persistence.Close() })
	provisionJobs.mu.Lock()
	provisionJobs.persist = persistence
	provisionJobs.mu.Unlock()
	t.Cleanup(func() {
		provisionJobs.mu.Lock()
		provisionJobs.persist = nil
		provisionJobs.mu.Unlock()
	})
	// now must be set explicitly: the handler chain (logRequests) uses it,
	// and only api.New installs the default.
	server := &Server{
		resolveAdapter: func(string) (partner.PartnerAdapter, bool) {
			return provisioner, true
		},
		now: func() time.Time { return time.Now().UTC() },
	}
	return server, persistence
}

// persistedRow reads one job row straight from the database, bypassing the
// in-memory tracker: the point of these tests is that the row is what a
// restart would see.
func persistedRow(t *testing.T, persistence *store.SQLiteStore, name string) store.ProvisionJobRecord {
	t.Helper()
	jobs, err := persistence.ListProvisionJobs(context.Background())
	if err != nil {
		t.Fatalf("list provision jobs: %v", err)
	}
	for _, job := range jobs {
		if job.Name == name {
			return job
		}
	}
	t.Fatalf("provision job %s was not persisted", name)
	return store.ProvisionJobRecord{}
}

// waitFor polls until the condition holds, failing with the last state.
func waitFor(t *testing.T, what string, timeout time.Duration, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Every step the pipeline takes must land in the durable row, because the row
// is what tells a restarted process where the platform request stands. The
// hold path is where this matters most: a queued request outlives the process
// that made it.
func TestProvisionJobPersistsEveryStep(t *testing.T) {
	release := make(chan struct{})
	provisioner := &fakeProvisioner{
		await: func(ctx context.Context, instanceID string) (partner.SSHAccess, error) {
			select {
			case <-release:
				return partner.SSHAccess{}, fmt.Errorf("still QUEUED: %w", partner.ErrAwaitingCapacity)
			case <-ctx.Done():
				return partner.SSHAccess{}, ctx.Err()
			}
		},
	}
	server, persistence := persistedServer(t, provisioner)
	// The direct pipeline call mirrors runProvision's create → wait legs with
	// the default fakes (key "CCI:SUBSCRIBE:key", instance "inst-1").
	queueJob(t, "glm53-persist")
	go server.runProvision(context.Background(), provisioner, "alayanew", "glm53-persist")

	waitFor(t, "the row to reach WAITING_SSH", 2*time.Second, func() bool {
		record, ok := provisionJobs.snapshot("glm53-persist")
		return ok && record.State == "WAITING_SSH"
	})

	row := persistedRow(t, persistence, "glm53-persist")
	if row.CreationKey != "CCI:SUBSCRIBE:key" {
		t.Errorf("the creation key must be persisted the moment it is returned, got %q", row.CreationKey)
	}
	if row.InstanceID != "inst-1" {
		t.Errorf("the instance id must be persisted, got %q", row.InstanceID)
	}
	if row.State != "WAITING_SSH" {
		t.Errorf("state = %s", row.State)
	}

	// The hold lands in the row too, so a restart knows the request is queued
	// rather than failed.
	close(release)
	waitFor(t, "the hold to reach the row", 2*time.Second, func() bool {
		row = persistedRow(t, persistence, "glm53-persist")
		return row.State == "QUEUED" && strings.Contains(row.LastError, "QUEUED")
	})
	if got := OutstandingProvisions(); got != 1 {
		t.Fatalf("a held job is still outstanding, got %d", got)
	}
}

// A queued request that outlives the process must keep being followed: the
// restart re-arms the SSH wait with the persisted instance id, so capacity
// arriving hours later still completes the creation instead of leaving a
// silent outstanding row forever.
func TestRestartResumesAQueuedJobAtSSHAccess(t *testing.T) {
	awaitEntered := make(chan struct{})
	release := make(chan struct{})
	provisioner := &fakeProvisioner{
		await: func(ctx context.Context, instanceID string) (partner.SSHAccess, error) {
			if instanceID != "inst-9" {
				t.Errorf("resume must wait on the persisted instance id, got %q", instanceID)
			}
			close(awaitEntered)
			select {
			case <-release:
				return partner.SSHAccess{}, fmt.Errorf("still QUEUED: %w", partner.ErrAwaitingCapacity)
			case <-ctx.Done():
				return partner.SSHAccess{}, ctx.Err()
			}
		},
	}
	server, persistence := persistedServer(t, provisioner)
	now := time.Now().UTC().Add(-time.Hour)
	if err := persistence.SaveProvisionJob(context.Background(), store.ProvisionJobRecord{
		Name: "glm53-queued", PartnerID: "alayanew", Path: "ssh",
		State: "QUEUED", InstanceID: "inst-9",
		Stage: "平台已受理创建请求，仍在排队等待算力（未占用容器、不计费；保留队列位次）",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed the row: %v", err)
	}

	server.ResumeProvisions()

	select {
	case <-awaitEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("the resumed job never reached AwaitSSHAccess")
	}
	close(release)
	waitFor(t, "the resumed job to hold again", 2*time.Second, func() bool {
		record, ok := provisionJobs.snapshot("glm53-queued")
		return ok && record.State == "QUEUED" && strings.Contains(record.Error, "QUEUED")
	})
	if got := OutstandingProvisions(); got != 1 {
		t.Fatalf("a resumed queued job is still outstanding, got %d", got)
	}
}

// A row without a creation key is one whose create call may never have been
// submitted, or whose key was lost in the crash window. The only safe move is
// re-creating under the SAME name: a fresh name would allocate a second
// container while the first request may still exist.
func TestRestartRecreatesUnderTheSameNameWhenTheKeyWasLost(t *testing.T) {
	var created []string
	provisioner := &fakeProvisioner{
		create: func(_ context.Context, name string) (string, error) {
			created = append(created, name)
			return "CCI:SUBSCRIBE:key-2", nil
		},
		await: func(context.Context, string) (partner.SSHAccess, error) {
			return partner.SSHAccess{}, fmt.Errorf("still QUEUED: %w", partner.ErrAwaitingCapacity)
		},
	}
	server, persistence := persistedServer(t, provisioner)
	now := time.Now().UTC().Add(-time.Hour)
	if err := persistence.SaveProvisionJob(context.Background(), store.ProvisionJobRecord{
		Name: "glm53-lost", PartnerID: "alayanew", Path: "ssh",
		State: "CREATING", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed the row: %v", err)
	}

	server.ResumeProvisions()

	waitFor(t, "the recreated request to hold", 2*time.Second, func() bool {
		row := persistedRow(t, persistence, "glm53-lost")
		return row.State == "QUEUED" && row.CreationKey == "CCI:SUBSCRIBE:key-2"
	})
	if len(created) != 1 || created[0] != "glm53-lost" {
		t.Fatalf("the recreate must reuse the persisted name, got %v", created)
	}
}

// A name-in-use refusal on the recreate path is not a free rename: the first
// request probably exists on the platform, so stepping to a fresh name would
// allocate a second container on top of it. The job fails with operator
// guidance instead.
func TestRestartWithALostKeyAndATakenNameFailsWithoutRenaming(t *testing.T) {
	var created []string
	provisioner := &fakeProvisioner{
		create: func(_ context.Context, name string) (string, error) {
			created = append(created, name)
			return "", fmt.Errorf("create instance %s: %w: alayanew api status 212035", name, partner.ErrNameInUse)
		},
	}
	server, persistence := persistedServer(t, provisioner)
	now := time.Now().UTC().Add(-time.Hour)
	if err := persistence.SaveProvisionJob(context.Background(), store.ProvisionJobRecord{
		Name: "glm53-ghost", PartnerID: "alayanew", Path: "ssh",
		State: "CREATING", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed the row: %v", err)
	}

	server.ResumeProvisions()

	waitFor(t, "the job to fail with guidance", 2*time.Second, func() bool {
		row := persistedRow(t, persistence, "glm53-ghost")
		return row.State == "FAILED"
	})
	row := persistedRow(t, persistence, "glm53-ghost")
	if !strings.Contains(row.LastError, "核对") {
		t.Errorf("the failure must tell the operator what to check, got %q", row.LastError)
	}
	if len(created) != 1 || created[0] != "glm53-ghost" {
		t.Fatalf("no rename ladder may run here, got %v", created)
	}
}

// Terminal rows are history, not work: they are loaded back into the tracker
// (the reconciliation record of what was asked for) but never re-armed.
func TestRestartLeavesTerminalRowsAlone(t *testing.T) {
	provisioner := &fakeProvisioner{
		create: func(_ context.Context, name string) (string, error) {
			t.Errorf("a MANAGED job must not be re-created, got create(%q)", name)
			return "", errors.New("unreachable")
		},
	}
	server, persistence := persistedServer(t, provisioner)
	now := time.Now().UTC().Add(-time.Hour)
	for name, state := range map[string]string{"glm53-done": "MANAGED", "glm53-dead": "FAILED"} {
		if err := persistence.SaveProvisionJob(context.Background(), store.ProvisionJobRecord{
			Name: name, PartnerID: "alayanew", Path: "ssh",
			State: state, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	server.ResumeProvisions()
	time.Sleep(100 * time.Millisecond)

	if got := OutstandingProvisions(); got != 0 {
		t.Fatalf("terminal rows are not outstanding, got %d", got)
	}
	for _, name := range []string{"glm53-done", "glm53-dead"} {
		if record, ok := provisionJobs.snapshot(name); !ok || record.State == "" {
			t.Errorf("terminal row %s must still be visible for reconciliation", name)
		}
	}
}

// Cancelling a job that already holds a container releases the container:
// cancelling is the operator taking responsibility for the request, and
// leaving the container behind would leak billable quota. Cancelling also
// clears the outstanding count, which is the lever a permanently queued
// request needs.
func TestCancelReleasesTheContainerAndClearsTheSlot(t *testing.T) {
	provisioner := &fakeProvisioner{
		await: func(ctx context.Context, instanceID string) (partner.SSHAccess, error) {
			<-ctx.Done()
			return partner.SSHAccess{}, ctx.Err()
		},
	}
	server, persistence := persistedServer(t, provisioner)
	now := time.Now().UTC().Add(-time.Hour)
	if err := persistence.SaveProvisionJob(context.Background(), store.ProvisionJobRecord{
		Name: "glm53-stop", PartnerID: "alayanew", Path: "ssh",
		State: "WAITING_SSH", InstanceID: "inst-7", CreationKey: "CCI:SUBSCRIBE:key-7",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed the row: %v", err)
	}
	server.ResumeProvisions()
	waitFor(t, "the resumed job to reach the SSH wait", 2*time.Second, func() bool {
		record, ok := provisionJobs.snapshot("glm53-stop")
		return ok && record.State == "WAITING_SSH"
	})

	server.opts.Config = config.Default()
	server.opts.Config.Server.AdminToken = "admin-token"
	// Through the real handler chain: PathValue only populates when the
	// request is routed by the mux, which is also what a live call does.
	handler := server.Handler()
	request := httptest.NewRequest(http.MethodDelete, "/v1/capacity/console/provision/glm53-stop", nil)
	request.Header.Set("Authorization", "Bearer admin-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("cancel status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	waitFor(t, "the cancel to reach the row", 2*time.Second, func() bool {
		row := persistedRow(t, persistence, "glm53-stop")
		return row.State == "CANCELLED"
	})
	if len(provisioner.released) != 1 || provisioner.released[0] != "inst-7" {
		t.Fatalf("the container must be released on cancel, released=%v", provisioner.released)
	}
	if got := OutstandingProvisions(); got != 0 {
		t.Fatalf("a cancelled job is not outstanding, got %d", got)
	}

	// A second cancel of a finished job is a conflict, not a silent success:
	// it almost always means the caller is acting on stale state.
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("second cancel status = %d", recorder.Code)
	}
}

// The cancel entry on a request that is only queued releases nothing: there
// is no container, and giving up the queue position is exactly what the
// operator asked for.
func TestCancelOfAQueuedJobReleasesNothing(t *testing.T) {
	provisioner := &fakeProvisioner{}
	server, _ := persistedServer(t, provisioner)
	now := time.Now().UTC()
	provisionJobs.add(ProvisionRecord{
		Name: "glm53-inqueue", PartnerID: "alayanew", State: "QUEUED", Path: "ssh",
		CreatedAt: now, UpdatedAt: now,
	})

	server.opts.Config = config.Default()
	server.opts.Config.Server.AdminToken = "admin-token"
	request := httptest.NewRequest(http.MethodDelete, "/v1/capacity/console/provision/glm53-inqueue", nil)
	request.Header.Set("Authorization", "Bearer admin-token")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("cancel status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	record, _ := provisionJobs.snapshot("glm53-inqueue")
	if record.State != "CANCELLED" {
		t.Fatalf("state = %s", record.State)
	}
	if len(provisioner.released) != 0 {
		t.Fatalf("a queued request owns nothing to release, released=%v", provisioner.released)
	}
	if got := OutstandingProvisions(); got != 0 {
		t.Fatalf("outstanding = %d after cancel", got)
	}
}

// The persisted-history seeding must not loosen the name-in-use guard: a name
// the control plane already manages stays refused after a restart, which is
// what stops a re-request from creating a second copy of a live container.
func TestRequestCapacityRefusesANameTheHistoryHolds(t *testing.T) {
	provisioner := &fakeProvisioner{}
	server, persistence := persistedServer(t, provisioner)
	now := time.Now().UTC().Add(-2 * time.Hour)
	if err := persistence.SaveProvisionJob(context.Background(), store.ProvisionJobRecord{
		Name: "glm53-live", PartnerID: "alayanew", Path: "ssh",
		State: "MANAGED", InstanceID: "inst-1", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed the row: %v", err)
	}

	server.ResumeProvisions()

	server.opts.Config = config.Default()
	_, err := server.RequestCapacity(context.Background(), "alayanew", "glm53-live")
	if !errors.Is(err, errProvisionNameInUse) {
		t.Fatalf("a managed name must stay in use after a restart, got %v", err)
	}
}
