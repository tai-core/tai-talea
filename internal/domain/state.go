package domain

import "fmt"

// Severity classifies a state rule violation.
type Severity string

const (
	// SeverityReject means the transition must not be applied.
	SeverityReject Severity = "REJECT"
	// SeverityAlert means the combination is tolerated but must be logged and alerted.
	SeverityAlert Severity = "ALERT"
)

// Verdict is the result of validating one (instance state, service state, role) triple.
type Verdict struct {
	Legal    bool
	Severity Severity
	// Code is a stable machine readable identifier used in metrics and alerts.
	Code string
	// Reason is a human readable explanation.
	Reason string
}

// Violation is returned when a state change or combination is not allowed.
type Violation struct {
	Code     string
	Severity Severity
	Message  string
}

func (v *Violation) Error() string { return v.Message }

// NewViolation builds a structured violation error.
func NewViolation(code string, severity Severity, format string, args ...any) *Violation {
	return &Violation{Code: code, Severity: severity, Message: fmt.Sprintf(format, args...)}
}

// Codes used by the state validation layer.
const (
	CodeIllegalInstanceTransition = "illegal_instance_transition"
	CodeIllegalServiceTransition  = "illegal_service_transition"
	CodeIllegalCombination        = "illegal_combination"
	CodeUnknownInstanceState      = "unknown_instance_state"
	CodeUnknownServiceState       = "unknown_service_state"
	CodeMissingRole               = "missing_role"
	CodeServiceStateUnknown       = "service_state_unknown"
)

// instanceTransitions is the container lifecycle graph. It is declared as data
// so that the allowed edges are reviewable at a glance and testable directly.
//
//	ALLOCATING -> PREPARING -> IDLE -> RELEASING -> RELEASED
//	any runnable state -> LOST
//
// IDLE -> PREPARING exists so that a container which failed preparation can be
// re-prepared without being released.
var instanceTransitions = map[InstanceState]map[InstanceState]bool{
	InstanceAllocating: {InstancePreparing: true, InstanceLost: true},
	InstancePreparing:  {InstanceIdle: true, InstanceReleasing: true, InstanceLost: true},
	InstanceIdle:       {InstancePreparing: true, InstanceReleasing: true, InstanceLost: true},
	InstanceReleasing:  {InstanceReleased: true, InstanceLost: true},
	InstanceReleased:   {},
	// LOST compensates back into the control plane or is released; it never
	// jumps straight to RELEASED without a RELEASING step except when the
	// partner already confirmed the container is gone.
	InstanceLost: {InstancePreparing: true, InstanceReleasing: true, InstanceReleased: true},
}

// serviceTransitions is the SGLang service graph from development document §4.2.
//
//	NONE -> STARTING -> HEALTHY -> REGISTERING -> SERVING -> DRAINING -> NONE
//	STARTING / HEALTHY / REGISTERING / SERVING / DRAINING -> FAILED
//	FAILED -> NONE (retry) | STARTING (retry)
var serviceTransitions = map[ServiceState]map[ServiceState]bool{
	ServiceNone:        {ServiceStarting: true},
	ServiceStarting:    {ServiceHealthy: true, ServiceFailed: true},
	ServiceHealthy:     {ServiceRegistering: true, ServiceFailed: true},
	ServiceRegistering: {ServiceServing: true, ServiceFailed: true},
	ServiceServing:     {ServiceDraining: true, ServiceFailed: true},
	ServiceDraining:    {ServiceNone: true, ServiceFailed: true},
	// FAILED -> DRAINING exists so that a failed service can still be stopped
	// and the container handed back; FAILED -> NONE covers a retry that never
	// produced a service at all.
	ServiceFailed: {ServiceNone: true, ServiceStarting: true, ServiceDraining: true},
}

// KnownInstanceState reports whether the value is a defined container state.
func KnownInstanceState(state InstanceState) bool {
	_, ok := instanceTransitions[state]
	return ok
}

// KnownServiceState reports whether the value is a defined service state.
func KnownServiceState(state ServiceState) bool {
	_, ok := serviceTransitions[state]
	return ok
}

// CanTransitionInstance reports whether the container state edge exists.
func CanTransitionInstance(from, to InstanceState) bool {
	next, ok := instanceTransitions[from]
	if !ok {
		return false
	}
	if from == to {
		return true
	}
	return next[to]
}

// CanTransitionService reports whether the service state edge exists.
func CanTransitionService(from, to ServiceState) bool {
	next, ok := serviceTransitions[from]
	if !ok {
		return false
	}
	if from == to {
		return true
	}
	return next[to]
}

