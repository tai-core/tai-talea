package controller

import (
	"context"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/config"
	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/launcher"
)

// parkedFor is how much quiet time a cooldown test asks for. It is longer than
// the minutes parkInstance burns while spending the budget, so the park itself
// never accidentally satisfies the cooldown.
const parkedFor = 30 * time.Minute

// withCooldown turns the automatic re-arm on for one harness.
func withCooldown(h *harness, cooldown time.Duration, maxRearms int) {
	h.ctrl.cfg.Controller.StartRetryCooldown = config.Duration(cooldown)
	h.ctrl.cfg.Controller.StartRetryMaxRearms = maxRearms
}

// driveToPark runs reconcile rounds until the instance has spent its budget
// again, whatever it was before. It is the loop parkInstance uses, extracted so
// a test can park an instance that has already been re-armed once.
func driveToPark(t *testing.T, h *harness) domain.Instance {
	t.Helper()
	for round := 0; round < 30; round++ {
		current := h.instance("node")
		if current.StartAttempts >= h.ctrl.cfg.Controller.StartMaxAttempts &&
			current.ServiceState == domain.ServiceFailed && !current.StartParkedAt.IsZero() {
			return current
		}
		h.clock.Advance(time.Minute)
		if _, err := h.ctrl.ReconcileOnce(t.Context()); err != nil {
			t.Fatalf("reconcile round %d: %v", round, err)
		}
	}
	t.Fatalf("the instance never parked again: %+v", h.instance("node"))
	return domain.Instance{}
}

func auditActions(t *testing.T, h *harness, instanceID string) map[string]int {
	t.Helper()
	entries, err := h.store.ListAudit(context.Background(), instanceID, 200)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	counts := map[string]int{}
	for _, entry := range entries {
		counts[entry.Action]++
	}
	return counts
}

// A spent budget without a cooldown stays spent. This is the behaviour every
// deployment has today, and turning it into something else by default would
// change how much GPU time a broken launch burns without anyone asking.
func TestSpentBudgetStaysSpentWithoutACooldown(t *testing.T) {
	h := newHarness(t)
	parked := parkInstance(t, h)
	if passed := h.ctrl.cfg.Controller.StartRetryCooldown.Duration(); passed != 0 {
		t.Fatalf("the shipped default cooldown is %s; it must be 0", passed)
	}
	if parked.StartParkedAt.IsZero() {
		t.Fatal("parking must stamp when the budget was spent, even with the cooldown off")
	}
	if parked.StartRearms != 0 {
		t.Fatalf("start_rearms = %d before any cooldown elapsed", parked.StartRearms)
	}

	for round := 0; round < 10; round++ {
		h.clock.Advance(time.Hour)
		if _, err := h.ctrl.ReconcileOnce(t.Context()); err != nil {
			t.Fatalf("reconcile round %d: %v", round, err)
		}
	}
	after := h.instance("node")
	if after.StartAttempts != h.ctrl.cfg.Controller.StartMaxAttempts || after.StartRearms != 0 {
		t.Fatalf("a cooldown of zero re-armed the budget: attempts=%d rearms=%d",
			after.StartAttempts, after.StartRearms)
	}
}

