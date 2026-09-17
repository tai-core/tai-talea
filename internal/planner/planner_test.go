package planner

import (
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
)

func newPlanner(t *testing.T, config Config) *Planner {
	t.Helper()
	instance, err := New(config)
	if err != nil {
		t.Fatalf("new planner: %v", err)
	}
	return instance
}

func baseConfig() Config {
	return Config{Gear: "1:1", MaxChangePerRound: 4, MinHold: time.Minute, Cooldown: 0}
}

func TestPlanSplitsIdleCapacityAcrossRoles(t *testing.T) {
	instance := newPlanner(t, baseConfig())
	now := time.Now().UTC()
	decision, err := instance.Plan(Input{
		Now: now,
		Idle: []Candidate{
			{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"},
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if decision.TargetPrefill != 2 || decision.TargetDecode != 2 {
		t.Fatalf("1:1 over four instances must split 2/2, got %d/%d", decision.TargetPrefill, decision.TargetDecode)
	}
	if len(decision.Actions) != 4 {
		t.Fatalf("expected four assignments, got %d (%+v)", len(decision.Actions), decision.Actions)
	}
	prefill, decode := 0, 0
	for _, action := range decision.Actions {
		if action.Kind != ActionAssign {
			t.Fatalf("idle instances must be assigned, got %s", action.Kind)
		}
		switch action.Role {
		case domain.RolePrefill:
			prefill++
		case domain.RoleDecode:
			decode++
		}
	}
	if prefill != 2 || decode != 2 {
		t.Fatalf("assignment split %d/%d, want 2/2", prefill, decode)
	}
}

func TestGearsProduceTheDocumentedRatio(t *testing.T) {
	cases := map[string][2]int{
		"1:1": {3, 3},
		"2:1": {4, 2},
		"1:2": {2, 4},
	}
	for gear, want := range cases {
		instance := newPlanner(t, Config{Gear: gear, MaxChangePerRound: 10})
		now := time.Now().UTC()
		idle := make([]Candidate, 0, 6)
		for index := 0; index < 6; index++ {
			idle = append(idle, Candidate{ID: string(rune('a' + index))})
		}
		decision, err := instance.Plan(Input{Now: now, Idle: idle})
		if err != nil {
			t.Fatalf("plan %s: %v", gear, err)
		}
		if decision.TargetPrefill != want[0] || decision.TargetDecode != want[1] {
			t.Fatalf("gear %s must target %d/%d, got %d/%d",
				gear, want[0], want[1], decision.TargetPrefill, decision.TargetDecode)
		}
	}
}

func TestMaxChangePerRoundCapsTheRound(t *testing.T) {
	instance := newPlanner(t, Config{Gear: "1:1", MaxChangePerRound: 2})
	now := time.Now().UTC()
	idle := []Candidate{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	decision, err := instance.Plan(Input{Now: now, Idle: idle})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(decision.Actions) != 2 {
		t.Fatalf("max_change_per_round=2 must cap the round, got %d actions", len(decision.Actions))
	}
}

func TestCooldownBlocksRepeatedRounds(t *testing.T) {
	instance := newPlanner(t, Config{Gear: "1:1", MaxChangePerRound: 4, Cooldown: time.Minute})
	now := time.Now().UTC()
	first, err := instance.Plan(Input{Now: now, Idle: []Candidate{{ID: "a"}, {ID: "b"}}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(first.Actions) == 0 {
		t.Fatal("the first round must produce actions")
	}
	second, err := instance.Plan(Input{Now: now.Add(5 * time.Second), Idle: []Candidate{{ID: "c"}, {ID: "d"}}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(second.Actions) != 0 {
		t.Fatalf("cooldown must suppress a change inside the window, got %+v", second.Actions)
	}
	if len(second.Notes) == 0 {
		t.Fatal("a suppressed round must explain why")
	}
	third, err := instance.Plan(Input{Now: now.Add(2 * time.Minute), Idle: []Candidate{{ID: "c"}, {ID: "d"}}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(third.Actions) == 0 {
		t.Fatal("the round after the cooldown must be free to act")
	}
}

func TestMinHoldProtectsFreshAssignments(t *testing.T) {
	instance := newPlanner(t, Config{Gear: "2:1", MaxChangePerRound: 4, MinHold: 5 * time.Minute})
	now := time.Now().UTC()
	serving := []Candidate{
		{ID: "p1", Role: domain.RolePrefill, Serving: true, RoleSince: now.Add(-10 * time.Minute), Assigned: true},
		{ID: "p2", Role: domain.RolePrefill, Serving: true, RoleSince: now.Add(-10 * time.Minute), Assigned: true},
		{ID: "d1", Role: domain.RoleDecode, Serving: true, RoleSince: now.Add(-10 * time.Second), Assigned: true},
	}
	decision, err := instance.Plan(Input{Now: now, Serving: serving})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, action := range decision.Actions {
		if action.InstanceID == "d1" {
			t.Fatal("an instance inside min_hold must not change role")
		}
	}
}

func TestReassignConvertsSurplusIntoDeficit(t *testing.T) {
	instance := newPlanner(t, Config{Gear: "1:1", MaxChangePerRound: 4})
	now := time.Now().UTC()
	serving := []Candidate{
		{ID: "p1", Role: domain.RolePrefill, Serving: true, Assigned: true},
		{ID: "p2", Role: domain.RolePrefill, Serving: true, Assigned: true},
		{ID: "p3", Role: domain.RolePrefill, Serving: true, Assigned: true},
	}
	decision, err := instance.Plan(Input{Now: now, Serving: serving})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	converted := 0
	for _, action := range decision.Actions {
		if action.Kind == ActionReassign && action.FromRole == domain.RolePrefill && action.Role == domain.RoleDecode {
			converted++
		}
	}
	if converted == 0 {
		t.Fatalf("three prefill workers under a 1:1 gear must convert one to decode, got %+v", decision.Actions)
	}
}

func TestIdleInstancesArePreferredOverConversions(t *testing.T) {
	instance := newPlanner(t, Config{Gear: "2:1", MaxChangePerRound: 10})
	now := time.Now().UTC()
	decision, err := instance.Plan(Input{
		Now:     now,
		Serving: []Candidate{{ID: "d1", Role: domain.RoleDecode, Serving: true, Assigned: true}},
		Idle:    []Candidate{{ID: "i1"}, {ID: "i2"}},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	assigns := 0
	for _, action := range decision.Actions {
		if action.Kind == ActionAssign {
			assigns++
		}
		if action.InstanceID == "d1" {
			t.Fatalf("a working decode worker must not be converted while idle capacity exists: %+v", action)
		}
	}
	if assigns != 2 {
		t.Fatalf("both idle instances must be used, got %d assignments", assigns)
	}
}

func TestMaxServingDrainsSurplus(t *testing.T) {
	instance := newPlanner(t, Config{Gear: "1:1", MaxChangePerRound: 10, MaxServing: 2})
	now := time.Now().UTC()
	serving := []Candidate{
		{ID: "p1", Role: domain.RolePrefill, Serving: true, Assigned: true, RoleSince: now.Add(-time.Hour)},
		{ID: "d1", Role: domain.RoleDecode, Serving: true, Assigned: true, RoleSince: now.Add(-time.Hour)},
		{ID: "p2", Role: domain.RolePrefill, Serving: true, Assigned: true, RoleSince: now.Add(-time.Minute)},
	}
	decision, err := instance.Plan(Input{Now: now, Serving: serving})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	drained := 0
	for _, action := range decision.Actions {
		if action.Kind == ActionDrain {
			drained++
		}
	}
	if drained != 1 {
		t.Fatalf("max_serving=2 with three serving workers must drain one, got %+v", decision.Actions)
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := New(Config{Gear: "3:1", MaxChangePerRound: 1}); err == nil {
		t.Fatal("an unknown gear must be rejected")
	}
	if _, err := New(Config{Gear: "1:1", MaxChangePerRound: 0}); err == nil {
		t.Fatal("a non positive max_change_per_round must be rejected")
	}
	if _, err := New(Config{Gear: "1:1", MaxChangePerRound: 1, MinHold: -time.Second}); err == nil {
		t.Fatal("a negative min_hold must be rejected")
	}
}

func TestSetGearSwitchesTheManualGear(t *testing.T) {
	instance := newPlanner(t, baseConfig())
	if err := instance.SetGear("2:1"); err != nil {
		t.Fatalf("set gear: %v", err)
	}
	if instance.Config().Gear != "2:1" {
		t.Fatalf("gear=%s, want 2:1", instance.Config().Gear)
	}
	if err := instance.SetGear("5:1"); err == nil {
		t.Fatal("an unsupported gear must be rejected")
	}
}