// ValidateInstanceTransition rejects container state edges that are not in the graph.
func ValidateInstanceTransition(from, to InstanceState) error {
	if !KnownInstanceState(from) {
		return NewViolation(CodeUnknownInstanceState, SeverityReject, "unknown instance state %q", from)
	}
	if !KnownInstanceState(to) {
		return NewViolation(CodeUnknownInstanceState, SeverityReject, "unknown instance state %q", to)
	}
	if !CanTransitionInstance(from, to) {
		return NewViolation(CodeIllegalInstanceTransition, SeverityReject,
			"instance transition %s -> %s is not allowed", from, to)
	}
	return nil
}

// ValidateServiceTransition rejects service state edges that are not in the graph.
func ValidateServiceTransition(from, to ServiceState) error {
	if !KnownServiceState(from) {
		return NewViolation(CodeUnknownServiceState, SeverityReject, "unknown service state %q", from)
	}
	if !KnownServiceState(to) {
		return NewViolation(CodeUnknownServiceState, SeverityReject, "unknown service state %q", to)
	}
	if !CanTransitionService(from, to) {
		return NewViolation(CodeIllegalServiceTransition, SeverityReject,
			"service transition %s -> %s is not allowed", from, to)
	}
	return nil
}

// ValidateTransition validates both layers and the resulting combination.
// Either state may stay unchanged; the change is only accepted when both edges
// exist in their graphs and the new pair is legal.
func ValidateTransition(from, to Instance) error {
	if err := ValidateInstanceTransition(from.InstanceState, to.InstanceState); err != nil {
		return err
	}
	if err := ValidateServiceTransition(from.ServiceState, to.ServiceState); err != nil {
		return err
	}
	if verdict := CheckCombination(to.InstanceState, to.ServiceState, to.Role); !verdict.Legal {
		return NewViolation(verdict.Code, verdict.Severity, "refuse %s -> %s/%s/%s: %s",
			from, to.InstanceState, to.ServiceState, to.Role, verdict.Reason)
	}
	return nil
}

// runnableServiceStates are the service states that require an assigned role.
var runnableServiceStates = map[ServiceState]bool{
	ServiceStarting:    true,
	ServiceHealthy:     true,
	ServiceRegistering: true,
	ServiceServing:     true,
	ServiceDraining:    true,
}

// CheckCombination validates one (instance state, service state, role) triple
// against the development document §4.2 legality table.
//
// Illegal combinations return Legal=false with SeverityReject: the caller must
// refuse the change, log it and raise an alert - never silently fix it.
// LOST with any service state returns Legal=true with SeverityAlert because the
// real service state is unknown; the caller must keep the instance out of the
// scheduling pool and alert.
func CheckCombination(instance InstanceState, service ServiceState, role Role) Verdict {
	if !KnownInstanceState(instance) {
		return Verdict{Code: CodeUnknownInstanceState, Severity: SeverityReject,
			Reason: fmt.Sprintf("unknown instance state %q", instance)}
	}
	if !KnownServiceState(service) {
		return Verdict{Code: CodeUnknownServiceState, Severity: SeverityReject,
			Reason: fmt.Sprintf("unknown service state %q", service)}
	}

	if runnableServiceStates[service] && !role.Valid() {
		return Verdict{Code: CodeMissingRole, Severity: SeverityReject,
			Reason: fmt.Sprintf("service state %s requires an assigned PD role", service)}
	}

	legal := func() Verdict { return Verdict{Legal: true, Severity: "", Code: "", Reason: ""} }
	reject := func(code, format string, args ...any) Verdict {
		return Verdict{Legal: false, Severity: SeverityReject, Code: code, Reason: fmt.Sprintf(format, args...)}
	}

	switch instance {
	case InstanceAllocating:
		if service != ServiceNone {
			return reject(CodeIllegalCombination,
				"instance %s cannot carry service state %s before preparation", instance, service)
		}
	case InstancePreparing:
		if service != ServiceNone && service != ServiceFailed {
			return reject(CodeIllegalCombination,
				"instance %s cannot carry service state %s", instance, service)
		}
	case InstanceIdle:
		// IDLE + SERVING is the normal steady state once a role is assigned,
		// so the role check above is what makes the unassigned case illegal.
	case InstanceReleasing:
		if service == ServiceStarting || service == ServiceHealthy ||
			service == ServiceRegistering || service == ServiceServing {
			return reject(CodeIllegalCombination,
				"instance %s must drain before hosting service state %s", instance, service)
		}
	case InstanceReleased:
		if service != ServiceNone {
			return reject(CodeIllegalCombination,
				"released instance %s cannot carry service state %s", instance, service)
		}
	case InstanceLost:
		// The real service state is unknown: tolerate, but never stay silent.
		return Verdict{Legal: true, Severity: SeverityAlert, Code: CodeServiceStateUnknown,
			Reason: fmt.Sprintf("instance %s is LOST; reported service state %s cannot be trusted", instance, service)}
	}

	return legal()
}