// The point of the mechanism: after the cooldown the control plane grants
// itself a fresh budget instead of waiting for an operator.
func TestCooldownRearmsASpentBudget(t *testing.T) {
	h := newHarness(t)
	withCooldown(h, parkedFor, 3)
	parked := parkInstance(t, h)

	// A park that just happened is not quiet yet.
	h.clock.Advance(parkedFor / 2)
	if _, err := h.ctrl.ReconcileOnce(t.Context()); err != nil {
		t.Fatalf("reconcile inside the cooldown: %v", err)
	}
	early := h.instance("node")
	if early.StartAttempts != h.ctrl.cfg.Controller.StartMaxAttempts || early.StartRearms != 0 {
		t.Fatalf("the budget was re-armed before the cooldown elapsed: attempts=%d rearms=%d",
			early.StartAttempts, early.StartRearms)
	}
	if !early.StartParkedAt.Equal(parked.StartParkedAt) {
		t.Fatalf("the park stamp moved inside the cooldown: %s -> %s",
			parked.StartParkedAt, early.StartParkedAt)
	}

	// Past the cooldown it is granted again, and the park stamp is cleared so
	// the next window is measured from the next failure rather than from this
	// one twice.
	h.clock.Advance(parkedFor)
	if _, err := h.ctrl.ReconcileOnce(t.Context()); err != nil {
		t.Fatalf("reconcile after the cooldown: %v", err)
	}
	rearmed := h.instance("node")
	if rearmed.StartAttempts != 0 {
		t.Fatalf("start attempts = %d after a re-arm, want 0", rearmed.StartAttempts)
	}
	if rearmed.StartRearms != 1 {
		t.Fatalf("start re-arms = %d after one re-arm, want 1", rearmed.StartRearms)
	}
	if !rearmed.StartParkedAt.IsZero() {
		t.Fatalf("the park stamp survived the re-arm: %s", rearmed.StartParkedAt)
	}
	if rearmed.ServiceState != domain.ServiceFailed || rearmed.InstanceState != domain.InstanceIdle {
		t.Fatalf("a re-arm must not invent a live service: %s/%s", rearmed.InstanceState, rearmed.ServiceState)
	}
	if !rearmed.Role.Valid() {
		t.Fatalf("the re-arm dropped the role, which hands recovery to the planner: %+v", rearmed)
	}
	if counts := auditActions(t, h, "node"); counts["service_start_rearmed"] != 1 {
		t.Fatalf("the re-arm left no audit trail: %v", counts)
	}

	// Rounds that do not advance the clock cannot re-arm twice, because there
	// is no park to measure from until the retry fails again.
	for round := 0; round < 3; round++ {
		if _, err := h.ctrl.ReconcileOnce(t.Context()); err != nil {
			t.Fatalf("reconcile round %d: %v", round, err)
		}
	}
	if got := h.instance("node").StartRearms; got != 1 {
		t.Fatalf("start re-arms = %d without a new park, want 1", got)
	}
}

// A re-armed budget is a real attempt: when the machine is healthy again the
// service comes up, and the debt that parked it is cleared so it can never
// accumulate across recoveries.
func TestReArmedBudgetCanRecoverTheService(t *testing.T) {
	h := newHarness(t)
	withCooldown(h, parkedFor, 3)
	parkInstance(t, h)
	h.clock.Advance(parkedFor + time.Minute)
	if _, err := h.ctrl.ReconcileOnce(t.Context()); err != nil {
		t.Fatalf("re-arm: %v", err)
	}

	// The machine is free now: the process the earlier attempts were missing.
	h.bootstrap.mu.Lock()
	h.bootstrap.failStart = false
	h.bootstrap.phase = launcher.PhaseRunning
	h.bootstrap.servicePID = 4242
	h.bootstrap.mu.Unlock()
	if _, err := h.ctrl.ReconcileOnce(t.Context()); err != nil {
		t.Fatalf("reconcile with a healthy process: %v", err)
	}

	recovered := h.instance("node")
	if recovered.ServiceState != domain.ServiceServing {
		t.Fatalf("service_state = %s, want SERVING after a re-arm and a healthy round", recovered.ServiceState)
	}
	if recovered.StartRearms != 0 || !recovered.StartParkedAt.IsZero() {
		t.Fatalf("a serving instance carries start debt: rearms=%d parked_at=%s",
			recovered.StartRearms, recovered.StartParkedAt)
	}
}

