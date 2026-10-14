package planner

import (
	"strings"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
)

// A worker the control plane already owns and is restarting is not a worker it
// has not ordered. Queueing pressure cannot tell the two apart, so without this
// the round buys a container to sit next to one that is already coming back.
func TestProvisionWaitsForAWorkerThatIsBeingRestarted(t *testing.T) {
	instance := newPlanner(t, Config{
		Mode: ModeAuto, Gear: "2:1", MaxChangePerRound: 4, CapacityActions: true,
	})
	decision, err := instance.Plan(Input{
		Now: time.Now().UTC(), Serving: pool(), Load: measured(0.95, 0.05),
		Recovering: []Candidate{{
			ID: "d2", Role: domain.RoleDecode, NotServing: true, RoleSince: time.Now().UTC().Add(-time.Hour),
		}},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if action := capacityAction(decision); action != nil {
		t.Fatalf("capacity was bought while a worker was already being restarted: %+v", action)
	}
	if !strings.Contains(strings.Join(decision.Notes, "; "), "already hold a role and are being restarted") {
		t.Fatalf("the hold must name its reason, got %v", decision.Notes)
	}
	if !strings.Contains(strings.Join(decision.Notes, "; "), "d2") {
		t.Fatalf("the hold must name the instance it is waiting for, got %v", decision.Notes)
	}

	// The gate opens the moment nothing is recovering: the shortfall is real
	// then, and the pool has to grow.
	open, err := instance.Plan(Input{Now: time.Now().UTC(), Serving: pool(), Load: measured(0.95, 0.05)})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if action := capacityAction(open); action == nil || action.Kind != ActionProvision {
		t.Fatalf("provisioning must resume once nothing is recovering, got %+v (notes %v)",
			open.Actions, open.Notes)
	}
}

// A parked worker is not capacity, and the round must not pretend it is: the
// ratio target describes what can serve traffic now. Counting it is how a
// degraded pool gets planned as a whole one.
func TestRecoveringCapacityIsNotCountedAsServing(t *testing.T) {
	instance := newPlanner(t, Config{Mode: ModeFixed, Gear: "1:1", MaxChangePerRound: 4})
	decision, err := instance.Plan(Input{
		Now:        time.Now().UTC(),
		Serving:    []Candidate{{ID: "p1", Role: domain.RolePrefill, Serving: true, Assigned: true, RoleSince: time.Now().UTC().Add(-time.Hour)}},
		Recovering: []Candidate{{ID: "d1", Role: domain.RoleDecode, NotServing: true}},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if decision.ServingCount != 1 {
		t.Fatalf("serving_count = %d, want only the worker that can take traffic", decision.ServingCount)
	}
	if decision.RecoveringCount != 1 {
		t.Fatalf("recovering_count = %d, want the parked worker reported", decision.RecoveringCount)
	}
	// The pool is one worker. The last-worker guard is what keeps the round from
	// trading the only Prefill for a Decode and stopping traffic entirely.
	for _, action := range decision.Actions {
		if action.Kind == ActionReassign {
			t.Fatalf("the planner moved the only serving worker to fill a gap that is already being filled: %+v", action)
		}
	}
}

// Capacity parked for good is a hole in the pool, not a worker on its way back.
// It is reported so the operator can see it, and it never holds back the ask
// for capacity that would actually restore service.
func TestStrandedCapacityIsReportedButDoesNotHoldProvisioning(t *testing.T) {
	instance := newPlanner(t, Config{
		Mode: ModeAuto, Gear: "2:1", MaxChangePerRound: 4, CapacityActions: true,
	})
	decision, err := instance.Plan(Input{
		Now: time.Now().UTC(), Serving: pool(), Load: measured(0.95, 0.05),
		Stranded: []Candidate{{ID: "d9", Role: domain.RoleDecode, NotServing: true}},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if decision.StrandedCount != 1 {
		t.Fatalf("stranded_count = %d, want 1", decision.StrandedCount)
	}
	if !strings.Contains(strings.Join(decision.Notes, "; "), "an operator has to requeue or release them") {
		t.Fatalf("the escalation must be stated, got %v", decision.Notes)
	}
	if decision.RecoveringCount != 0 {
		t.Fatalf("recovering_count = %d for an instance nothing will retry", decision.RecoveringCount)
	}
	if action := capacityAction(decision); action == nil || action.Kind != ActionProvision {
		t.Fatalf("a hole in the pool must still be filled, got %+v (notes %v)", decision.Actions, decision.Notes)
	}
}

// A stuck worker must not silence the capacity verbs for good: hand-back is
// suppressed just as provisioning is, so an instance that never comes back
// cannot pin idle money in place.
func TestStrandedCapacityDoesNotBlockRelease(t *testing.T) {
	instance := newPlanner(t, Config{
		Mode: ModeAuto, Gear: "1:1", MaxChangePerRound: 4, CapacityActions: true, AutoReleaseBelow: 0.1,
	})
	decision, err := instance.Plan(Input{
		Now: time.Now().UTC(), Serving: pool(), Idle: []Candidate{{ID: "i1"}},
		Stranded: []Candidate{{ID: "d9", Role: domain.RoleDecode, NotServing: true}},
		Load:     idleLoad(0.01, 0.02),
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	action := capacityAction(decision)
	if action == nil || action.Kind != ActionRelease || action.InstanceID != "i1" {
		t.Fatalf("an idle instance must still be handed back, got %+v (notes %v)", decision.Actions, decision.Notes)
	}
}

// The summary line is the one an operator reads in the log to understand why a
// pool is short. It has to name all three populations.
func TestDecisionSummaryNamesEveryPopulation(t *testing.T) {
	decision := Decision{
		Gear: "1:1", TargetPrefill: 1, TargetDecode: 1,
		ServingCount: 1, IdleCount: 2, RecoveringCount: 3, StrandedCount: 4,
		Actions: []Action{{Kind: ActionDrain}},
	}
	summary := decision.DecisionSummary()
	for _, want := range []string{"serving=1", "idle=2", "recovering=3", "stranded=4"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q does not report %q", summary, want)
		}
	}
}

// candidateIDs is bounded so a large pool cannot turn one log line into a page.
func TestCandidateIDsIsBounded(t *testing.T) {
	candidates := make([]Candidate, 0, 9)
	for index := 0; index < 9; index++ {
		candidates = append(candidates, Candidate{ID: string(rune('a' + index))})
	}
	rendered := candidateIDs(candidates)
	if !strings.Contains(rendered, "and 5 more") {
		t.Fatalf("candidateIDs = %q, want a bounded rendering", rendered)
	}
	if strings.Count(rendered, ",") > 4 {
		t.Fatalf("candidateIDs = %q, want at most four names", rendered)
	}
	if got := candidateIDs(nil); got != "" {
		t.Fatalf("candidateIDs(nil) = %q", got)
	}
}
