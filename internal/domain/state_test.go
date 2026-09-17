package domain

import (
	"testing"
	"time"
)

func TestInstanceTransitionGraph(t *testing.T) {
	allowed := []struct {
		from InstanceState
		to   InstanceState
	}{
		{InstanceAllocating, InstancePreparing},
		{InstanceAllocating, InstanceLost},
		{InstancePreparing, InstanceIdle},
		{InstancePreparing, InstanceReleasing},
		{InstancePreparing, InstanceLost},
		{InstanceIdle, InstanceReleasing},
		{InstanceIdle, InstanceLost},
		{InstanceIdle, InstancePreparing},
		{InstanceReleasing, InstanceReleased},
		{InstanceReleasing, InstanceLost},
		{InstanceLost, InstancePreparing},
		{InstanceLost, InstanceReleasing},
		{InstanceLost, InstanceReleased},
	}
	for _, edge := range allowed {
		if err := ValidateInstanceTransition(edge.from, edge.to); err != nil {
			t.Errorf("%s -> %s must be allowed: %v", edge.from, edge.to, err)
		}
	}

	rejected := []struct {
		from InstanceState
		to   InstanceState
	}{
		{InstanceAllocating, InstanceIdle},
		{InstancePreparing, InstanceReleased},
		{InstanceIdle, InstanceReleased},
		{InstanceReleased, InstancePreparing},
		{InstanceReleased, InstanceIdle},
		{InstanceReleased, InstanceLost},
	}
	for _, edge := range rejected {
		err := ValidateInstanceTransition(edge.from, edge.to)
		if err == nil {
			t.Fatalf("%s -> %s must be rejected", edge.from, edge.to)
		}
		var violation *Violation
		if !asViolation(err, &violation) {
			t.Fatalf("%s -> %s must return a structured violation, got %T", edge.from, edge.to, err)
		}
		if violation.Severity != SeverityReject {
			t.Fatalf("%s -> %s must be a reject severity violation", edge.from, edge.to)
		}
	}
}

func TestServiceTransitionGraph(t *testing.T) {
	allowed := []struct {
		from ServiceState
		to   ServiceState
	}{
		{ServiceNone, ServiceStarting},
		{ServiceStarting, ServiceHealthy},
		{ServiceStarting, ServiceFailed},
		{ServiceHealthy, ServiceRegistering},
		{ServiceRegistering, ServiceServing},
		{ServiceServing, ServiceDraining},
		{ServiceServing, ServiceFailed},
		{ServiceDraining, ServiceNone},
		{ServiceDraining, ServiceFailed},
		{ServiceFailed, ServiceNone},
		{ServiceFailed, ServiceStarting},
	}
	for _, edge := range allowed {
		if err := ValidateServiceTransition(edge.from, edge.to); err != nil {
			t.Errorf("%s -> %s must be allowed: %v", edge.from, edge.to, err)
		}
	}

	rejected := []struct {
		from ServiceState
		to   ServiceState
	}{
		{ServiceNone, ServiceServing},
		{ServiceNone, ServiceDraining},
		{ServiceServing, ServiceStarting},
		{ServiceDraining, ServiceServing},
		{ServiceServing, ServiceNone},
	}
	for _, edge := range rejected {
		if err := ValidateServiceTransition(edge.from, edge.to); err == nil {
			t.Fatalf("%s -> %s must be rejected", edge.from, edge.to)
		}
	}
}

// TestCombinationTable mirrors the legality table in development document §4.2.
func TestCombinationTable(t *testing.T) {
	cases := []struct {
		name      string
		instance  InstanceState
		service   ServiceState
		role      Role
		wantLegal bool
		wantSever Severity
	}{
		{"idle none is legal", InstanceIdle, ServiceNone, RoleNone, true, ""},
		{"idle failed is legal", InstanceIdle, ServiceFailed, RoleNone, true, ""},
		{"preparing none is legal", InstancePreparing, ServiceNone, RoleNone, true, ""},
		{"preparing starting is illegal", InstancePreparing, ServiceStarting, RolePrefill, false, SeverityReject},
		{"idle serving without role is illegal", InstanceIdle, ServiceServing, RoleNone, false, SeverityReject},
		{"idle serving with role is legal", InstanceIdle, ServiceServing, RoleDecode, true, ""},
		{"allocating none is legal", InstanceAllocating, ServiceNone, RoleNone, true, ""},
		{"allocating starting is illegal", InstanceAllocating, ServiceStarting, RolePrefill, false, SeverityReject},
		{"released with service is illegal", InstanceReleased, ServiceStarting, RolePrefill, false, SeverityReject},
		{"released none is legal", InstanceReleased, ServiceNone, RoleNone, true, ""},
		{"releasing draining is legal", InstanceReleasing, ServiceDraining, RolePrefill, true, ""},
		{"releasing serving is illegal", InstanceReleasing, ServiceServing, RolePrefill, false, SeverityReject},
		{"lost alerts but stays legal", InstanceLost, ServiceServing, RoleDecode, true, SeverityAlert},
		{"starting without role is illegal", InstanceIdle, ServiceStarting, RoleNone, false, SeverityReject},
		{"healthy without role is illegal", InstanceIdle, ServiceHealthy, RoleNone, false, SeverityReject},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			verdict := CheckCombination(testCase.instance, testCase.service, testCase.role)
			if verdict.Legal != testCase.wantLegal {
				t.Fatalf("legal=%v, want %v (%s)", verdict.Legal, testCase.wantLegal, verdict.Reason)
			}
			if verdict.Severity != testCase.wantSever {
				t.Fatalf("severity=%q, want %q", verdict.Severity, testCase.wantSever)
			}
			if !verdict.Legal && verdict.Code == "" {
				t.Fatal("an illegal combination must carry a machine readable code")
			}
			if verdict.Severity == SeverityAlert && verdict.Reason == "" {
				t.Fatal("an alerted combination must explain itself")
			}
		})
	}
}

