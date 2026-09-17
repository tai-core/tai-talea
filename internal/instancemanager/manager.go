// Package instancemanager implements the container preparation steps of
// development document §7.1.
//
// Preparation deliberately stays read-only with respect to the container: the
// control plane only calls GET /bootstrap/health and GET /bootstrap/status.
// Working and log directories are created by the bootstrap process itself and
// reported back, because the control plane must never execute arbitrary
// commands inside a partner container (§9 and §15).
package instancemanager

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/launcher"
	"github.com/tai-core/tai-talea/internal/obs"
)

// Preparation failure reasons. They map onto the alert details and the
// operation last_error column.
var (
	// ErrEndpointUnreachable means the container bootstrap did not answer.
	ErrEndpointUnreachable = errors.New("endpoint unreachable")
	// ErrBootstrapVersion means the container bootstrap version is not the one
	// the control plane was validated against.
	ErrBootstrapVersion = errors.New("bootstrap version mismatch")
	// ErrCompatMismatch means the container does not match the controlled image profile.
	ErrCompatMismatch = errors.New("environment compatibility mismatch")
	// ErrLeaseInvalid means the partner lease cannot be trusted.
	ErrLeaseInvalid = errors.New("partner lease is not valid")
)

// Profile is the controlled image contract from §10 (M1 controlled image route).
type Profile struct {
	OS               string
	CUDA             string
	Python           string
	SGLang           string
	Wheelhouse       string
	Virtualenv       string
	BootstrapVersion string
	// ModelID is the model every container must advertise support for.
	ModelID string
	// MaxLeaseAge bounds how old a lease observation may be before the
	// control plane refuses to start work on the container.
	MaxLeaseAge time.Duration
}

// Validate checks the profile.
func (p Profile) Validate() error {
	required := map[string]string{
		"os": p.OS, "cuda": p.CUDA, "python": p.Python, "sglang": p.SGLang,
		"bootstrap_version": p.BootstrapVersion,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required in the controlled image profile", name)
		}
	}
	if p.MaxLeaseAge < 0 {
		return errors.New("max lease age must not be negative")
	}
	return nil
}

// Report is the evidence produced by a successful preparation.
type Report struct {
	Endpoint   string
	Health     launcher.Health
	Status     launcher.Status
	Mismatches []string
	PreparedAt time.Time
}

// Manager prepares containers for service start.
type Manager struct {
	Launcher launcherAPI
	Profile  Profile
	Now      func() time.Time
}

// launcherAPI is the subset of the bootstrap client the manager needs.
type launcherAPI interface {
	Health(ctx context.Context, endpoint string) (launcher.Health, error)
	Status(ctx context.Context, endpoint string) (launcher.Status, error)
}

// New validates the profile and builds a manager.
func New(client launcherAPI, profile Profile) (*Manager, error) {
	if client == nil {
		return nil, errors.New("instance manager requires a bootstrap client")
	}
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	return &Manager{Launcher: client, Profile: profile, Now: func() time.Time { return time.Now().UTC() }}, nil
}

func (m *Manager) clock() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now().UTC()
}

// Prepare runs every §7.1 preparation step and returns the observed evidence.
//
//  1. verify endpoint reachability
//  2. verify the partner lease is still valid
//  3. check GPU / driver / CUDA / Python / model compatibility
//  4. verify the working and log directories
//  5. verify the bootstrap version
func (m *Manager) Prepare(ctx context.Context, instance domain.Instance) (Report, error) {
	if domain.NormalizeEndpoint(instance.Endpoint) == "" {
		return Report{}, fmt.Errorf("%w: instance %s has no endpoint", ErrEndpointUnreachable, instance.ID)
	}

	health, err := m.Launcher.Health(ctx, instance.Endpoint)
	if err != nil {
		return Report{}, fmt.Errorf("%w: instance %s: %v", ErrEndpointUnreachable, instance.ID, err)
	}
	if !health.Healthy() {
		return Report{}, fmt.Errorf("%w: instance %s bootstrap reports %s", ErrEndpointUnreachable, instance.ID, health.Status)
	}
	if health.Version != m.Profile.BootstrapVersion {
		return Report{}, fmt.Errorf("%w: instance %s runs bootstrap %q, profile requires %q",
			ErrBootstrapVersion, instance.ID, health.Version, m.Profile.BootstrapVersion)
	}

	if err := m.checkLease(instance); err != nil {
		return Report{}, err
	}

	status, err := m.Launcher.Status(ctx, instance.Endpoint)
	if err != nil {
		return Report{}, fmt.Errorf("%w: instance %s status: %v", ErrEndpointUnreachable, instance.ID, err)
	}

	mismatches := m.compatMismatches(status)
	if len(mismatches) > 0 {
		return Report{
			Endpoint:   instance.Endpoint,
			Health:     health,
			Status:     status,
			Mismatches: mismatches,
			PreparedAt: m.clock(),
		}, fmt.Errorf("%w: instance %s: %s", ErrCompatMismatch, instance.ID, strings.Join(mismatches, "; "))
	}

	if strings.TrimSpace(m.Profile.ModelID) != "" && !instance.Spec.SupportsModel(m.Profile.ModelID) {
		return Report{
			Endpoint:   instance.Endpoint,
			Health:     health,
			Status:     status,
			Mismatches: []string{fmt.Sprintf("container does not advertise support for model %q", m.Profile.ModelID)},
			PreparedAt: m.clock(),
		}, fmt.Errorf("%w: instance %s does not support model %q", ErrCompatMismatch, instance.ID, m.Profile.ModelID)
	}

	return Report{
		Endpoint:   instance.Endpoint,
		Health:     health,
		Status:     status,
		PreparedAt: m.clock(),
	}, nil
}

