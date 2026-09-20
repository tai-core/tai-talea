package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/config"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/store"
)

func TestPersistRestartIdempotencyAndCredentialErasure(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	calls := 0
	runner := func(ctx context.Context, r Request, progress func(string) error) (domain.Instance, error) {
		calls++
		if r.Password != "secret-password" {
			t.Fatal("lost credentials")
		}
		if err := progress("安装依赖"); err != nil {
			return domain.Instance{}, err
		}
		return domain.Instance{Endpoint: "http://10.0.0.1:9001", ServiceEndpoint: "http://10.0.0.1:9002"}, nil
	}
	var received domain.Instance
	enroll := func(_ context.Context, i domain.Instance) error { received = i; return nil }
	m, err := New(filepath.Join(dir, "jobs"), db, runner, enroll)
	if err != nil {
		t.Fatal(err)
	}
	r := Request{RequestID: "request-1", ID: "node-1", PartnerID: "partner-a", Host: "10.0.0.1", Port: 22, User: "root", Password: "secret-password"}
	job, err := m.Submit(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Submit(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(job)
	if strings.Contains(string(encoded), r.Password) {
		t.Fatal("API exposes password")
	}
	path := filepath.Join(dir, "jobs", "job-request-1.json")
	disk, _ := os.ReadFile(path)
	if strings.Contains(string(disk), r.Password) {
		t.Fatal("password stored in plaintext")
	}
	conflict := r
	conflict.PartnerID = "partner-b"
	if _, err = m.Submit(t.Context(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatal("cross-partner request collision accepted")
	}
	conflict = r
	conflict.RequestID = "request-2"
	if _, err = m.Submit(t.Context(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatal("duplicate active node accepted")
	}
	m, err = New(filepath.Join(dir, "jobs"), db, runner, enroll)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if received.ID != r.ID || received.PartnerID != r.PartnerID || received.LeaseID != "ssh-request-1" {
		t.Fatalf("wrong enrollment: %+v", received)
	}
	if jobs := m.List("partner-a"); len(jobs) != 1 || jobs[0].State != "MANAGED" || jobs[0].LeaseID != received.LeaseID {
		t.Fatalf("jobs: %+v", jobs)
	}
	if len(m.List("partner-b")) != 0 {
		t.Fatal("job isolation failed")
	}
	disk, _ = os.ReadFile(path)
	var rec record
	_ = json.Unmarshal(disk, &rec)
	if len(rec.Secret) != 0 {
		t.Fatal("credentials retained after installation")
	}
	if err = m.RunOnce(t.Context()); err != nil || calls != 1 {
		t.Fatal("completed job installed twice")
	}
	// Existing records contain the lease in Result, not Job. Preserve that
	// identity across restarts and a later successful job using the same name.
	m, err = New(filepath.Join(dir, "jobs"), db, runner, enroll)
	if err != nil {
		t.Fatal(err)
	}
	r.RequestID = "request-2"
	if _, err = m.Submit(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if err = m.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	leases := map[string]string{}
	for _, job := range m.List("partner-a") {
		leases[job.JobID] = job.LeaseID
	}
	if leases["request-1"] != "ssh-request-1" || leases["request-2"] != "ssh-request-2" {
		t.Fatalf("same-name job history lost lease identity: %v", leases)
	}
}

func TestRestartBetweenProvisioningAndEnrollmentDoesNotReinstall(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	run := func(context.Context, Request, func(string) error) (domain.Instance, error) {
		calls++
		return domain.Instance{Endpoint: "http://10.0.0.1:9001"}, nil
	}
	m, err := New(filepath.Join(dir, "jobs"), db, run, func(context.Context, domain.Instance) error { cancel(); return context.Canceled })
	if err != nil {
		t.Fatal(err)
	}
	r := Request{RequestID: "resume", ID: "node", PartnerID: "a", Host: "10.0.0.1", Port: 22, User: "root", Password: "password"}
	if _, err = m.Submit(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err = m.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	m, err = New(filepath.Join(dir, "jobs"), db, run, func(context.Context, domain.Instance) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = m.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || m.List("")[0].State != "MANAGED" {
		t.Fatal("restart repeated installation")
	}
}

func TestFailureClearsCredentialsAndAllowsExplicitRetry(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := New(filepath.Join(dir, "jobs"), db, func(context.Context, Request, func(string) error) (domain.Instance, error) {
		return domain.Instance{}, errors.New("SSH 认证失败")
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := Request{RequestID: "failed", ID: "node", PartnerID: "a", Host: "host.example", Port: 22, User: "root", Password: "wrong"}
	if _, err = m.Submit(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if err = m.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.jobs["failed"].Job.State != "FAILED" || len(m.jobs["failed"].Secret) != 0 {
		t.Fatal("terminal credentials not erased")
	}
	r.RequestID = "retry"
	r.Password = "correct"
	if _, err = m.Submit(t.Context(), r); err != nil {
		t.Fatal(err)
	}
}

func TestJobDeadlineClearsCredentialsAndUnblocksQueue(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	calls := 0
	m, err := New(filepath.Join(dir, "jobs"), db, func(ctx context.Context, _ Request, _ func(string) error) (domain.Instance, error) {
		calls++
		if calls == 1 {
			<-ctx.Done()
			return domain.Instance{}, ctx.Err()
		}
		return domain.Instance{Endpoint: "http://10.0.0.2:9001"}, nil
	}, func(context.Context, domain.Instance) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"slow", "next"} {
		_, err = m.Submit(t.Context(), Request{RequestID: id, ID: id, PartnerID: "a", Host: id + ".example", Port: 22, User: "root", Password: "secret"})
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err = m.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	failed := m.jobs["slow"]
	if failed.Job.State != "FAILED" || len(failed.Secret) != 0 || !strings.Contains(failed.Job.Error, "超时") {
		t.Fatalf("deadline was not terminal: %+v", failed.Job)
	}
	if err = m.RunOnce(t.Context()); err != nil || m.jobs["next"].Job.State != "MANAGED" || calls != 2 {
		t.Fatalf("queue did not advance: calls=%d err=%v", calls, err)
	}
}

func TestProcessRunnerRejectsOversizedOutputWithoutHanging(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	runner := ProcessRunner(config.OnboardingConfig{Python: executable, Script: "-test.run=^TestInstallerOutputHelper$"}, "token")
	start := time.Now()
	_, err = runner(ctx, Request{Host: "output-helper"}, func(string) error { return nil })
	if err == nil || ctx.Err() != nil || time.Since(start) > 5*time.Second {
		t.Fatalf("oversized installer output did not fail promptly: %v", err)
	}
}

func TestInstallerOutputHelper(t *testing.T) {
	// Child invocation has only the helper selected; regular test runs skip it.
	if len(os.Args) != 2 || os.Args[1] != "-test.run=^TestInstallerOutputHelper$" {
		return
	}
	_, _ = os.Stdout.WriteString(strings.Repeat("x", 128*1024))
	time.Sleep(30 * time.Second)
	os.Exit(0)
}
