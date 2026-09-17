// Package domain holds the capacity plane's state vocabulary: partner
// container instance state, SGLang service state, the capacity event model and
// the rules that keep the two layers consistent.
//
// The package is deliberately free of I/O so that every state rule in
// SGLang容器化实例资源管控工具开发文档 §4 can be unit tested directly.
package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// InstanceState models the lifecycle of one partner-provided container.
type InstanceState string

const (
	// InstanceAllocating means capacity was discovered and is being taken over.
	InstanceAllocating InstanceState = "ALLOCATING"
	// InstancePreparing means the container is being probed and prepared.
	InstancePreparing InstanceState = "PREPARING"
	// InstanceIdle means the container is usable but runs no SGLang service.
	InstanceIdle InstanceState = "IDLE"
	// InstanceReleasing means the partner was asked to reclaim the container.
	InstanceReleasing InstanceState = "RELEASING"
	// InstanceReleased means the container left the control plane.
	InstanceReleased InstanceState = "RELEASED"
	// InstanceLost means the container is unreachable or its lease expired.
	InstanceLost InstanceState = "LOST"
)

// ServiceState models the SGLang service the control plane runs in a container.
type ServiceState string

const (
	// ServiceNone means no control-plane managed SGLang service exists.
	ServiceNone ServiceState = "NONE"
	// ServiceStarting means SGLang is being launched.
	ServiceStarting ServiceState = "STARTING"
	// ServiceHealthy means the SGLang health probe passed but registration did not run yet.
	ServiceHealthy ServiceState = "HEALTHY"
	// ServiceRegistering means the worker is being registered with the Router.
	ServiceRegistering ServiceState = "REGISTERING"
	// ServiceServing means the worker is registered and routable.
	ServiceServing ServiceState = "SERVING"
	// ServiceDraining means readiness is off and in-flight requests are finishing.
	ServiceDraining ServiceState = "DRAINING"
	// ServiceFailed means start, health check, registration or drain failed.
	ServiceFailed ServiceState = "FAILED"
)

// Role is the prefill/decode assignment produced by the PD planner.
type Role string

const (
	RoleNone    Role = ""
	RolePrefill Role = "prefill"
	RoleDecode  Role = "decode"
)

// Valid reports whether the role is one the Router accepts.
func (r Role) Valid() bool { return r == RolePrefill || r == RoleDecode }

// InstanceSpec describes the container shape advertised by the partner.
type InstanceSpec struct {
	GPU          string   `json:"gpu,omitempty" yaml:"gpu,omitempty"`
	GPUCount     int      `json:"gpu_count,omitempty" yaml:"gpu_count,omitempty"`
	ModelSupport []string `json:"model_support,omitempty" yaml:"model_support,omitempty"`
}

// SupportsModel reports whether the spec advertises the model.
func (s InstanceSpec) SupportsModel(model string) bool {
	if model == "" || len(s.ModelSupport) == 0 {
		return true
	}
	for _, candidate := range s.ModelSupport {
		if candidate == model {
			return true
		}
	}
	return false
}

// Instance is the control plane's view of one container. InstanceState and
// ServiceState change independently; see CheckCombination.
type Instance struct {
	ID            string        `json:"id"`
	PartnerID     string        `json:"partner_id"`
	Endpoint      string        `json:"endpoint"`
	LeaseID       string        `json:"lease_id,omitempty"`
	Spec          InstanceSpec  `json:"spec"`
	InstanceState InstanceState `json:"instance_state"`
	ServiceState  ServiceState  `json:"service_state"`
	Role          Role          `json:"role,omitempty"`
	// RoleAssignedAt records when the current PD role was assigned. The planner
	// uses it to honour min_hold_seconds.
	RoleAssignedAt      time.Time `json:"role_assigned_at,omitempty"`
	RouterWorkerID      string    `json:"router_worker_id,omitempty"`
	ReadinessGeneration int64     `json:"readiness_generation"`
	PrepareAttempts     int       `json:"prepare_attempts"`
	StartAttempts       int       `json:"start_attempts"`
	DrainDeadlineAt     time.Time `json:"drain_deadline_at,omitempty"`
	// PendingRelease records that the container must be handed back to the
	// partner once draining finished. It is what distinguishes a revoked
	// container from a plain role conversion.
	PendingRelease bool      `json:"pending_release"`
	LeaseUpdatedAt time.Time `json:"lease_updated_at,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	LastSeenAt     time.Time `json:"last_seen_at,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Runnable reports whether the container may host a control-plane SGLang service.
func (i Instance) Runnable() bool {
	return i.InstanceState == InstanceIdle || i.InstanceState == InstanceReleasing
}

// Active reports whether the instance still belongs to the control plane.
func (i Instance) Active() bool {
	return i.InstanceState != InstanceReleased
}

// Serving reports whether the instance currently holds a Router membership that
// can receive traffic.
func (i Instance) Serving() bool {
	return i.ServiceState == ServiceServing || i.ServiceState == ServiceRegistering
}

// HasService reports whether any control-plane managed SGLang service exists.
func (i Instance) HasService() bool {
	return i.ServiceState != ServiceNone
}

// String renders a compact audit-friendly reference.
func (i Instance) String() string {
	return fmt.Sprintf("%s[%s/%s role=%s]", i.ID, i.InstanceState, i.ServiceState, roleLabel(i.Role))
}

func roleLabel(role Role) string {
	if role == RoleNone {
		return "-"
	}
	return string(role)
}

// SnapshotSpec returns a stable copy of the advertised model list.
func (s InstanceSpec) SnapshotSpec() []string {
	out := append([]string(nil), s.ModelSupport...)
	sort.Strings(out)
	return out
}

// NormalizeEndpoint trims whitespace and a trailing slash so that comparisons
// and Router registrations are stable.
func NormalizeEndpoint(endpoint string) string {
	return strings.TrimRight(strings.TrimSpace(endpoint), "/")
}