func (m *Manager) checkLease(instance domain.Instance) error {
	if strings.TrimSpace(instance.LeaseID) == "" {
		return fmt.Errorf("%w: instance %s has no lease id", ErrLeaseInvalid, instance.ID)
	}
	if instance.LeaseUpdatedAt.IsZero() {
		return fmt.Errorf("%w: instance %s has no lease observation timestamp", ErrLeaseInvalid, instance.ID)
	}
	if maxAge := m.Profile.MaxLeaseAge; maxAge > 0 {
		age := m.clock().Sub(instance.LeaseUpdatedAt)
		if age > maxAge {
			return fmt.Errorf("%w: instance %s lease observation is %s old (limit %s)",
				ErrLeaseInvalid, instance.ID, age.Round(time.Second), maxAge)
		}
	}
	return nil
}

// compatMismatches compares the reported container environment with the profile.
// It also verifies that the wheelhouse and virtualenv reported by the bootstrap
// exist, which is the §7.1 "working and log directories" step.
func (m *Manager) compatMismatches(status launcher.Status) []string {
	var mismatches []string
	observed := status.Environment
	compare := func(field, want, got string) {
		if strings.TrimSpace(want) == "" {
			return
		}
		if !VersionMatches(want, got) {
			mismatches = append(mismatches, fmt.Sprintf("%s=%q does not match profile %q", field, got, want))
		}
	}
	compare("os", m.Profile.OS, observed.OS)
	compare("cuda", m.Profile.CUDA, observed.CUDA)
	compare("python", m.Profile.Python, observed.Python)
	compare("sglang", m.Profile.SGLang, observed.SGLang)

	if strings.TrimSpace(observed.Wheelhouse) == "" {
		mismatches = append(mismatches, "bootstrap did not report a wheelhouse directory")
	}
	if strings.TrimSpace(observed.Virtualenv) == "" {
		mismatches = append(mismatches, "bootstrap did not report a virtualenv directory")
	}
	if status.Phase == launcher.PhaseFailed {
		mismatches = append(mismatches, "bootstrap reports FAILED phase: "+status.LastError)
	}
	sort.Strings(mismatches)
	return mismatches
}

// VersionMatches compares a profile requirement with an observed version using
// the same rules the container bootstrap applies: a trailing ".x" accepts any
// version on that line, otherwise the values must match exactly. Keeping both
// sides identical is what stops an operator from configuring "0.4.x" in the
// control plane while the container refuses to start.
func VersionMatches(want, got string) bool {
	want = strings.TrimSpace(want)
	got = strings.TrimSpace(got)
	if want == "" {
		return true
	}
	if strings.HasSuffix(want, ".x") {
		return strings.HasPrefix(got, strings.TrimSuffix(want, "x"))
	}
	return strings.EqualFold(want, got)
}

// DescribeFailure renders a preparation failure into metrics and alert details.
func DescribeFailure(err error) (reason string, details map[string]string) {
	details = map[string]string{}
	switch {
	case errors.Is(err, ErrEndpointUnreachable):
		reason = "endpoint_unreachable"
	case errors.Is(err, ErrBootstrapVersion):
		reason = "bootstrap_version_mismatch"
		details["alert"] = obs.AlertBootstrapFailure
	case errors.Is(err, ErrCompatMismatch):
		reason = "environment_mismatch"
		details["alert"] = obs.AlertEnvironmentMismatch
	case errors.Is(err, ErrLeaseInvalid):
		reason = "lease_invalid"
	default:
		reason = "prepare_failed"
	}
	details["error"] = err.Error()
	return reason, details
}
