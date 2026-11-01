package planner

import (
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
)

func aggregatedConfig() Config {
	return Config{Mode: ModeAggregated, MaxChangePerRound: 8, MinHold: 0, Cooldown: 0}
}

// An aggregated pool must not invent a ratio: every idle instance gets the same
// role, and nothing is left unassigned waiting for a decode slot.
func TestAggregatedAssignsEveryIdleInstanceAsRegular(t *testing.T) {
	instance := newPlanner(t, aggregatedConfig())
	decision, err := instance.Plan(Input{
		Now:  time.Now().UTC(),
		Idle: []Candidate{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if decision.Mode != ModeAggregated {
		t.Fatalf("mode = %q, want %q", decision.Mode, ModeAggregated)
	}
	if decision.TargetRegular != 4 {
		t.Fatalf("target regular = %d, want 4", decision.TargetRegular)
	}
	if decision.TargetPrefill != 0 || decision.TargetDecode != 0 {
		t.Fatalf("aggregated mode must not publish a P/D target, got %d/%d",
			decision.TargetPrefill, decision.TargetDecode)
	}
	if len(decision.Actions) != 4 {
		t.Fatalf("expected four assignments, got %d (%+v)", len(decision.Actions), decision.Actions)
	}
	for _, action := range decision.Actions {
		if action.Kind != ActionAssign {
			t.Fatalf("idle instances must be assigned, got %s", action.Kind)
		}
		if action.Role != domain.RoleRegular {
			t.Fatalf("aggregated assignment gave role %q, want %q", action.Role, domain.RoleRegular)
		}
	}
}

// A gear is meaningless without a ratio, so aggregated mode must accept a
// configuration that carries none at all.
func TestAggregatedConfigDoesNotRequireAGear(t *testing.T) {
	if err := aggregatedConfig().Validate(); err != nil {
		t.Fatalf("aggregated config must validate without a gear: %v", err)
	}
	if err := (Config{Mode: ModeAggregated, MaxChangePerRound: 1, Gear: ""}).Validate(); err != nil {
		t.Fatalf("empty gear in aggregated mode: %v", err)
	}
	if err := (Config{Mode: ModeFixed, MaxChangePerRound: 1, Gear: ""}).Validate(); err == nil {
		t.Fatal("fixed mode without a gear must still be rejected")
	}
}

// Switching a running PD deployment to aggregated is a real operation, so it
// must be safe: the workers that were started with --disaggregation-mode are
// drained first and re-assigned as regular afterwards.
func TestAggregatedDrainsLeftoverPDWorkers(t *testing.T) {
	instance := newPlanner(t, aggregatedConfig())
	now := time.Now().UTC()
	decision, err := instance.Plan(Input{
		Now: now,
		Serving: []Candidate{
			{ID: "p", Role: domain.RolePrefill, Serving: true, Assigned: true, RoleSince: now.Add(-time.Hour)},
			{ID: "d", Role: domain.RoleDecode, Serving: true, Assigned: true, RoleSince: now.Add(-time.Hour)},
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(decision.Actions) != 2 {
		t.Fatalf("expected both PD workers to be drained, got %d (%+v)", len(decision.Actions), decision.Actions)
	}
	for _, action := range decision.Actions {
		if action.Kind != ActionDrain {
			t.Fatalf("PD workers in an aggregated pool must drain first, got %s", action.Kind)
		}
	}
}

// max_serving is the only sizing knob aggregated mode has, and it must cap the
// pool rather than being ignored.
func TestAggregatedHonoursMaxServing(t *testing.T) {
	config := aggregatedConfig()
	config.MaxServing = 3
	instance := newPlanner(t, config)
	decision, err := instance.Plan(Input{
		Now:  time.Now().UTC(),
		Idle: []Candidate{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if decision.TargetRegular != 3 {
		t.Fatalf("target regular = %d, want 3", decision.TargetRegular)
	}
	if len(decision.Actions) != 3 {
		t.Fatalf("expected three assignments under max_serving, got %d", len(decision.Actions))
	}
}

// Aggregated mode has no load signal, so capacity verbs must stay refused.
func TestAggregatedRejectsCapacityActions(t *testing.T) {
	config := aggregatedConfig()
	config.CapacityActions = true
	if err := config.Validate(); err == nil {
		t.Fatal("capacity actions need a load signal and must be refused in aggregated mode")
	}
}

// The reverse switch must be safe too: a regular worker left in a PD pool is
// drained rather than counted as a decode worker, because counting it would move
// the ratio target and convert a healthy worker to fix a phantom imbalance.
func TestFixedModeDrainsLeftoverRegularWorkers(t *testing.T) {
	instance := newPlanner(t, baseConfig())
	now := time.Now().UTC()
	decision, err := instance.Plan(Input{
		Now: now,
		Serving: []Candidate{
			{ID: "p", Role: domain.RolePrefill, Serving: true, Assigned: true, RoleSince: now.Add(-time.Hour)},
			{ID: "d", Role: domain.RoleDecode, Serving: true, Assigned: true, RoleSince: now.Add(-time.Hour)},
			{ID: "r", Role: domain.RoleRegular, Serving: true, Assigned: true, RoleSince: now.Add(-time.Hour)},
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if decision.CurrentRatio != 0.5 {
		t.Fatalf("current ratio = %v, want 0.5 (the regular worker is not a decode worker)", decision.CurrentRatio)
	}
	drains := 0
	for _, action := range decision.Actions {
		if action.Kind == ActionDrain && action.InstanceID == "r" {
			drains++
		}
		if action.Kind == ActionReassign {
			t.Fatalf("a leftover regular worker must not trigger a reassignment: %+v", action)
		}
	}
	if drains != 1 {
		t.Fatalf("regular worker was drained %d times, want 1 (%+v)", drains, decision.Actions)
	}
}

// Adding a third role must not silently change an existing PD ratio, and the
// decision must keep reporting the PD shape rather than an aggregated target.
func TestFixedModeIgnoresRegularWorkersInTheRatio(t *testing.T) {
	instance := newPlanner(t, baseConfig())
	now := time.Now().UTC()
	decision, err := instance.Plan(Input{
		Now: now,
		Serving: []Candidate{
			{ID: "p", Role: domain.RolePrefill, Serving: true, Assigned: true, RoleSince: now.Add(-time.Hour)},
			{ID: "d", Role: domain.RoleDecode, Serving: true, Assigned: true, RoleSince: now.Add(-time.Hour)},
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if decision.Mode != ModeFixed {
		t.Fatalf("mode = %q, want %q", decision.Mode, ModeFixed)
	}
	if decision.CurrentRatio != 0.5 {
		t.Fatalf("current ratio = %v, want 0.5", decision.CurrentRatio)
	}
	if decision.TargetRegular != 0 {
		t.Fatalf("PD mode must not publish a regular target, got %d", decision.TargetRegular)
	}
}
