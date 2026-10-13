package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/partner"
)

// fakeProvisioner drives the provisioning pipeline one leg at a time with no
// platform behind it. Each leg is a closure so a test states exactly where the
// pipeline stops and what the partner said when it did.
type fakeProvisioner struct {
	create   func(ctx context.Context, name string) (string, error)
	wait     func(ctx context.Context, key string) (string, error)
	await    func(ctx context.Context, instanceID string) (partner.SSHAccess, error)
	released []string
}

func (f *fakeProvisioner) PartnerID() string { return "alayanew" }

func (f *fakeProvisioner) ListAvailableInstances(context.Context) ([]partner.CapacityInstance, error) {
	return nil, nil
}

func (f *fakeProvisioner) ListActiveLeases(context.Context) ([]partner.CapacityInstance, error) {
	return nil, nil
}

func (f *fakeProvisioner) ReleaseInstance(_ context.Context, instanceID string) error {
	f.released = append(f.released, instanceID)
	return nil
}

func (f *fakeProvisioner) CreateInstance(ctx context.Context, name string) (string, error) {
	if f.create != nil {
		return f.create(ctx, name)
	}
	return "CCI:SUBSCRIBE:key", nil
}

func (f *fakeProvisioner) WaitCreation(ctx context.Context, key string) (string, error) {
	if f.wait != nil {
		return f.wait(ctx, key)
	}
	return "inst-1", nil
}

func (f *fakeProvisioner) AwaitSSHAccess(ctx context.Context, instanceID string) (partner.SSHAccess, error) {
	if f.await != nil {
		return f.await(ctx, instanceID)
	}
	return partner.SSHAccess{Host: "10.0.0.1", Port: 22, User: "root", Password: "pw"}, nil
}

