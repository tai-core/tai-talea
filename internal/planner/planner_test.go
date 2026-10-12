package planner

import (
	"strings"
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

// TestRefusesToReassignTheLastPrefill pins the nightshift incident that
// motivated the guard. With one Prefill and one Decode serving, draining the
// Decode left a single serving instance, the ratio target then asked for one
// Decode and the round reassigned the only Prefill: the deployment reached
// 0P0D and stopped serving for four minutes.
func TestRefusesToReassignTheLastPrefill(t *testing.T) {
	instance := newPlanner(t, Config{Gear: "1:1", MaxChangePerRound: 4})
	now := time.Now().UTC()
	decision, err := instance.Plan(Input{
		Now: now,
		Serving: []Candidate{
			{ID: "p1", Role: domain.RolePrefill, Serving: true, Assigned: true, RoleSince: now.Add(-time.Hour)},
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(decision.Actions) != 0 {
		t.Fatalf("the last prefill worker must not be reassigned, got %+v", decision.Actions)
	}
	if len(decision.Notes) == 0 {
		t.Fatal("a refusal must explain itself")
	}
	if !strings.Contains(strings.Join(decision.Notes, "; "), "last prefill worker") {
		t.Fatalf("the note must name what was protected, got %v", decision.Notes)
	}
}

// TestFreshLastPrefillIsAlsoRefused covers the same deficit when min_hold, not
// the guard, is what actually blocks the round: the refusal has to stay
// non-destructive and still be explained.
func TestFreshLastPrefillIsAlsoRefused(t *testing.T) {
	instance := newPlanner(t, Config{Gear: "1:1", MaxChangePerRound: 4, MinHold: time.Hour})
	now := time.Now().UTC()
	decision, err := instance.Plan(Input{
		Now: now,
		Serving: []Candidate{
			{ID: "p1", Role: domain.RolePrefill, Serving: true, Assigned: true, RoleSince: now.Add(-time.Minute)},
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(decision.Actions) != 0 {
		t.Fatalf("a worker inside min_hold must not change role, got %+v", decision.Actions)
	}
	if !strings.Contains(strings.Join(decision.Notes, "; "), "no movable prefill worker") {
		t.Fatalf("the note must name the min_hold reason, got %v", decision.Notes)
	}
}

// TestReassignStillWorksWhenOneSpareWorkerRemains is the boundary the guard
// must not overreach: two prefills and no decode is the smallest pool where the
// conversion is legal, because the source role keeps a worker afterwards.
func TestReassignStillWorksWhenOneSpareWorkerRemains(t *testing.T) {
	instance := newPlanner(t, Config{Gear: "1:1", MaxChangePerRound: 4})
	now := time.Now().UTC()
	decision, err := instance.Plan(Input{
		Now: now,
		Serving: []Candidate{
			{ID: "p1", Role: domain.RolePrefill, Serving: true, Assigned: true, RoleSince: now.Add(-time.Hour)},
			{ID: "p2", Role: domain.RolePrefill, Serving: true, Assigned: true, RoleSince: now.Add(-time.Hour)},
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(decision.Actions) != 1 {
		t.Fatalf("one prefill must convert to decode, got %+v (notes %v)", decision.Actions, decision.Notes)
	}
	action := decision.Actions[0]
	if action.Kind != ActionReassign || action.FromRole != domain.RolePrefill || action.Role != domain.RoleDecode {
		t.Fatalf("want prefill -> decode reassignment, got %+v", action)
	}
}

func countRoles(candidates []Candidate) map[domain.Role]int {
	counts := map[domain.Role]int{}
	for _, candidate := range candidates {
		if candidate.Role != "" {
			counts[candidate.Role]++
		}
	}
	return counts
}

// TestPlanNeverEmptiesARole sweeps the reachable populations and gears and
// asserts the invariant the guard exists for: a round may not move the last
// worker out of a role. A disaggregated pool needs both roles to serve a single
// request, so an empty role is not a ratio change, it is an outage.
func TestPlanNeverEmptiesARole(t *testing.T) {
	now := time.Now().UTC()
	serving := func(id string, role domain.Role) Candidate {
		return Candidate{ID: id, Role: role, Serving: true, Assigned: true, RoleSince: now.Add(-time.Hour)}
	}
	idle := func(id string) Candidate { return Candidate{ID: id} }
	populations := [][]Candidate{
		{serving("p1", domain.RolePrefill)},
		{serving("d1", domain.RoleDecode)},
		{serving("p1", domain.RolePrefill), serving("d1", domain.RoleDecode)},
		{serving("p1", domain.RolePrefill), serving("p2", domain.RolePrefill)},
		{serving("d1", domain.RoleDecode), serving("d2", domain.RoleDecode)},
		{serving("p1", domain.RolePrefill), serving("d1", domain.RoleDecode), serving("d2", domain.RoleDecode)},
		{serving("p1", domain.RolePrefill), serving("p2", domain.RolePrefill), serving("d1", domain.RoleDecode)},
		{serving("p1", domain.RolePrefill), serving("p2", domain.RolePrefill), serving("p3", domain.RolePrefill)},
		{serving("d1", domain.RoleDecode), serving("d2", domain.RoleDecode), serving("d3", domain.RoleDecode)},
		{serving("p1", domain.RolePrefill), idle("i1")},
		{serving("d1", domain.RoleDecode), idle("i1"), idle("i2")},
		{idle("i1")},
	}
	for _, gear := range GearNames() {
		for index, population := range populations {
			input := Input{Now: now}
			for _, candidate := range population {
				if candidate.Role == "" {
					input.Idle = append(input.Idle, candidate)
				} else {
					input.Serving = append(input.Serving, candidate)
				}
			}
			instance := newPlanner(t, Config{Gear: gear, MaxChangePerRound: 10})
			decision, err := instance.Plan(input)
			if err != nil {
				t.Fatalf("plan gear=%s population=%d: %v", gear, index, err)
			}
			before := countRoles(population)
			after := countRoles(population)
			for _, action := range decision.Actions {
				switch action.Kind {
				case ActionAssign:
					after[action.Role]++
				case ActionReassign:
					after[action.FromRole]--
					after[action.Role]++
				case ActionDrain:
					after[action.FromRole]--
				}
			}
			for _, role := range []domain.Role{domain.RolePrefill, domain.RoleDecode} {
				if before[role] > 0 && after[role] == 0 {
					t.Fatalf("gear=%s population=%d emptied %s: actions=%+v notes=%v",
						gear, index, role, decision.Actions, decision.Notes)
				}
			}
		}
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
	if _, err := New(Config{Mode: "adaptive", Gear: "1:1", MaxChangePerRound: 1}); err == nil {
		t.Fatal("an unknown planner mode must be rejected")
	}
	if _, err := New(Config{Mode: ModeAuto, Gear: "1:1", MaxChangePerRound: 1, AutoDeadBand: -0.1}); err == nil {
		t.Fatal("a negative auto dead band must be rejected")
	}
	if _, err := New(Config{Mode: ModeAuto, Gear: "1:1", MaxChangePerRound: 1}); err != nil {
		t.Fatalf("auto mode must be a valid configuration: %v", err)
	}
	if _, err := New(Config{Gear: "1:1", MaxChangePerRound: 1}); err != nil {
		t.Fatalf("an unset mode must stay valid and mean fixed: %v", err)
	}
}

// measured builds a complete load sample with requests waiting, which is the
// only situation in which auto mode is allowed to move.
func measured(prefill, decode float64) LoadSnapshot {
	return LoadSnapshot{
		Valid: true, PrefillPressure: &prefill, DecodePressure: &decode, QueueLength: 1,
	}
}

// TestAutoModeIgnoresAPressureGapWithoutQueueing follows the sampler's own
// advice: with nothing waiting, a pressure difference only reflects concurrency
// occupancy, so it is not a capacity shortage and must not move the ladder.
func TestAutoModeIgnoresAPressureGapWithoutQueueing(t *testing.T) {
	prefill, decode := 0.95, 0.05
	instance := newPlanner(t, Config{Mode: ModeAuto, Gear: "1:1", MaxChangePerRound: 4})
	decision, err := instance.Plan(Input{Now: time.Now().UTC(), Load: LoadSnapshot{
		Valid: true, PrefillPressure: &prefill, DecodePressure: &decode,
	}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if decision.EffectiveGear != "1:1" {
		t.Fatalf("a gap without queueing must not move the ladder, got %s", decision.EffectiveGear)
	}
	if !strings.Contains(strings.Join(decision.Notes, "; "), "nothing is queued") {
		t.Fatalf("the hold must be explained, got %v", decision.Notes)
	}
}

// TestFixedModeIgnoresTheLoad is the milestone 1 guarantee: a complete sample
// with an extreme imbalance must not move the configured gear.
func TestFixedModeIgnoresTheLoad(t *testing.T) {
	instance := newPlanner(t, Config{Gear: "1:1", MaxChangePerRound: 4})
	decision, err := instance.Plan(Input{Now: time.Now().UTC(), Load: measured(0.99, 0.01)})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if decision.Mode != ModeFixed || decision.EffectiveGear != "1:1" || decision.TargetRatio != 0.5 {
		t.Fatalf("fixed mode must keep its gear, got mode=%s effective=%s ratio=%v",
			decision.Mode, decision.EffectiveGear, decision.TargetRatio)
	}
	if instance.EffectiveGear() != "1:1" {
		t.Fatalf("fixed mode moved the ladder: %s", instance.EffectiveGear())
	}
}

func TestAutoModeMovesOneGearStepTowardsTheBusierRole(t *testing.T) {
	cases := []struct {
		gear            string
		prefill, decode float64
		want            string
	}{
		{"1:1", 0.9, 0.1, "2:1"},
		{"1:1", 0.1, 0.9, "1:2"},
		// One rung per round: a decode-heavy sample must not jump 2:1 -> 1:2.
		{"2:1", 0.1, 0.9, "1:1"},
		{"1:2", 0.9, 0.1, "1:1"},
	}
	for _, testCase := range cases {
		instance := newPlanner(t, Config{Mode: ModeAuto, Gear: testCase.gear, MaxChangePerRound: 4})
		decision, err := instance.Plan(Input{Now: time.Now().UTC(), Load: measured(testCase.prefill, testCase.decode)})
		if err != nil {
			t.Fatalf("plan %s: %v", testCase.gear, err)
		}
		if decision.EffectiveGear != testCase.want {
			t.Fatalf("gear %s with pressure %v/%v must move to %s, got %s",
				testCase.gear, testCase.prefill, testCase.decode, testCase.want, decision.EffectiveGear)
		}
		if decision.Gear != testCase.gear {
			t.Fatalf("the configured gear is the anchor and must not be rewritten, got %s", decision.Gear)
		}
		if decision.TargetRatio != Gears[testCase.want] {
			t.Fatalf("target ratio=%v, want %v", decision.TargetRatio, Gears[testCase.want])
		}
		if len(decision.Notes) == 0 {
			t.Fatal("an automatic gear change must be explained")
		}
		if instance.EffectiveGear() != testCase.want {
			t.Fatalf("the chosen rung must persist for the next round, got %s", instance.EffectiveGear())
		}
	}
}

func TestAutoModeWalksTheLadderOneRungPerRound(t *testing.T) {
	instance := newPlanner(t, Config{Mode: ModeAuto, Gear: "2:1", MaxChangePerRound: 4})
	now := time.Now().UTC()
	for _, want := range []string{"1:1", "1:2", "1:2"} {
		decision, err := instance.Plan(Input{Now: now, Load: measured(0.1, 0.9)})
		if err != nil {
			t.Fatalf("plan at %s: %v", now, err)
		}
		if decision.EffectiveGear != want {
			t.Fatalf("at %s effective gear=%s, want %s", now, decision.EffectiveGear, want)
		}
		now = now.Add(time.Second)
	}
	// The last round is already at the decode-heavy end and has to say so.
	last, err := instance.Plan(Input{Now: now, Load: measured(0.1, 0.9)})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if last.EffectiveGear != "1:2" {
		t.Fatalf("the ladder must stop at 1:2, got %s", last.EffectiveGear)
	}
	if !strings.Contains(strings.Join(last.Notes, "; "), "gear ladder") {
		t.Fatalf("a saturated ladder must explain itself, got %v", last.Notes)
	}
}

func TestAutoModeHoldsInsideTheDeadBand(t *testing.T) {
	cases := []struct {
		prefill, decode float64
		want            string
	}{
		{0.50, 0.50, "1:1"},
		{0.55, 0.45, "1:1"},
		// Exactly the dead band still counts as noise, one step past it does not.
		{0.60, 0.40, "1:1"},
		{0.61, 0.39, "2:1"},
	}
	for _, testCase := range cases {
		instance := newPlanner(t, Config{Mode: ModeAuto, Gear: "1:1", MaxChangePerRound: 4})
		decision, err := instance.Plan(Input{Now: time.Now().UTC(), Load: measured(testCase.prefill, testCase.decode)})
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		if decision.EffectiveGear != testCase.want {
			t.Fatalf("pressure %v/%v must hold %s, got %s",
				testCase.prefill, testCase.decode, testCase.want, decision.EffectiveGear)
		}
		if len(decision.Notes) == 0 {
			t.Fatal("a held gear must still be explained")
		}
	}
}

func TestAutoModeHoldsWithoutACompleteLoadSample(t *testing.T) {
	prefill, decode := 0.9, 0.1
	samples := map[string]LoadSnapshot{
		"nothing measured yet": {},
		"gates failed":         {Valid: false, Advice: "等待 P/D 完整指标", PrefillPressure: &prefill, DecodePressure: &decode},
		"one side missing":     {Valid: true, PrefillPressure: &prefill},
	}
	for name, sample := range samples {
		instance := newPlanner(t, Config{Mode: ModeAuto, Gear: "1:1", MaxChangePerRound: 4})
		decision, err := instance.Plan(Input{Now: time.Now().UTC(), Load: sample})
		if err != nil {
			t.Fatalf("%s: plan: %v", name, err)
		}
		if decision.EffectiveGear != "1:1" || decision.TargetRatio != 0.5 {
			t.Fatalf("%s must hold the configured gear, got %s", name, decision.EffectiveGear)
		}
		if len(decision.Notes) == 0 {
			t.Fatalf("%s must say why it held", name)
		}
	}
}

// TestAutoModeGearStepIsPacedByTheCooldown pins the damping: a role change has
// to drain and restart a service before the pressure can reflect it, so the
// ladder may only move once per cooldown window.
func TestAutoModeGearStepIsPacedByTheCooldown(t *testing.T) {
	instance := newPlanner(t, Config{Mode: ModeAuto, Gear: "2:1", MaxChangePerRound: 4, Cooldown: time.Minute})
	now := time.Now().UTC()
	first, err := instance.Plan(Input{Now: now, Load: measured(0.1, 0.9)})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if first.EffectiveGear != "1:1" {
		t.Fatalf("the first round must move to 1:1, got %s", first.EffectiveGear)
	}
	second, err := instance.Plan(Input{Now: now.Add(5 * time.Second), Load: measured(0.1, 0.9)})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if second.EffectiveGear != "1:1" {
		t.Fatalf("the rung moved inside the cooldown: %s", second.EffectiveGear)
	}
	third, err := instance.Plan(Input{Now: now.Add(2 * time.Minute), Load: measured(0.1, 0.9)})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if third.EffectiveGear != "1:2" {
		t.Fatalf("the round after the cooldown must walk on to 1:2, got %s", third.EffectiveGear)
	}
}

// TestAutoModeLoadChangesThePlannedAssignments is the milestone 3 claim itself:
// the measured pressure, not only a human, decides how the pool is split.
func TestAutoModeLoadChangesThePlannedAssignments(t *testing.T) {
	idle := []Candidate{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	counts := func(load LoadSnapshot) (prefill, decode int) {
		instance := newPlanner(t, Config{Mode: ModeAuto, Gear: "1:1", MaxChangePerRound: 10})
		decision, err := instance.Plan(Input{Now: time.Now().UTC(), Idle: idle, Load: load})
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		for _, action := range decision.Actions {
			switch action.Role {
			case domain.RolePrefill:
				prefill++
			case domain.RoleDecode:
				decode++
			}
		}
		if decision.TargetPrefill+decision.TargetDecode != 4 {
			t.Fatalf("four idle instances must all be used, got %d/%d",
				decision.TargetPrefill, decision.TargetDecode)
		}
		return prefill, decode
	}
	// A balanced sample keeps the 1:1 baseline.
	prefill, decode := counts(LoadSnapshot{})
	if prefill != 2 || decode != 2 {
		t.Fatalf("without a sample the pool must stay 2/2, got %d/%d", prefill, decode)
	}
	// A decode-heavy sample walks the gear to 1:2 and the pool follows.
	prefill, decode = counts(measured(0.05, 0.95))
	if prefill != 1 || decode != 3 {
		t.Fatalf("a decode-heavy sample must plan 1/3, got %d/%d", prefill, decode)
	}
}

// pool builds the smallest serving pool that can be rebalanced legally.
func pool() []Candidate {
	now := time.Now().UTC()
	return []Candidate{
		{ID: "p1", Role: domain.RolePrefill, Serving: true, Assigned: true, RoleSince: now.Add(-time.Hour)},
		{ID: "d1", Role: domain.RoleDecode, Serving: true, Assigned: true, RoleSince: now.Add(-time.Hour)},
	}
}

func capacityAction(decision Decision) *Action {
	for index := range decision.Actions {
		switch decision.Actions[index].Kind {
		case ActionProvision, ActionRelease:
			return &decision.Actions[index]
		}
	}
	return nil
}

// TestCapacityActionsAreOffByDefault pins the operational default: on AlayaNeW
// a released container's name cannot be created again, so the planner must not
// hand capacity back (or buy more) unless an operator opts in.
func TestCapacityActionsAreOffByDefault(t *testing.T) {
	instance := newPlanner(t, Config{Mode: ModeAuto, Gear: "2:1", MaxChangePerRound: 4})
	now := time.Now().UTC()
	for _, load := range []LoadSnapshot{measured(0.95, 0.05), measured(0.01, 0.02)} {
		decision, err := instance.Plan(Input{Now: now, Serving: pool(), Idle: []Candidate{{ID: "i1"}}, Load: load})
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		if action := capacityAction(decision); action != nil {
			t.Fatalf("capacity actions are opt-in, got %+v", action)
		}
	}
}

// TestProvisionIsRequestedWhenTheLadderIsSaturated is the "ask for capacity"
// half of milestone 3: the ratio cannot be improved by re-shaping, requests are
// still waiting, so the pool itself is too small.
func TestProvisionIsRequestedWhenTheLadderIsSaturated(t *testing.T) {
	cases := []struct {
		gear            string
		prefill, decode float64
		want            domain.Role
	}{
		{"2:1", 0.95, 0.05, domain.RolePrefill},
		{"1:2", 0.05, 0.95, domain.RoleDecode},
	}
	for _, testCase := range cases {
		instance := newPlanner(t, Config{
			Mode: ModeAuto, Gear: testCase.gear, MaxChangePerRound: 4, CapacityActions: true,
		})
		decision, err := instance.Plan(Input{
			Now: time.Now().UTC(), Serving: pool(), Load: measured(testCase.prefill, testCase.decode),
		})
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		action := capacityAction(decision)
		if action == nil || action.Kind != ActionProvision {
			t.Fatalf("gear %s with a saturated ladder must provision, got %+v (notes %v)",
				testCase.gear, decision.Actions, decision.Notes)
		}
		if action.Role != testCase.want || action.Count != 1 {
			t.Fatalf("provision=%+v, want one %s instance", action, testCase.want)
		}
		if action.Reason == "" {
			t.Fatal("a capacity request must carry the observation that caused it")
		}
	}
}

// TestProvisionWaitsForTheLadderToSettle is the "ratio first" rule: the round
// that moves the ladder must not also buy a container, because the re-shape it
// just asked for has not even been applied yet.
func TestProvisionWaitsForTheLadderToSettle(t *testing.T) {
	instance := newPlanner(t, Config{
		Mode: ModeAuto, Gear: "1:1", MaxChangePerRound: 4, CapacityActions: true,
	})
	now := time.Now().UTC()
	first, err := instance.Plan(Input{Now: now, Serving: pool(), Load: measured(0.95, 0.05)})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if first.EffectiveGear != "2:1" {
		t.Fatalf("the first round must re-shape to 2:1, got %s", first.EffectiveGear)
	}
	if action := capacityAction(first); action != nil {
		t.Fatalf("the round that moved the ladder must not also provision: %+v", action)
	}
	second, err := instance.Plan(Input{Now: now.Add(time.Second), Serving: pool(), Load: measured(0.95, 0.05)})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if action := capacityAction(second); action == nil || action.Kind != ActionProvision {
		t.Fatalf("once the ladder is at its end the pool must be grown, got %+v (notes %v)",
			second.Actions, second.Notes)
	}
}

// TestProvisionWaitsForAnOutstandingRequest is the queueing reality: on a full
// pool the platform can leave a create request QUEUED for hours, so a round
// must not repeat an ask that is already open.
func TestProvisionWaitsForAnOutstandingRequest(t *testing.T) {
	instance := newPlanner(t, Config{
		Mode: ModeAuto, Gear: "2:1", MaxChangePerRound: 4, CapacityActions: true,
	})
	decision, err := instance.Plan(Input{
		Now: time.Now().UTC(), Serving: pool(), Load: measured(0.95, 0.05),
		Capacity: CapacityState{Provisioning: 1},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if action := capacityAction(decision); action != nil {
		t.Fatalf("a second request must wait for the first: %+v", action)
	}
	if !strings.Contains(strings.Join(decision.Notes, "; "), "still open with the partner") {
		t.Fatalf("the hold must be explained, got %v", decision.Notes)
	}
}

// idleLoad is a complete sample with nothing waiting anywhere, which is the
// only situation in which auto mode may hand capacity back.
func idleLoad(prefill, decode float64) LoadSnapshot {
	return LoadSnapshot{Valid: true, PrefillPressure: &prefill, DecodePressure: &decode}
}

// TestReleasePrefersAnUnassignedInstance: giving back an instance that carries
// no role leaves the pool's shape alone and needs no drain.
func TestReleasePrefersAnUnassignedInstance(t *testing.T) {
	instance := newPlanner(t, Config{
		Mode: ModeAuto, Gear: "1:1", MaxChangePerRound: 4, CapacityActions: true,
	})
	decision, err := instance.Plan(Input{
		Now: time.Now().UTC(), Serving: pool(), Idle: []Candidate{{ID: "i1"}, {ID: "i2"}},
		Load: idleLoad(0.01, 0.02),
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	action := capacityAction(decision)
	if action == nil || action.Kind != ActionRelease || action.InstanceID != "i1" {
		t.Fatalf("an idle instance must be handed back first, got %+v (notes %v)", decision.Actions, decision.Notes)
	}
}

// TestReleaseGivesBackTheLessLoadedRole covers the case where every instance
// carries a role: give back the role the load needs least, and never its last
// worker.
func TestReleaseGivesBackTheLessLoadedRole(t *testing.T) {
	now := time.Now().UTC()
	serving := []Candidate{
		{ID: "p1", Role: domain.RolePrefill, Serving: true, Assigned: true, RoleSince: now.Add(-time.Hour)},
		{ID: "p2", Role: domain.RolePrefill, Serving: true, Assigned: true, RoleSince: now.Add(-time.Hour)},
		{ID: "d1", Role: domain.RoleDecode, Serving: true, Assigned: true, RoleSince: now.Add(-time.Hour)},
	}
	instance := newPlanner(t, Config{
		Mode: ModeAuto, Gear: "2:1", MaxChangePerRound: 4, CapacityActions: true,
	})
	decision, err := instance.Plan(Input{Now: now, Serving: serving, Load: idleLoad(0.01, 0.04)})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	action := capacityAction(decision)
	if action == nil || action.Kind != ActionRelease {
		t.Fatalf("an idle pool must hand capacity back, got %+v (notes %v)", decision.Actions, decision.Notes)
	}
	if action.InstanceID != "p1" || action.Role != domain.RolePrefill {
		t.Fatalf("prefill is the less loaded role and the only one with a spare, got %+v", action)
	}
}

func TestReleaseNeverTakesTheLastWorkerOfARole(t *testing.T) {
	instance := newPlanner(t, Config{
		Mode: ModeAuto, Gear: "1:1", MaxChangePerRound: 4, CapacityActions: true,
	})
	decision, err := instance.Plan(Input{Now: time.Now().UTC(), Serving: pool(), Load: idleLoad(0.01, 0.02)})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if action := capacityAction(decision); action != nil {
		t.Fatalf("a released container cannot be created again, so both roles must survive: %+v", action)
	}
	if !strings.Contains(strings.Join(decision.Notes, "; "), "release held") {
		t.Fatalf("the hold must be explained, got %v", decision.Notes)
	}
}

func TestReleaseHoldsWhileAnythingIsStillQueueing(t *testing.T) {
	load := measured(0.01, 0.02)
	load.QueueLength = 1
	instance := newPlanner(t, Config{
		Mode: ModeAuto, Gear: "1:1", MaxChangePerRound: 4, CapacityActions: true,
	})
	decision, err := instance.Plan(Input{
		Now: time.Now().UTC(), Serving: pool(), Idle: []Candidate{{ID: "i1"}}, Load: load,
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if action := capacityAction(decision); action != nil {
		t.Fatalf("one queued request is reason enough to keep the capacity: %+v", action)
	}
}

func TestCapacityActionsNeedAutoMode(t *testing.T) {
	if _, err := New(Config{Gear: "1:1", MaxChangePerRound: 1, CapacityActions: true}); err == nil {
		t.Fatal("capacity actions without a load signal must be rejected")
	}
	if _, err := New(Config{
		Mode: ModeAuto, Gear: "1:1", MaxChangePerRound: 1, CapacityActions: true, AutoReleaseBelow: -0.1,
	}); err == nil {
		t.Fatal("a negative release threshold must be rejected")
	}
}

func TestGearChangesRestartTheAutoLadder(t *testing.T) {
	instance := newPlanner(t, Config{Mode: ModeAuto, Gear: "1:1", MaxChangePerRound: 4})
	if _, err := instance.Plan(Input{Now: time.Now().UTC(), Load: measured(0.9, 0.1)}); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if instance.EffectiveGear() != "2:1" {
		t.Fatalf("auto mode must walk to 2:1, got %s", instance.EffectiveGear())
	}
	// A manual gear change is a new anchor, not a stale rung.
	if err := instance.SetGear("1:2"); err != nil {
		t.Fatalf("set gear: %v", err)
	}
	if instance.EffectiveGear() != "1:2" {
		t.Fatalf("a manual gear must reset the ladder, got %s", instance.EffectiveGear())
	}
	if err := instance.SetMode(ModeFixed); err != nil {
		t.Fatalf("set mode: %v", err)
	}
	if instance.Mode() != ModeFixed {
		t.Fatalf("mode=%s, want fixed", instance.Mode())
	}
	decision, err := instance.Plan(Input{Now: time.Now().UTC(), Load: measured(0.01, 0.99)})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if decision.Mode != ModeFixed || decision.EffectiveGear != "1:2" {
		t.Fatalf("fixed mode must apply the configured gear, got mode=%s effective=%s",
			decision.Mode, decision.EffectiveGear)
	}
	if err := instance.SetMode(ModeAuto); err != nil {
		t.Fatalf("set mode: %v", err)
	}
	if instance.EffectiveGear() != "1:2" {
		t.Fatalf("auto mode must restart from the configured gear, got %s", instance.EffectiveGear())
	}
	if err := instance.SetMode("adaptive"); err == nil {
		t.Fatal("an unknown mode must be rejected")
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
