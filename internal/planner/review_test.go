package planner

import (
	"testing"

	"github.com/tai-core/tai-talea/internal/domain"
)

func TestSurplusDrainHonorsRoundCapAndDoesNotReuseDrainedWorker(t *testing.T) {
	for _, limit := range []int{1, 10} {
		p := newPlanner(t, Config{Gear: "1:1", MaxChangePerRound: limit, MaxServing: 2})
		decision, err := p.Plan(Input{Serving: []Candidate{
			{ID: "a", Role: domain.RolePrefill}, {ID: "b", Role: domain.RolePrefill},
			{ID: "c", Role: domain.RoleDecode}, {ID: "d", Role: domain.RoleDecode},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if len(decision.Actions) > limit {
			t.Fatalf("round limit %d exceeded: %+v", limit, decision.Actions)
		}
		seen := map[string]bool{}
		for _, action := range decision.Actions {
			if seen[action.InstanceID] {
				t.Fatalf("node changed twice in one round: %+v", decision.Actions)
			}
			seen[action.InstanceID] = true
		}
	}
}

func TestPlannerPreservesBothRolesInTwoNodeCluster(t *testing.T) {
	p := newPlanner(t, Config{Gear: "1:2", MaxChangePerRound: 4})
	decision, err := p.Plan(Input{Idle: []Candidate{{ID: "a"}, {ID: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	if decision.TargetPrefill != 1 || decision.TargetDecode != 1 {
		t.Fatalf("two nodes must form a usable PD pair: %+v", decision)
	}
}

func TestIdleAssignmentCannotExceedMaxServing(t *testing.T) {
	p := newPlanner(t, Config{Gear: "1:1", MaxChangePerRound: 4, MaxServing: 2})
	decision, err := p.Plan(Input{
		Serving: []Candidate{{ID: "a", Role: domain.RolePrefill}, {ID: "b", Role: domain.RolePrefill}},
		Idle:    []Candidate{{ID: "c"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range decision.Actions {
		if action.Kind == ActionAssign {
			t.Fatalf("already at capacity cap: %+v", decision.Actions)
		}
	}
}
