// Package planner implements the capacity planner. It has two independent
// shapes and a deployment picks one of them:
//
//   - disaggregated: a Prefill/Decode ratio with manual gears, change caps,
//     minimum hold time, cooldown and a preference for idle instances. An
//     optional automatic mode walks the gear ladder from measured pressure.
//   - aggregated: every available instance runs the same self-contained
//     worker. There is no ratio to keep, so the whole gear machinery - ladder,
//     ratio targets, role conversion - is out of the picture and the planner
//     only has to make the pool's shape match the number of containers the
//     partner provided.
//
// Development document §11 is explicit that milestone 1 must not claim adaptive
// Prefill/Decode sizing, so the fixed gear remains the default and the only
// behaviour unless auto mode is switched on. Milestone 3 adds that mode: it
// consumes the measured LoadSnapshot and walks the gear ladder towards the role
// with the higher scheduler pressure, one rung per cooldown window.
//
// Aggregated mode was added for deployments whose throughput is best with a
// single process per GPU (GLM-5.3-Flash on B300: four TP4 replicas beat the
// disaggregated layouts, and speculative decoding does not start under PD at
// this cut). It is a separate branch rather than a ratio of 1:0 because a
// regular worker is not a prefill worker with the decode half missing: it has
// no KV peer, the bootstrap must not pass --disaggregation-mode, and the
// Router files it under a third worker type.
package planner

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
)

// Gears maps the manual gear names to the Prefill share of total serving
// instances. "2:1" means two Prefill workers for every Decode worker.
var Gears = map[string]float64{
	"1:1": 0.5,
	"2:1": 2.0 / 3.0,
	"1:2": 1.0 / 3.0,
}

// Planner modes.
const (
	// ModeFixed applies the configured gear and never moves the ratio on its own.
	ModeFixed = "fixed"
	// ModeAuto walks the gear ladder towards the role that reports the higher
	// scheduler pressure. It needs a complete P/D load sample to act at all,
	// and it moves at most one rung per cooldown window.
	ModeAuto = "auto"
	// ModeAggregated drops the ratio entirely: every available instance runs
	// the same self-contained worker and the pool is exactly as wide as the
	// partner made it. Gear, load and capacity actions do not apply.
	ModeAggregated = "aggregated"
)

// DefaultDeadBand is the pressure gap below which auto mode keeps the current
// gear. Pressure is a share of a concurrency budget, so a planner that reacts
// to a couple of percent of it would mostly chase its own noise. The value
// matches the threshold the sampler itself uses in telemetry.Snapshot before it
// advises an operator to add capacity.
const DefaultDeadBand = 0.2

// DefaultReleaseBelow is how idle both roles must be before auto mode offers
// capacity back. It is deliberately far below DefaultDeadBand: asking for
// capacity again costs a queue position, not just money.
const DefaultReleaseBelow = 0.1

// ModeNames returns the supported planner modes in a stable order. A console
// that offers a mode switch should read this rather than hard-code the list,
// because a new shape is a value here and nothing else.
func ModeNames() []string { return []string{ModeFixed, ModeAuto, ModeAggregated} }