func TestEmptyServiceStatesReportAProblem(t *testing.T) {
	bogus := CheckCombination(InstanceState("RUNNING"), ServiceNone, RoleNone)
	if bogus.Legal || bogus.Code != CodeUnknownInstanceState {
		t.Fatalf("unknown instance state must be rejected with %s, got %+v", CodeUnknownInstanceState, bogus)
	}
	bogusService := CheckCombination(InstanceIdle, ServiceState("BUSY"), RoleNone)
	if bogusService.Legal || bogusService.Code != CodeUnknownServiceState {
		t.Fatalf("unknown service state must be rejected with %s, got %+v", CodeUnknownServiceState, bogusService)
	}
}

func TestValidateTransitionAcrossBothLayers(t *testing.T) {
	from := Instance{InstanceState: InstanceIdle, ServiceState: ServiceNone}
	to := Instance{InstanceState: InstanceIdle, ServiceState: ServiceStarting, Role: RolePrefill}
	if err := ValidateTransition(from, to); err != nil {
		t.Fatalf("IDLE/NONE -> IDLE/STARTING must be allowed: %v", err)
	}

	// Both edges exist in their graphs, but the resulting pair is illegal:
	// ValidateTransition must still refuse it.
	illegalFrom := Instance{InstanceState: InstancePreparing, ServiceState: ServiceNone}
	illegalTo := Instance{InstanceState: InstancePreparing, ServiceState: ServiceStarting, Role: RolePrefill}
	err := ValidateTransition(illegalFrom, illegalTo)
	if err == nil {
		t.Fatal("PREPARING/NONE -> PREPARING/STARTING must be rejected")
	}
	var violation *Violation
	if !asViolation(err, &violation) || violation.Code != CodeIllegalCombination {
		t.Fatalf("expected an illegal_combination violation, got %v", err)
	}

	// A service state that requires a role must be refused when no role is set.
	if err := ValidateTransition(from, Instance{InstanceState: InstanceIdle, ServiceState: ServiceStarting}); err == nil {
		t.Fatal("STARTING without a PD role must be rejected")
	}
}

func TestEventValidation(t *testing.T) {
	valid := CapacityEvent{
		EventID:    "partner-a:evt-1",
		PartnerID:  "partner-a",
		Type:       EventCapacityAdded,
		OccurredAt: time.Now().UTC(),
		Instance: &EventInstance{
			ID:       "container-1",
			Endpoint: "http://10.0.0.1:8080",
			LeaseID:  "lease-1",
			Spec:     &InstanceSpec{GPU: "H100", GPUCount: 1, ModelSupport: []string{"model-a"}},
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid event must pass: %v", err)
	}

	cases := map[string]CapacityEvent{
		"missing event id":   {PartnerID: "partner-a", Type: EventCapacityAdded, OccurredAt: time.Now()},
		"missing partner":    {EventID: "e", Type: EventCapacityAdded, OccurredAt: time.Now()},
		"unknown type":       {EventID: "e", PartnerID: "p", Type: EventType("CAPACITY_EXPLODED"), OccurredAt: time.Now()},
		"missing occurrence": {EventID: "e", PartnerID: "p", Type: EventCapacityAdded},
		"missing instance":   {EventID: "e", PartnerID: "p", Type: EventCapacityAdded, OccurredAt: time.Now()},
		"bad endpoint": {
			EventID: "e", PartnerID: "p", Type: EventCapacityAdded, OccurredAt: time.Now(),
			Instance: &EventInstance{ID: "x", Endpoint: "10.0.0.1:8080"},
		},
		"padded id": {
			EventID: "e", PartnerID: "p", Type: EventCapacityAdded, OccurredAt: time.Now(),
			Instance: &EventInstance{ID: " x "},
		},
		"grace too large": {
			EventID: "e", PartnerID: "p", Type: EventCapacityRevoked, OccurredAt: time.Now(),
			Instance: &EventInstance{ID: "x"}, GraceSeconds: MaxGraceSeconds + 1,
		},
	}
	for name, event := range cases {
		if err := event.Validate(); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
}

func TestSnapshotValidationRejectsDuplicates(t *testing.T) {
	snapshot := Snapshot{
		Version: "v1",
		Instances: []EventInstance{
			{ID: "a", Endpoint: "http://a:1"},
			{ID: "a", Endpoint: "http://a:1"},
		},
	}
	if err := snapshot.Validate(); err == nil {
		t.Fatal("duplicate snapshot instances must be rejected")
	}
}

func asViolation(err error, target **Violation) bool {
	violation, ok := err.(*Violation)
	if ok {
		*target = violation
	}
	return ok
}