// The bound. However long the instance sits there, the control plane re-arms it
// start_retry_max_rearms times and then escalates to an operator. Without this
// a launch that is broken for its own reasons reloads every GPU on the node
// once per cooldown, forever.
func TestReArmCountIsBounded(t *testing.T) {
	h := newHarness(t)
	const maxRearms = 2
	withCooldown(h, parkedFor, maxRearms)
	parkInstance(t, h)

	for want := 1; want <= maxRearms; want++ {
		h.clock.Advance(parkedFor + time.Minute)
		if _, err := h.ctrl.ReconcileOnce(t.Context()); err != nil {
			t.Fatalf("re-arm %d: %v", want, err)
		}
		if got := h.instance("node").StartRearms; got != want {
			t.Fatalf("start re-arms = %d, want %d", got, want)
		}
		// The retry fails again, which is what makes this a broken launch
		// rather than a recovered one.
		driveToPark(t, h)
	}

	// Spent, and now bounded: no amount of quiet time grants another.
	for round := 0; round < 5; round++ {
		h.clock.Advance(24 * time.Hour)
		if _, err := h.ctrl.ReconcileOnce(t.Context()); err != nil {
			t.Fatalf("reconcile past the cap: %v", err)
		}
	}
	final := h.instance("node")
	if final.StartRearms != maxRearms {
		t.Fatalf("start re-arms = %d past the cap, want %d", final.StartRearms, maxRearms)
	}
	if final.StartAttempts != h.ctrl.cfg.Controller.StartMaxAttempts {
		t.Fatalf("start attempts = %d past the cap, want the budget spent (%d)",
			final.StartAttempts, h.ctrl.cfg.Controller.StartMaxAttempts)
	}
	if counts := auditActions(t, h, "node"); counts["service_start_rearmed"] != maxRearms {
		t.Fatalf("re-arm audit count = %d, want %d", counts["service_start_rearmed"], maxRearms)
	}
	// The escalation an operator answers still happened, once per park.
	if counts := auditActions(t, h, "node"); counts["service_start_failed"] == 0 {
		t.Fatal("no start failures were audited; the escalation path is missing")
	}
}

// The operator's own answer must clear the same debt, otherwise a requeue into
// a launch that fails once more would park with a counter that was never reset.
func TestOperatorRequeueAlsoClearsTheReArmDebt(t *testing.T) {
	h := newHarness(t)
	withCooldown(h, parkedFor, 3)
	parkInstance(t, h)
	h.clock.Advance(parkedFor + time.Minute)
	if _, err := h.ctrl.ReconcileOnce(t.Context()); err != nil {
		t.Fatalf("re-arm: %v", err)
	}
	driveToPark(t, h)

	if err := h.ctrl.RequeueService(t.Context(), "node", "operator took over"); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	requeued := h.instance("node")
	if requeued.StartRearms != 0 || !requeued.StartParkedAt.IsZero() {
		t.Fatalf("a requeue left start debt behind: rearms=%d parked_at=%s",
			requeued.StartRearms, requeued.StartParkedAt)
	}
}

// The park stamp is a property of a spent budget, not of any failure: a failure
// with attempts left must not leave one behind, or the cooldown would be
// measured from an attempt the reconciler is still retrying on its own.
func TestOnlyASpentBudgetStampsThePark(t *testing.T) {
	h := newHarness(t)
	withCooldown(h, parkedFor, 3)
	h.handle(h.addedEvent("node", h.bootstrap.Endpoint(), "lease", h.clock.Now()))
	if err := h.ctrl.StartService(t.Context(), "node", domain.RoleDecode); err != nil {
		t.Fatalf("first start: %v", err)
	}
	h.bootstrap.mu.Lock()
	h.bootstrap.phase = launcher.PhaseFailed
	h.bootstrap.servicePID = 0
	h.bootstrap.failStart = true
	h.bootstrap.mu.Unlock()

	// Exactly one round: enough to fail the attempt, not enough to spend the
	// budget.
	h.clock.Advance(time.Minute)
	if _, err := h.ctrl.ReconcileOnce(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	hit := h.instance("node")
	if hit.ServiceState != domain.ServiceFailed {
		t.Fatalf("service_state = %s, want FAILED after one blown attempt", hit.ServiceState)
	}
	if hit.StartAttempts >= h.ctrl.cfg.Controller.StartMaxAttempts {
		t.Fatalf("one round spent the whole budget (%d attempts)", hit.StartAttempts)
	}
	if !hit.StartParkedAt.IsZero() {
		t.Fatalf("an intermediate failure stamped a park: %s", hit.StartParkedAt)
	}
	h.clock.Advance(24 * time.Hour)
	if _, err := h.ctrl.ReconcileOnce(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := h.instance("node").StartRearms; got != 0 {
		t.Fatalf("start re-arms = %d while the budget was never spent", got)
	}
}