// GearNames returns the supported gears in a stable order.
func GearNames() []string {
	names := make([]string, 0, len(Gears))
	for name := range Gears {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// gearLadder lists the gears in ascending Prefill share, which is the order
// auto mode walks: from decode-heavy to prefill-heavy.
func gearLadder() []string {
	ladder := GearNames()
	sort.Slice(ladder, func(i, j int) bool {
		if Gears[ladder[i]] == Gears[ladder[j]] {
			return ladder[i] < ladder[j]
		}
		return Gears[ladder[i]] < Gears[ladder[j]]
	})
	return ladder
}

// stepGear returns the gear step rungs away from current, if it exists.
func stepGear(current string, step int) (string, bool) {
	if step == 0 {
		return current, false
	}
	ladder := gearLadder()
	for index, name := range ladder {
		if name != current {
			continue
		}
		if next := index + step; next >= 0 && next < len(ladder) {
			return ladder[next], true
		}
		return current, false
	}
	return current, false
}

// Config is the planner tuning surface.
type Config struct {
	// Mode is ModeFixed (default), ModeAuto or ModeAggregated.
	Mode string `json:"mode" yaml:"mode"`
	// Gear is the manual ratio gear, for example "1:1". Auto mode starts the
	// ladder from it. Aggregated mode ignores it and accepts it being empty.
	Gear string `json:"gear" yaml:"gear"`
	// MaxChangePerRound caps how many instances may change role per round.
	MaxChangePerRound int `json:"max_change_per_round" yaml:"max_change_per_round"`
	// MinHold is the minimum time an instance keeps an assigned role.
	MinHold time.Duration `json:"min_hold" yaml:"min_hold"`
	// Cooldown is the minimum time between two planner rounds that change anything.
	// In auto mode it also paces the ladder, because a gear step counts as a change.
	Cooldown time.Duration `json:"cooldown" yaml:"cooldown"`
	// MaxServing optionally caps how many instances serve traffic at once.
	// Zero means "use every available instance", which is the milestone 1
	// behaviour: we never ask the partner for capacity we do not have. In
	// aggregated mode it is the only sizing knob there is: the pool serves
	// min(available, max_serving) regular workers.
	MaxServing int `json:"max_serving" yaml:"max_serving"`
	// AutoDeadBand is the minimum |prefill_pressure - decode_pressure| that makes
	// auto mode move a step. Zero selects DefaultDeadBand.
	AutoDeadBand float64 `json:"auto_dead_band" yaml:"auto_dead_band"`
	// CapacityActions lets auto mode ask the partner for one more instance when
	// the gear ladder is saturated, and give one back when the pool is idle.
	//
	// Off by default, and that default is the honest one on AlayaNeW: a released
	// container's name cannot be created again (status 212035), and the 8-GPU
	// pool can leave a new request QUEUED for hours because the platform never
	// cancels a queued creation. Releasing capacity is a decision an operator has
	// to opt into.
	CapacityActions bool `json:"capacity_actions" yaml:"capacity_actions"`
	// AutoReleaseBelow is the pressure below which both roles count as idle in
	// auto mode. Zero selects DefaultReleaseBelow.
	AutoReleaseBelow float64 `json:"auto_release_below" yaml:"auto_release_below"`
}

// normalisedMode reports the active mode, treating the unset value as fixed so
// configurations written before auto mode existed keep their behaviour.
func (c Config) normalisedMode() string {
	switch c.Mode {
	case ModeAuto:
		return ModeAuto
	case ModeAggregated:
		return ModeAggregated
	default:
		return ModeFixed
	}
}

// aggregated reports whether the ratio machinery is out of the picture.
func (c Config) aggregated() bool { return c.normalisedMode() == ModeAggregated }

// deadBand returns the active dead band, falling back to DefaultDeadBand so
// auto mode can be switched on without tuning anything.
func (c Config) deadBand() float64 {
	if c.AutoDeadBand > 0 {
		return c.AutoDeadBand
	}
	return DefaultDeadBand
}

// releaseBelow returns the active idle threshold, falling back to
// DefaultReleaseBelow.
func (c Config) releaseBelow() float64 {
	if c.AutoReleaseBelow > 0 {
		return c.AutoReleaseBelow
	}
	return DefaultReleaseBelow
}

// Validate rejects impossible planner configurations.
func (c Config) Validate() error {
	switch c.Mode {
	case "", ModeFixed, ModeAuto, ModeAggregated:
	default:
		return fmt.Errorf("planner mode must be %q, %q or %q, got %q",
			ModeFixed, ModeAuto, ModeAggregated, c.Mode)
	}
	if !c.aggregated() {
		// A gear is meaningless without a ratio to keep, so requiring one in
		// aggregated mode would force operators to write a value the planner
		// never reads - and then quietly disagree with it.
		if _, ok := Gears[c.Gear]; !ok {
			return fmt.Errorf("planner gear must be one of %v, got %q", GearNames(), c.Gear)
		}
	}
	if c.MaxChangePerRound <= 0 {
		return errors.New("planner max change per round must be positive")
	}
	if c.MinHold < 0 || c.Cooldown < 0 {
		return errors.New("planner min hold and cooldown must not be negative")
	}
	if c.MaxServing < 0 {
		return errors.New("planner max serving must not be negative")
	}
	if c.AutoDeadBand < 0 {
		return errors.New("planner auto dead band must not be negative")
	}
	if c.AutoReleaseBelow < 0 {
		return errors.New("planner auto release threshold must not be negative")
	}
	if c.CapacityActions && c.normalisedMode() != ModeAuto {
		return errors.New("planner capacity actions need auto mode: a fixed gear has no load signal to scale against")
	}
	return nil
}

// LoadSnapshot carries measured scheduler load. Auto mode uses it to choose the
// gear; fixed mode only records it, so the console can show what the planner saw
// without the planner having acted on it.
type LoadSnapshot struct {
	Valid                bool      `json:"valid"`
	MeasuredAt           time.Time `json:"measured_at"`
	Advice               string    `json:"advice"`
	PrefillPressure      *float64  `json:"prefill_pressure"`
	DecodePressure       *float64  `json:"decode_pressure"`
	InputTokenRate       *float64  `json:"input_tokens_per_second"`
	OutputTokenRate      *float64  `json:"output_tokens_per_second"`
	RequestThroughput    float64   `json:"request_throughput,omitempty"`
	PrefillLatencyMS     float64   `json:"prefill_latency_ms,omitempty"`
	DecodeLatencyMS      float64   `json:"decode_latency_ms,omitempty"`
	TokenRate            float64   `json:"token_rate,omitempty"`
	QueueLength          float64   `json:"queue_length"`
	WorkerGPUUtilization float64   `json:"worker_gpu_utilization,omitempty"`
	PDLoadImbalance      float64   `json:"pd_load_imbalance"`
}

// Candidate is one instance the planner may consider.
type Candidate struct {
	ID string
	// Role is the currently assigned role, empty when no role is assigned.
	Role domain.Role
	// Serving reports whether the instance currently serves Router traffic.
	Serving bool
	// RoleSince is when the current role was assigned.
	RoleSince time.Time
	// Assigned is the profile of a live PD worker used by the Router.
	Assigned bool
	// NotServing marks an instance that holds a role but cannot take traffic
	// right now: the model process failed, or the container stopped answering.
	// It is capacity the control plane already owns and cannot use yet, which
	// is a different thing from capacity that does not exist.
	NotServing bool
}

// CapacityState is what the planner knows about capacity it has already asked
// for. A controller round can be minutes or hours ahead of the platform: on a
// full AlayaNeW pool a create request waits in QUEUED until a GPU frees up,
// while the container is invisible to every other part of the system.
type CapacityState struct {
	// Provisioning counts instances that were requested from the partner and are
	// not serving or idle yet.
	Provisioning int `json:"provisioning"`
}

// Input is one planner round input.
type Input struct {
	Now time.Time
	// Serving lists instances that already hold a PD role and can take traffic.
	Serving []Candidate
	// Idle lists prepared instances without a role.
	Idle []Candidate
	// Recovering lists instances that hold a role but are not serving, where
	// something will try again without an operator: the reconciler still has
	// start attempts, or a configured cooldown will re-arm the budget, or a
	// container that stopped answering will be re-entered into preparation when
	// it does. They are deliberately not part of Serving - a pool short of a
	// worker must not be described as whole - and not part of Idle, because an
	// instance the reconciler owns must not be raced by the planner. They exist
	// so the pool's true shape is visible: without them a parked worker looks
	// exactly like a worker that was never created, and the next capacity
	// decision is made on a number that is wrong.
	Recovering []Candidate
	// Stranded lists instances in the same shape where nothing will try again
	// without an operator - the start budget is spent and no re-arm is coming.
	// They are reported and never moved: the escalation an operator answers.
	Stranded []Candidate
	// Load is the latest scheduler sample. Auto mode reads it; fixed mode only
	// echoes it back through the decision.
	Load LoadSnapshot
	// Capacity reports capacity requests that are still open, so a round does
	// not repeat an ask the partner has not answered yet.
	Capacity CapacityState
}

// ActionKind enumerates the planner's output verbs.
type ActionKind string

const (
	// ActionAssign starts a service on an idle instance.
	ActionAssign ActionKind = "ASSIGN"
	// ActionReassign converts a serving instance to the other role.
	ActionReassign ActionKind = "REASSIGN"
	// ActionDrain removes an instance from serving, returning it to idle.
	ActionDrain ActionKind = "DRAIN"
	// ActionProvision asks the partner for one more instance. The planner decides
	// that it is needed; the executor that owns the onboarding pipeline performs
	// it, because a create can stay queued for hours.
	ActionProvision ActionKind = "PROVISION"
	// ActionRelease gives one instance back to the partner.
	ActionRelease ActionKind = "RELEASE"
)

// Action is one planned change.
type Action struct {
	Kind       ActionKind `json:"kind"`
	InstanceID string     `json:"instance_id"`
	// Role is the target role for ASSIGN/REASSIGN, the role that is short for
	// PROVISION and the role being handed back for RELEASE.
	Role domain.Role `json:"role,omitempty"`
	// FromRole is the current role for REASSIGN/DRAIN/RELEASE.
	FromRole domain.Role `json:"from_role,omitempty"`
	// Count is how many instances a PROVISION asks for. It is 1 for every other
	// verb, which act on exactly one instance.
	Count  int    `json:"count,omitempty"`
	Reason string `json:"reason"`
}

// Decision is the planner output for one round.
type Decision struct {
	ObservedLoad LoadSnapshot `json:"observed_load"`
	Mode         string       `json:"mode"`
	// Gear is the configured gear. EffectiveGear is the gear this round used:
	// the same value in fixed mode, and the rung the load moved the planner to
	// in auto mode.
	Gear          string  `json:"gear"`
	EffectiveGear string  `json:"effective_gear"`
	TargetRatio   float64 `json:"target_ratio"`
	CurrentRatio  float64 `json:"current_ratio"`
	TargetPrefill int     `json:"target_prefill"`
	TargetDecode  int     `json:"target_decode"`
	// TargetRegular is the aggregated mode target and is zero in every PD
	// mode. It is a separate field rather than "all of the above" because a
	// console that renders a P/D ratio must be able to tell the two shapes
	// apart; a regular pool has no ratio to draw.
	TargetRegular int     `json:"target_regular,omitempty"`
	ServingCount  int     `json:"serving_count"`
	IdleCount     int     `json:"idle_count"`
	// RecoveringCount and StrandedCount describe capacity the control plane owns
	// that is not serving and not idle. They are reported rather than acted on:
	// a pool that is short a worker has to say whether it is short because the
	// partner has not delivered one or because one it already delivered is
	// being restarted.
	RecoveringCount int       `json:"recovering_count,omitempty"`
	StrandedCount   int       `json:"stranded_count,omitempty"`
	Actions         []Action  `json:"actions"`
	Notes           []string  `json:"notes,omitempty"`
	PlannedAt       time.Time `json:"planned_at"`
}

// Planner applies a ratio gear to the current worker population. In auto mode
// the gear itself follows the measured load, one rung per cooldown window.
type Planner struct {
	lastLoad LoadSnapshot
	config   Config
	mu       sync.Mutex
	// lastChangeAt is the timestamp of the last round that produced changes.
	lastChangeAt time.Time
	// autoGear is the rung auto mode reached. Empty means "start from the
	// configured gear", which is the state before the first auto round.
	autoGear string
}

// New validates the configuration and builds a planner.
func New(config Config) (*Planner, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Planner{config: config}, nil
}

// Config returns the active configuration.
func (p *Planner) Config() Config { p.mu.Lock(); defer p.mu.Unlock(); return p.config }

// Mode reports the active mode.
func (p *Planner) Mode() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.config.normalisedMode()
}

// EffectiveGear reports the gear the next round starts from. Auto mode reports
// the rung the load moved the planner to, which is not the configured gear.
func (p *Planner) EffectiveGear() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gear()
}

