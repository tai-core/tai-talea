package instancemanager_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/instancemanager"
	"github.com/tai-core/tai-talea/internal/launcher"
	"github.com/tai-core/tai-talea/internal/obs"
)

const bootstrapVersion = "tai-talea-bootstrap/1"

func testProfile() instancemanager.Profile {
	return instancemanager.Profile{
		OS: "ubuntu-22.04", CUDA: "12.4", Python: "3.11", SGLang: "0.4.x",
		Wheelhouse: "/opt/tai-talea/wheelhouse", Virtualenv: "/opt/tai-talea/venv",
		BootstrapVersion: bootstrapVersion, ModelID: "model-a", MaxLeaseAge: 10 * time.Minute,
	}
}

func goodEnvironment() launcher.Environment {
	return launcher.Environment{
		OS: "ubuntu-22.04", CUDA: "12.4", Python: "3.11", SGLang: "0.4.6",
		Wheelhouse: "/opt/tai-talea/wheelhouse", Virtualenv: "/opt/tai-talea/venv",
	}
}

type bootstrapStub struct {
	health launcher.Health
	status launcher.Status
	fail   int
	server *httptest.Server
}

func newBootstrapStub(t *testing.T) *bootstrapStub {
	t.Helper()
	stub := &bootstrapStub{
		health: launcher.Health{Status: "ok", Phase: launcher.PhaseIdle, Version: bootstrapVersion},
		status: launcher.Status{Phase: launcher.PhaseIdle, Environment: goodEnvironment()},
	}
	mux := http.NewServeMux()
	mux.HandleFunc(launcher.PathHealth, func(writer http.ResponseWriter, _ *http.Request) {
		if stub.fail > 0 {
			writer.WriteHeader(stub.fail)
			return
		}
		writeJSON(writer, stub.health)
	})
	mux.HandleFunc(launcher.PathStatus, func(writer http.ResponseWriter, _ *http.Request) {
		if stub.fail > 0 {
			writer.WriteHeader(stub.fail)
			return
		}
		writeJSON(writer, stub.status)
	})
	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

func writeJSON(writer http.ResponseWriter, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(payload)
}

func newManager(t *testing.T, profile instancemanager.Profile) *instancemanager.Manager {
	t.Helper()
	client, err := launcher.New(3 * time.Second)
	if err != nil {
		t.Fatalf("launcher: %v", err)
	}
	manager, err := instancemanager.New(client, profile)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	return manager
}

func instanceFor(endpoint string) domain.Instance {
	return domain.Instance{
		ID: "container-1", PartnerID: "partner-a", Endpoint: endpoint, LeaseID: "lease-1",
		LeaseUpdatedAt: time.Now().UTC(),
		Spec:           domain.InstanceSpec{GPU: "H100", GPUCount: 1, ModelSupport: []string{"model-a"}},
	}
}

func TestVersionMatchesFollowsTheBootstrapRules(t *testing.T) {
	cases := []struct {
		want, got string
		expected  bool
	}{
		{"0.4.x", "0.4.6", true},
		{"0.4.x", "0.4.0", true},
		{"0.4.x", "0.5.0", false},
		{"0.4.6", "0.4.6", true},
		{"0.4.6", "0.4.7", false},
		{"", "anything", true},
		{"ubuntu-22.04", "ubuntu-22.04", true},
		{"UBUNTU-22.04", "ubuntu-22.04", true},
	}
	for _, testCase := range cases {
		if got := instancemanager.VersionMatches(testCase.want, testCase.got); got != testCase.expected {
			t.Errorf("VersionMatches(%q, %q)=%v, want %v", testCase.want, testCase.got, got, testCase.expected)
		}
	}
}

func TestPrepareSucceedsForAMatchingContainer(t *testing.T) {
	stub := newBootstrapStub(t)
	manager := newManager(t, testProfile())
	report, err := manager.Prepare(context.Background(), instanceFor(stub.server.URL))
	if err != nil {
		t.Fatalf("prepare must succeed: %v", err)
	}
	if report.Health.Version != bootstrapVersion {
		t.Fatalf("bootstrap version=%s", report.Health.Version)
	}
	if report.Status.Environment.SGLang != "0.4.6" {
		t.Fatalf("environment=%+v", report.Status.Environment)
	}
	if len(report.Mismatches) != 0 {
		t.Fatalf("unexpected mismatches: %v", report.Mismatches)
	}
}

func TestPrepareRejectsAnUnreachableContainer(t *testing.T) {
	manager := newManager(t, testProfile())
	_, err := manager.Prepare(context.Background(), instanceFor("http://127.0.0.1:1"))
	if !errors.Is(err, instancemanager.ErrEndpointUnreachable) {
		t.Fatalf("expected ErrEndpointUnreachable, got %v", err)
	}
}

func TestPrepareRejectsABootstrapVersionMismatch(t *testing.T) {
	stub := newBootstrapStub(t)
	stub.health.Version = "tai-talea-bootstrap/0"
	manager := newManager(t, testProfile())
	_, err := manager.Prepare(context.Background(), instanceFor(stub.server.URL))
	if !errors.Is(err, instancemanager.ErrBootstrapVersion) {
		t.Fatalf("expected ErrBootstrapVersion, got %v", err)
	}
}

func TestPrepareRejectsAnEnvironmentMismatch(t *testing.T) {
	stub := newBootstrapStub(t)
	stub.status.Environment = launcher.Environment{
		OS: "ubuntu-20.04", CUDA: "11.8", Python: "3.10", SGLang: "0.3.0",
	}
	manager := newManager(t, testProfile())
	report, err := manager.Prepare(context.Background(), instanceFor(stub.server.URL))
	if !errors.Is(err, instancemanager.ErrCompatMismatch) {
		t.Fatalf("expected ErrCompatMismatch, got %v", err)
	}
	if len(report.Mismatches) < 4 {
		t.Fatalf("every mismatching field must be reported, got %v", report.Mismatches)
	}
	versionMismatches := 0
	for _, mismatch := range report.Mismatches {
		if strings.TrimSpace(mismatch) == "" {
			t.Fatal("a mismatch must explain itself")
		}
		if strings.Contains(mismatch, "does not match profile") {
			versionMismatches++
		}
	}
	if versionMismatches < 4 {
		t.Fatalf("os, cuda, python and sglang must all be reported, got %v", report.Mismatches)
	}
}

func TestPrepareRejectsAMissingWheelhouse(t *testing.T) {
	stub := newBootstrapStub(t)
	stub.status.Environment.Wheelhouse = ""
	stub.status.Environment.Virtualenv = ""
	manager := newManager(t, testProfile())
	_, err := manager.Prepare(context.Background(), instanceFor(stub.server.URL))
	if !errors.Is(err, instancemanager.ErrCompatMismatch) {
		t.Fatalf("expected ErrCompatMismatch, got %v", err)
	}
}

func TestPrepareRejectsAModelTheContainerDoesNotSupport(t *testing.T) {
	stub := newBootstrapStub(t)
	manager := newManager(t, testProfile())
	instance := instanceFor(stub.server.URL)
	instance.Spec.ModelSupport = []string{"another-model"}
	_, err := manager.Prepare(context.Background(), instance)
	if !errors.Is(err, instancemanager.ErrCompatMismatch) {
		t.Fatalf("expected ErrCompatMismatch, got %v", err)
	}
}

func TestPrepareRejectsAnInvalidLease(t *testing.T) {
	stub := newBootstrapStub(t)
	manager := newManager(t, testProfile())

	missing := instanceFor(stub.server.URL)
	missing.LeaseID = ""
	if _, err := manager.Prepare(context.Background(), missing); !errors.Is(err, instancemanager.ErrLeaseInvalid) {
		t.Fatalf("a missing lease must be rejected, got %v", err)
	}

	stale := instanceFor(stub.server.URL)
	stale.LeaseUpdatedAt = time.Now().Add(-time.Hour)
	if _, err := manager.Prepare(context.Background(), stale); !errors.Is(err, instancemanager.ErrLeaseInvalid) {
		t.Fatalf("a stale lease observation must be rejected, got %v", err)
	}
}

func TestPrepareRejectsAFailedBootstrap(t *testing.T) {
	stub := newBootstrapStub(t)
	stub.health.Status = "failed"
	manager := newManager(t, testProfile())
	if _, err := manager.Prepare(context.Background(), instanceFor(stub.server.URL)); !errors.Is(err, instancemanager.ErrEndpointUnreachable) {
		t.Fatalf("a failed bootstrap must be rejected, got %v", err)
	}
}

func TestDescribeFailureMapsToAlerts(t *testing.T) {
	cases := []struct {
		err        error
		wantReason string
		wantAlert  string
	}{
		{instancemanager.ErrEndpointUnreachable, "endpoint_unreachable", ""},
		{instancemanager.ErrBootstrapVersion, "bootstrap_version_mismatch", obs.AlertBootstrapFailure},
		{instancemanager.ErrCompatMismatch, "environment_mismatch", obs.AlertEnvironmentMismatch},
		{instancemanager.ErrLeaseInvalid, "lease_invalid", ""},
		{errors.New("something else"), "prepare_failed", ""},
	}
	for _, testCase := range cases {
		reason, details := instancemanager.DescribeFailure(testCase.err)
		if reason != testCase.wantReason {
			t.Errorf("reason=%s, want %s", reason, testCase.wantReason)
		}
		if testCase.wantAlert != "" && details["alert"] != testCase.wantAlert {
			t.Errorf("alert=%s, want %s", details["alert"], testCase.wantAlert)
		}
		if details["error"] == "" {
			t.Error("the failure detail must carry the error text")
		}
	}
}

func TestProfileValidation(t *testing.T) {
	if err := testProfile().Validate(); err != nil {
		t.Fatalf("a complete profile must validate: %v", err)
	}
	incomplete := testProfile()
	incomplete.BootstrapVersion = ""
	if err := incomplete.Validate(); err == nil {
		t.Fatal("a profile without a bootstrap version must be rejected")
	}
	negative := testProfile()
	negative.MaxLeaseAge = -time.Second
	if err := negative.Validate(); err == nil {
		t.Fatal("a negative lease age must be rejected")
	}
}