// resetProvisions clears the process-wide job tracker. It is a global, so a
// test that leaves entries behind would change what the next one sees.
func resetProvisions(t *testing.T) {
	t.Helper()
	clear := func() {
		provisionJobs.mu.Lock()
		provisionJobs.byName = make(map[string]*ProvisionRecord)
		provisionJobs.mu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func queueJob(t *testing.T, name string) {
	t.Helper()
	now := time.Now().UTC()
	provisionJobs.add(ProvisionRecord{Name: name, PartnerID: "alayanew", State: "QUEUED", CreatedAt: now, UpdatedAt: now})
}

// A request the platform has accepted but not scheduled holds nothing but its
// place in the queue. The wait budget expiring says nothing about whether
// capacity will arrive, so the job must stay outstanding - releasing here is
// what turned a waiting provision into a failed one on 2026-10-13, when
// glm53-auto5 was reclaimed after exactly its 5h30m budget and immediately lost
// the position it had queued 5h30m for.
func TestAQueuedCreationIsHeldInsteadOfReleased(t *testing.T) {
	resetProvisions(t)
	queueJob(t, "glm53-auto5")

	provisioner := &fakeProvisioner{
		await: func(context.Context, string) (partner.SSHAccess, error) {
			return partner.SSHAccess{}, fmt.Errorf("instance inst-1 is still QUEUED: %w", partner.ErrAwaitingCapacity)
		},
	}
	server := &Server{}
	server.runProvision(context.Background(), provisioner, "alayanew", "glm53-auto5")

	record, ok := provisionJobs.snapshot("glm53-auto5")
	if !ok {
		t.Fatal("the job disappeared")
	}
	if record.State != "QUEUED" {
		t.Fatalf("waiting for capacity is not a failure, state=%s error=%s", record.State, record.Error)
	}
	if len(provisioner.released) != 0 {
		t.Fatalf("the queue position must survive the wait budget, released=%v", provisioner.released)
	}
	if !strings.Contains(record.Stage, "保留队列位次") {
		t.Errorf("the stage must say the queue position is kept, got %q", record.Stage)
	}
	if got := OutstandingProvisions(); got != 1 {
		t.Fatalf("a queued request is still outstanding, got %d", got)
	}
}

// The mirror image: a container that exists and never becomes usable is billed,
// so it must still be reclaimed. Otherwise the change above would trade a leak
// of quota for a saved queue position.
func TestAContainerThatNeverComesUpIsStillReleased(t *testing.T) {
	resetProvisions(t)
	queueJob(t, "glm53-slow")

	provisioner := &fakeProvisioner{
		await: func(context.Context, string) (partner.SSHAccess, error) {
			return partner.SSHAccess{}, errors.New("instance inst-1 not RUNNING within 30m (last: CREATING)")
		},
	}
	server := &Server{}
	server.runProvision(context.Background(), provisioner, "alayanew", "glm53-slow")

	record, _ := provisionJobs.snapshot("glm53-slow")
	if record.State != "FAILED" {
		t.Fatalf("a container that never comes up is a failure, state=%s", record.State)
	}
	if len(provisioner.released) != 1 || provisioner.released[0] != "inst-1" {
		t.Fatalf("the unusable container must be reclaimed, released=%v", provisioner.released)
	}
}

// A released container keeps its name reserved on the platform while vanishing
// from every listing, so reusing the name fails on the create call itself.
// Nothing has been allocated at that point, which is what makes stepping to a
// fresh name a free retry instead of a failure to report.
func TestATakenNameIsRetriedUnderAFreshOne(t *testing.T) {
	resetProvisions(t)
	queueJob(t, "glm53-auto4")

	var attempts []string
	provisioner := &fakeProvisioner{
		create: func(_ context.Context, name string) (string, error) {
			attempts = append(attempts, name)
			if len(attempts) == 1 {
				return "", fmt.Errorf("create instance %s: %w: alayanew api status 212035: 本企业内有重复实例名称，请更换", name, partner.ErrNameInUse)
			}
			return "CCI:SUBSCRIBE:key", nil
		},
		await: func(context.Context, string) (partner.SSHAccess, error) {
			return partner.SSHAccess{}, fmt.Errorf("still QUEUED: %w", partner.ErrAwaitingCapacity)
		},
	}
	server := &Server{}
	server.runProvision(context.Background(), provisioner, "alayanew", "glm53-auto4")

	if len(attempts) != 2 || attempts[0] != "glm53-auto4" || attempts[1] != "glm53-auto4-2" {
		t.Fatalf("expected one step up the name ladder, got %v", attempts)
	}
	if _, ok := provisionJobs.snapshot("glm53-auto4"); ok {
		t.Error("the refused name must not keep a job")
	}
	record, ok := provisionJobs.snapshot("glm53-auto4-2")
	if !ok {
		t.Fatal("the job must follow the name the platform accepted")
	}
	if record.InstanceID != "inst-1" {
		t.Errorf("the job must record the container it created, got %q", record.InstanceID)
	}
}

// The ladder is finite on purpose. A name nobody recognises in the platform
// console is worse than an honest failure, and the operator still has the last
// attempted name in front of them.
func TestTheRenameLadderGivesUp(t *testing.T) {
	resetProvisions(t)
	queueJob(t, "glm53-auto4")

	var attempts []string
	provisioner := &fakeProvisioner{
		create: func(_ context.Context, name string) (string, error) {
			attempts = append(attempts, name)
			return "", fmt.Errorf("%w: alayanew api status 212035", partner.ErrNameInUse)
		},
	}
	server := &Server{}
	server.runProvision(context.Background(), provisioner, "alayanew", "glm53-auto4")

	if len(attempts) != maxProvisionNameAttempts {
		t.Fatalf("expected %d attempts, got %v", maxProvisionNameAttempts, attempts)
	}
	last := attempts[len(attempts)-1]
	record, ok := provisionJobs.snapshot(last)
	if !ok || record.State != "FAILED" {
		t.Fatalf("the last attempt must be the one reported, got %+v", record)
	}
	if len(provisioner.released) != 0 {
		t.Fatalf("a creation that never succeeded has nothing to release, released=%v", provisioner.released)
	}
}

// Two jobs under one name would make the tracker lie about which container is
// which, so a rename onto a tracked name must be refused rather than applied.
func TestRenameRefusesToShadowAnotherJob(t *testing.T) {
	resetProvisions(t)
	queueJob(t, "glm53-auto4")
	queueJob(t, "glm53-auto4-2")

	if provisionJobs.rename("glm53-auto4", "glm53-auto4-2") {
		t.Fatal("a rename onto a tracked name must be refused")
	}
	if _, ok := provisionJobs.snapshot("glm53-auto4"); !ok {
		t.Error("a refused rename must leave the source in place")
	}
}