// LastObservedLoad is the input used by the last plan, with its sample timestamp.
func (p *Planner) LastObservedLoad() LoadSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastLoad
}

// SetGear switches the manual gear. It is also where auto mode restarts from,
// so a manual gear change is never silently overridden by a stale rung.
func (p *Planner) SetGear(gear string) error {
	if _, ok := Gears[gear]; !ok {
		return fmt.Errorf("planner gear must be one of %v, got %q", GearNames(), gear)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.config.Gear = gear
	p.autoGear = gear
	return nil
}

// SetMode switches between the fixed, automatic and aggregated planners. An
// empty mode selects the fixed default.
func (p *Planner) SetMode(mode string) error {
	if mode == "" {
		mode = ModeFixed
	}
	if mode != ModeFixed && mode != ModeAuto && mode != ModeAggregated {
		return fmt.Errorf("planner mode must be %q, %q or %q, got %q",
			ModeFixed, ModeAuto, ModeAggregated, mode)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.config.Mode = mode
	p.autoGear = p.config.Gear
	return nil
}

// gear is the gear the round starts from.
func (p *Planner) gear() string {
	if p.config.normalisedMode() == ModeAuto && p.autoGear != "" {
		return p.autoGear
	}
	return p.config.Gear
}

// LastChangeAt returns when the planner last produced an action.
func (p *Planner) LastChangeAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastChangeAt
}

// autoStep walks one rung of the gear ladder towards the role that reports the
// higher scheduler pressure.
//
// It returns the gear for this round, the notes that explain the choice and
// whether the gear moved. Three conditions keep it calm and honest:
//
//   - the sample must be complete, because an incomplete one is not evidence of
//     an imbalance;
//   - something must actually be queueing, because a pressure difference with
//     nothing waiting only reflects concurrency occupancy, not a shortage. The
//     sampler says the same thing in its own advice text;
//   - the pressure gap must clear the dead band, so measurement noise cannot
//     move the ladder.
//
// A move is one rung, paced by the cooldown, because a role change has to drain
// and restart a service before the pressure it answers can reflect it.
func (p *Planner) autoStep(load LoadSnapshot) (string, []string, bool) {
	current := p.gear()
	if !load.Valid || load.PrefillPressure == nil || load.DecodePressure == nil {
		advice := load.Advice
		if advice == "" {
			advice = "no complete P/D sample yet"
		}
		return current, []string{fmt.Sprintf("auto mode holds %s: %s", current, advice)}, false
	}
	if load.QueueLength <= 0 {
		return current, []string{fmt.Sprintf(
			"auto mode holds %s: nothing is queued, and a pressure gap without queueing is not a capacity shortage",
			current)}, false
	}
	gap := *load.PrefillPressure - *load.DecodePressure
	band := p.config.deadBand()
	if math.Abs(gap) <= band {
		return current, []string{fmt.Sprintf(
			"auto mode holds %s: pressure gap %+.3f is inside the %.3f dead band",
			current, gap, band)}, false
	}
	step := 1
	end := "prefill-heavy"
	if gap < 0 {
		step = -1
		end = "decode-heavy"
	}
	next, ok := stepGear(current, step)
	if !ok {
		return current, []string{fmt.Sprintf(
			"auto mode holds %s: already the %s end of the gear ladder", current, end)}, false
	}
	return next, []string{fmt.Sprintf(
		"auto mode moves %s -> %s: prefill pressure %.3f and decode pressure %.3f with %.0f queued (gap %+.3f)",
		current, next, *load.PrefillPressure, *load.DecodePressure, load.QueueLength, gap)}, true
}

// capacityStep asks for more capacity, or offers some back, when the load says
// the pool itself is the wrong size rather than the wrong shape.
//
// The gear ladder can only re-shape what the partner already gave us. When the
// ladder is saturated in the direction the load wants and requests are still
// queueing, the ratio is as good as it can get and the pool is short. When
// nothing is queueing anywhere and both roles sit far below the dead band, part
// of the pool is idle money. Either way the planner only decides: the executor
// that owns the partner credentials and the SSH onboarding pipeline performs it,
// because a create can stay queued for hours and must not block a round.
//
// rungMoved suppresses provisioning for the round that moved the ladder. The
// ratio is the cheap answer and it comes first: asking for a container before
// the pool has even been re-shaped would buy capacity on evidence that the
// re-shape might have answered.
func (p *Planner) capacityStep(input Input, planned []Action, rungMoved bool) (*Action, []string) {
	load := input.Load
	if !load.Valid || load.PrefillPressure == nil || load.DecodePressure == nil {
		return nil, nil
	}
	if input.Capacity.Provisioning > 0 {
		return nil, []string{fmt.Sprintf(
			"capacity held: %d provision request(s) are still open with the partner",
			input.Capacity.Provisioning)}
	}
	// A worker that is being restarted is not a worker that was never ordered.
	// Queueing pressure alone cannot tell the two apart, and confusing them buys
	// a container to sit next to one that is already coming back: on AlayaNeW
	// that is a queued creation measured in hours, and on the H100 nodes it is
	// eight cards held twice. The wait is bounded by construction - Recovery
	// lasts exactly as long as something will try again - and the moment the
	// instance either serves or is stranded, this gate opens.
	if len(input.Recovering) > 0 {
		return nil, []string{fmt.Sprintf(
			"capacity held: %d instance(s) already hold a role and are being restarted (%s), so the shortfall may be temporary",
			len(input.Recovering), candidateIDs(input.Recovering))}
	}
	if len(planned) >= p.config.MaxChangePerRound {
		return nil, nil
	}
	if !rungMoved {
		if action, note := p.provisionStep(input, load); action != nil || note != nil {
			return action, note
		}
	}
	return p.releaseStep(input, load, planned)
}

// provisionStep asks for one instance of the role the ladder cannot help any
// more. It never asks while the ladder can still answer by re-shaping.
func (p *Planner) provisionStep(input Input, load LoadSnapshot) (*Action, []string) {
	if load.QueueLength <= 0 {
		return nil, nil
	}
	band := p.config.deadBand()
	gap := *load.PrefillPressure - *load.DecodePressure
	if math.Abs(gap) <= band {
		return nil, nil
	}
	role, step, busy := domain.RoleDecode, -1, "decode"
	if gap > 0 {
		role, step, busy = domain.RolePrefill, 1, "prefill"
	}
	if _, ok := stepGear(p.gear(), step); ok {
		return nil, nil
	}
	if len(input.Serving) == 0 {
		// Creating the first instance is a human decision: the console's
		// provisioning entry names the partner and the credentials.
		return nil, []string{"capacity held: no serving worker names the partner to ask"}
	}
	return &Action{
		Kind:  ActionProvision,
		Role:  role,
		Count: 1,
		Reason: fmt.Sprintf(
			"the gear ladder is already at its %s end, %s is still queueing (pressure %.3f vs %.3f, gap %+.3f) and %.0f requests are waiting: one more %s instance is needed",
			busy, busy, *load.PrefillPressure, *load.DecodePressure, gap, load.QueueLength, busy),
	}, nil
}

// releaseStep offers one instance back when the pool is idle everywhere.
//
// It prefers an unassigned instance, because giving that back leaves the pool's
// shape untouched and needs no drain. When every instance carries a role it
// gives back the less loaded role, and never its last worker: a released
// container cannot be created again under the same name on AlayaNeW, so an
// empty role would not be a ratio problem, it would be permanent.
func (p *Planner) releaseStep(input Input, load LoadSnapshot, planned []Action) (*Action, []string) {
	if load.QueueLength > 0 {
		return nil, nil
	}
	below := p.config.releaseBelow()
	if math.Max(*load.PrefillPressure, *load.DecodePressure) > below {
		return nil, nil
	}
	if len(input.Idle) > 0 {
		return &Action{
			Kind:       ActionRelease,
			InstanceID: input.Idle[0].ID,
			Count:      1,
			Reason: fmt.Sprintf(
				"unassigned instance is surplus: nothing is queueing and both roles are below %.0f%% pressure (%0.3f/%0.3f)",
				below*100, *load.PrefillPressure, *load.DecodePressure),
		}, nil
	}

	prefill, decode := 0, 0
	for _, candidate := range input.Serving {
		if candidate.Role == domain.RolePrefill {
			prefill++
		} else {
			decode++
		}
	}
	if prefill == 0 || decode == 0 {
		return nil, nil
	}
	role, count := domain.RolePrefill, prefill
	switch {
	case *load.DecodePressure < *load.PrefillPressure:
		role, count = domain.RoleDecode, decode
	case *load.PrefillPressure == *load.DecodePressure && decode >= prefill:
		role, count = domain.RoleDecode, decode
	}
	if count <= 1 {
		return nil, []string{fmt.Sprintf(
			"release held: %s is the less loaded role but the pool holds only one %s worker", role, role)}
	}
	candidate, ok := nextConversion(input.Serving, planned, input.Now, p.config.MinHold, role)
	if !ok {
		return nil, []string{fmt.Sprintf(
			"release held: no movable %s worker is available to hand back", role)}
	}
	return &Action{
		Kind:       ActionRelease,
		InstanceID: candidate.ID,
		Role:       role,
		FromRole:   role,
		Count:      1,
		Reason: fmt.Sprintf(
			"idle pool: nothing is queueing and both roles are below %.0f%% pressure; handing back a %s worker while the pool keeps both roles",
			below*100, role),
	}, nil
}

// Plan computes the actions for one round.
//
// Pseudocode from development document §11:
//
//	desired_prefill = floor(serving_instances * prefill_ratio)
//	desired_decode  = serving_instances - desired_prefill
//	apply max_change_per_round
//	respect min_hold_seconds
//	respect cooldown_seconds
//	prefer idle instances
//
// Auto mode inserts one step before that: the ratio itself is chosen from the
// measured P/D pressure, so every guard rail below applies to the resulting
// role changes exactly as it does to a manual gear change.
func (p *Planner) Plan(input Input) (Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastLoad = input.Load

	now := input.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	mode := p.config.normalisedMode()
	// Aggregated mode has no ratio, so a gear is neither required nor read. The
	// lookup is skipped rather than defaulted: indexing Gears with an empty
	// string would fail and take down a deployment that is configured exactly
	// as the documentation says to configure it.
	gear := ""
	ratio := 0.0
	if mode != ModeAggregated {
		gear = p.gear()
		found := false
		ratio, found = Gears[gear]
		if !found {
			return Decision{}, fmt.Errorf("planner gear %q is not supported", gear)
		}
	}

	decision := Decision{
		ObservedLoad:    input.Load,
		Mode:            mode,
		Gear:            p.config.Gear,
		EffectiveGear:   gear,
		TargetRatio:     ratio,
		ServingCount:    len(input.Serving),
		IdleCount:       len(input.Idle),
		RecoveringCount: len(input.Recovering),
		StrandedCount:   len(input.Stranded),
		PlannedAt:       now,
	}
	if decision.RecoveringCount > 0 {
		decision.Notes = append(decision.Notes, fmt.Sprintf(
			"%d instance(s) hold a role and are not serving; they are being restarted and are not counted as capacity",
			decision.RecoveringCount))
	}
	if decision.StrandedCount > 0 {
		decision.Notes = append(decision.Notes, fmt.Sprintf(
			"%d instance(s) hold a role with the start budget spent and nothing left to retry them; an operator has to requeue or release them",
			decision.StrandedCount))
	}

	prefillNow, decodeNow := 0, 0
	for _, candidate := range input.Serving {
		switch candidate.Role {
		case domain.RolePrefill:
			prefillNow++
		case domain.RoleDecode:
			decodeNow++
		}
	}
	// The ratio describes the disaggregated workers only. A regular worker
	// left over from the other shape is neither side, and counting it as a
	// decode worker would move the target and trigger a reassignment to fix a
	// ratio that was never wrong.
	if pd := prefillNow + decodeNow; pd > 0 {
		decision.CurrentRatio = float64(prefillNow) / float64(pd)
	}

	if p.config.Cooldown > 0 && !p.lastChangeAt.IsZero() && now.Sub(p.lastChangeAt) < p.config.Cooldown {
		decision.Notes = append(decision.Notes, fmt.Sprintf(
			"cooldown active for another %s", (p.config.Cooldown-now.Sub(p.lastChangeAt)).Round(time.Second)))
		return decision, nil
	}

	if mode == ModeAggregated {
		p.planAggregated(input, &decision, now)
		return decision, nil
	}

	rungMoved := false
	if mode == ModeAuto {
		next, notes, moved := p.autoStep(input.Load)
		decision.Notes = append(decision.Notes, notes...)
		if moved {
			rungMoved = true
			p.autoGear = next
			ratio = Gears[next]
			decision.EffectiveGear = next
			decision.TargetRatio = ratio
			// A rung move counts as a change even when the current pool already
			// fits the new gear and no action follows. Rate limiting the ladder
			// here is what stops auto mode from steering by its own echo: the
			// role change it just asked for needs a drain and a restart before
			// the pressure it reacted to can move at all.
			p.lastChangeAt = now
		}
	}

	// Recovering and stranded instances are deliberately absent from this sum.
	// A ratio target describes what the pool can serve, and counting a worker
	// that is not serving as if it were is how a degraded pool gets planned as
	// a whole one: the deficit would be filled by converting a healthy worker
	// instead of by waiting for the one that is already coming back.
	//
	// Regular workers are absent for the same reason plus one of their own:
	// they are drained this round (a PD pool cannot use a worker that was
	// started without --disaggregation-mode), so counting them would make the
	// round plan against capacity it is about to take away.
	servingPD := prefillNow + decodeNow
	total := servingPD + len(input.Idle)
	if p.config.MaxServing > 0 && total > p.config.MaxServing {
		decision.Notes = append(decision.Notes, fmt.Sprintf(
			"max_serving=%d caps %d available instances", p.config.MaxServing, total))
		total = p.config.MaxServing
	}
	decision.TargetPrefill = int(math.Floor(float64(total) * ratio))
	// A disaggregated deployment with at least two workers needs both roles,
	// including when rounding a 1:2 gear over only two available containers.
	if total >= 2 {
		decision.TargetPrefill = max(1, min(total-1, decision.TargetPrefill))
	}
	decision.TargetDecode = total - decision.TargetPrefill

	// Drain surplus capacity before it can take a role.
	actions := []Action{}
	// A regular worker cannot serve in a PD pool: it was started without
	// --disaggregation-mode and has no KV peer. Draining it is the mirror of
	// what aggregated mode does to a leftover prefill or decode worker, and it
	// is what makes switching the other way round safe as well.
	for _, candidate := range movable(input.Serving, now, p.config.MinHold) {
		if len(actions) >= p.config.MaxChangePerRound {
			break
		}
		if candidate.Role != domain.RoleRegular {
			continue
		}
		actions = append(actions, Action{
			Kind:       ActionDrain,
			InstanceID: candidate.ID,
			FromRole:   candidate.Role,
			Reason:     "a disaggregated pool cannot use a regular worker: it has no KV peer to exchange with",
		})
	}
	// Capacity verbs come before the role phases on purpose: whichever instance a
	// release names is reserved here, and every phase below skips anything this
	// round has already touched.
	if p.config.CapacityActions && mode == ModeAuto {
		capacityAction, notes := p.capacityStep(input, actions, rungMoved)
		decision.Notes = append(decision.Notes, notes...)
		if capacityAction != nil {
			actions = append(actions, *capacityAction)
		}
	}
	prefillPlanned := prefillNow
	decodePlanned := decodeNow
	drainBudget := servingPD - total
	if drainBudget > 0 {
		eligible := movable(input.Serving, now, p.config.MinHold)
		for drainBudget > 0 && len(actions) < p.config.MaxChangePerRound {
			chosen := -1
			for index, candidate := range eligible {
				if alreadyPlanned(actions, candidate.ID) {
					continue
				}
				if chosen == -1 {
					chosen = index
				}
				if candidate.Role == domain.RolePrefill && prefillPlanned > decision.TargetPrefill ||
					candidate.Role == domain.RoleDecode && decodePlanned > decision.TargetDecode {
					chosen = index
					break
				}
			}
			if chosen == -1 {
				break
			}
			candidate := eligible[chosen]
			actions = append(actions, Action{
				Kind:       ActionDrain,
				InstanceID: candidate.ID,
				FromRole:   candidate.Role,
				Reason:     fmt.Sprintf("surplus capacity beyond max_serving=%d", p.config.MaxServing),
			})
			drainBudget--
			if candidate.Role == domain.RolePrefill {
				prefillPlanned--
			} else {
				decodePlanned--
			}
		}
	}

	// Phase 1: idle instances fill whatever the ratio still needs. Idle
	// instances are preferred because they carry no in-flight traffic, so a
	// working worker is only ever converted when idle capacity ran out.
	idle := make([]Candidate, len(input.Idle))
	copy(idle, input.Idle)
	sort.Slice(idle, func(i, j int) bool { return idle[i].ID < idle[j].ID })

	for len(idle) > 0 && prefillPlanned+decodePlanned < total {
		if len(actions) >= p.config.MaxChangePerRound {
			decision.Notes = append(decision.Notes, "max_change_per_round reached")
			break
		}
		switch {
		case prefillPlanned < decision.TargetPrefill:
			actions = append(actions, Action{
				Kind:       ActionAssign,
				InstanceID: idle[0].ID,
				Role:       domain.RolePrefill,
				Reason:     "idle instance preferred for the prefill deficit",
			})
			prefillPlanned++
		case decodePlanned < decision.TargetDecode:
			actions = append(actions, Action{
				Kind:       ActionAssign,
				InstanceID: idle[0].ID,
				Role:       domain.RoleDecode,
				Reason:     "idle instance preferred for the decode deficit",
			})
			decodePlanned++
		default:
			// Capacity is already balanced; the remaining idle instances stay
			// unassigned and available for the next capacity change.
			decision.Notes = append(decision.Notes, "capacity balanced; remaining idle instances stay unassigned")
			idle = nil
			continue
		}
		idle = idle[1:]
	}

	// Phase 2: only when idle capacity is exhausted does the planner convert an
	// existing worker to the other role, honouring min_hold and the round cap.
	//
	// A conversion is refused when it would take the last worker of its role. A
	// disaggregated pool serves a request only when both roles are present, so
	// trading the only Prefill for a Decode (or the reverse) does not rebalance
	// a degraded pool, it kills it. That is not hypothetical: with one Prefill
	// and one Decode serving, draining the Decode dropped the pool to a single
	// instance, the round then asked for one Decode and reassigned the only
	// Prefill, taking the deployment to 0P0D and stopping traffic for four
	// minutes until reconciliation restored both workers.
	//
	// The guard is checked only when a conversion would otherwise happen, so the
	// note always names the reason that actually blocked this round.
	for len(actions) < p.config.MaxChangePerRound {
		switch {
		case prefillPlanned < decision.TargetPrefill:
			candidate, ok := nextConversion(input.Serving, actions, now, p.config.MinHold, domain.RoleDecode)
			switch {
			case ok && decodePlanned > 1:
				actions = append(actions, Action{
					Kind:       ActionReassign,
					InstanceID: candidate.ID,
					Role:       domain.RolePrefill,
					FromRole:   domain.RoleDecode,
					Reason:     "no idle capacity left; rebalance a decode worker to prefill",
				})
				prefillPlanned++
				decodePlanned--
				continue
			case !ok:
				decision.Notes = append(decision.Notes, "no movable decode worker is available for the prefill deficit")
			default:
				decision.Notes = append(decision.Notes, fmt.Sprintf(
					"refusing to reassign the last decode worker %s: a disaggregated pool must keep both roles", candidate.ID))
			}
			prefillPlanned = decision.TargetPrefill
		case decodePlanned < decision.TargetDecode:
			candidate, ok := nextConversion(input.Serving, actions, now, p.config.MinHold, domain.RolePrefill)
			switch {
			case ok && prefillPlanned > 1:
				actions = append(actions, Action{
					Kind:       ActionReassign,
					InstanceID: candidate.ID,
					Role:       domain.RoleDecode,
					FromRole:   domain.RolePrefill,
					Reason:     "no idle capacity left; rebalance a prefill worker to decode",
				})
				decodePlanned++
				prefillPlanned--
				continue
			case !ok:
				decision.Notes = append(decision.Notes, "no movable prefill worker is available for the decode deficit")
			default:
				decision.Notes = append(decision.Notes, fmt.Sprintf(
					"refusing to reassign the last prefill worker %s: a disaggregated pool must keep both roles", candidate.ID))
			}
			decodePlanned = decision.TargetDecode
		default:
			// Ratio satisfied.
			idle = nil
			goto done
		}
	}
done:

	decision.Actions = actions
	if len(actions) > 0 {
		p.lastChangeAt = now
	}
	return decision, nil
}

// planAggregated is the second planning shape: the pool holds one kind of
// worker, so there is no ratio to keep.
//
// Deliberately absent: the gear ladder, the pressure sample and the capacity
// verbs. "Aggregated" means the deployment already decided how wide the pool
// should be - four TP4 replicas is a number that came from a benchmark, not
// from a pressure ratio - and the planner's whole job is to make sure every
// instance it was given runs a regular worker.
//
// Two details still need care:
//
//   - a worker left over from a PD deployment carries prefill or decode, and a
//     regular pool cannot use it: it was started with --disaggregation-mode and
//     waits for a KV peer this pool does not have. It is drained, and a later
//     round assigns it back as regular. That is also what makes switching a
//     running deployment from PD to aggregated safe instead of a manual rebuild.
//   - max_serving is the only sizing knob. Zero means "every instance the
//     partner provided", which is the normal case on a fixed-capacity cluster.
func (p *Planner) planAggregated(input Input, decision *Decision, now time.Time) {
	total := len(input.Serving) + len(input.Idle)
	target := total
	if p.config.MaxServing > 0 && total > p.config.MaxServing {
		decision.Notes = append(decision.Notes, fmt.Sprintf(
			"max_serving=%d caps %d available instances", p.config.MaxServing, total))
		target = p.config.MaxServing
	}
	decision.TargetRegular = target

	actions := make([]Action, 0, target)
	surplus := total - target

	// Phase 1: take out what cannot serve in this shape. Surplus capacity goes
	// first, then anything still carrying a PD role.
	for _, candidate := range movable(input.Serving, now, p.config.MinHold) {
		if len(actions) >= p.config.MaxChangePerRound {
			decision.Notes = append(decision.Notes, "max_change_per_round reached")
			break
		}
		switch {
		case surplus > 0:
			actions = append(actions, Action{
				Kind:       ActionDrain,
				InstanceID: candidate.ID,
				FromRole:   candidate.Role,
				Reason:     fmt.Sprintf("surplus capacity beyond max_serving=%d", p.config.MaxServing),
			})
			surplus--
		case candidate.Role != domain.RoleRegular:
			actions = append(actions, Action{
				Kind:       ActionDrain,
				InstanceID: candidate.ID,
				FromRole:   candidate.Role,
				Reason: fmt.Sprintf(
					"aggregated mode cannot use a %s worker: it was started with --disaggregation-mode and has no KV peer here",
					candidate.Role),
			})
		}
	}

	regular := 0
	for _, candidate := range input.Serving {
		if candidate.Role == domain.RoleRegular && !alreadyPlanned(actions, candidate.ID) {
			regular++
		}
	}

	// Phase 2: fill the pool from idle instances. Sorted so a round is
	// reproducible; there is no other preference to express, because every
	// regular worker is interchangeable.
	idle := make([]Candidate, len(input.Idle))
	copy(idle, input.Idle)
	sort.Slice(idle, func(i, j int) bool { return idle[i].ID < idle[j].ID })
	for _, candidate := range idle {
		if regular >= target {
			break
		}
		if len(actions) >= p.config.MaxChangePerRound {
			decision.Notes = append(decision.Notes, "max_change_per_round reached")
			break
		}
		if alreadyPlanned(actions, candidate.ID) {
			continue
		}
		actions = append(actions, Action{
			Kind:       ActionAssign,
			InstanceID: candidate.ID,
			Role:       domain.RoleRegular,
			Reason:     "aggregated mode runs an identical worker on every available instance",
		})
		regular++
	}

	decision.Actions = actions
	if len(actions) > 0 {
		p.lastChangeAt = now
	}
}

// nextConversion returns the worker this round should move out of fromRole, if
// any is allowed to move. It is the role-parameterised form of "oldest movable
// candidate that this round has not already touched".
func nextConversion(candidates []Candidate, actions []Action, now time.Time, minHold time.Duration, fromRole domain.Role) (Candidate, bool) {
	for _, candidate := range movable(candidates, now, minHold) {
		if candidate.Role != fromRole || alreadyPlanned(actions, candidate.ID) {
			continue
		}
		return candidate, true
	}
	return Candidate{}, false
}

// candidateIDs renders the candidate ids of one bucket for a note, bounded so a
// large pool cannot turn a log line into a page.
func candidateIDs(candidates []Candidate) string {
	const limit = 4
	ids := make([]string, 0, min(len(candidates), limit))
	for index, candidate := range candidates {
		if index == limit {
			ids = append(ids, fmt.Sprintf("and %d more", len(candidates)-limit))
			break
		}
		ids = append(ids, candidate.ID)
	}
	return strings.Join(ids, ", ")
}

// alreadyPlanned reports whether the round already changed the instance.
func alreadyPlanned(actions []Action, instanceID string) bool {
	for _, action := range actions {
		if action.InstanceID == instanceID {
			return true
		}
	}
	return false
}

// movable returns the candidates that are allowed to change role, oldest
// assignment first, so the planner prefers stability for freshly started work.
func movable(candidates []Candidate, now time.Time, minHold time.Duration) []Candidate {
	eligible := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if minHold > 0 && !candidate.RoleSince.IsZero() && now.Sub(candidate.RoleSince) < minHold {
			continue
		}
		eligible = append(eligible, candidate)
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].RoleSince.Equal(eligible[j].RoleSince) {
			return eligible[i].ID < eligible[j].ID
		}
		return eligible[i].RoleSince.Before(eligible[j].RoleSince)
	})
	return eligible
}

// DecisionSummary is a compact human readable rendering used in logs.
func (d Decision) DecisionSummary() string {
	if d.Mode == ModeAggregated {
		return fmt.Sprintf("mode=aggregated target=regular:%d serving=%d idle=%d recovering=%d stranded=%d actions=%d",
			d.TargetRegular, d.ServingCount, d.IdleCount,
			d.RecoveringCount, d.StrandedCount, len(d.Actions))
	}
	return fmt.Sprintf("gear=%s target=%d/%d serving=%d idle=%d recovering=%d stranded=%d actions=%d",
		d.Gear, d.TargetPrefill, d.TargetDecode, d.ServingCount, d.IdleCount,
		d.RecoveringCount, d.StrandedCount, len(d.Actions))
}
