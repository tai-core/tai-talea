// Package planner implements the milestone 1 PD planner: a fixed
// Prefill/Decode ratio with manual gears, change caps, minimum hold time,
// cooldown and a preference for idle instances.
//
// Development document §11 is explicit that v1 must NOT claim adaptive
// Prefill/Decode sizing. LoadSnapshot therefore exists only as a reserved input
// surface which milestone 3 will populate from real traffic.
package planner

import (
	"errors"
	"fmt"
	"math"
	"sort"
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

// GearNames returns the supported gears in a stable order.
func GearNames() []string {
	names := make([]string, 0, len(Gears))
	for name := range Gears {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Config is the planner tuning surface.
type Config struct {
	// Gear is the manual ratio gear, for example "1:1".
	Gear string `json:"gear" yaml:"gear"`
	// MaxChangePerRound caps how many instances may change role per round.
	MaxChangePerRound int `json:"max_change_per_round" yaml:"max_change_per_round"`
	// MinHold is the minimum time an instance keeps an assigned role.
	MinHold time.Duration `json:"min_hold" yaml:"min_hold"`
	// Cooldown is the minimum time between two planner rounds that change anything.
	Cooldown time.Duration `json:"cooldown" yaml:"cooldown"`
	// MaxServing optionally caps how many instances serve traffic at once.
	// Zero means "use every available instance", which is the milestone 1
	// behaviour: we never ask the partner for capacity we do not have.
	MaxServing int `json:"max_serving" yaml:"max_serving"`
}

// Validate rejects impossible planner configurations.
func (c Config) Validate() error {
	if _, ok := Gears[c.Gear]; !ok {
		return fmt.Errorf("planner gear must be one of %v, got %q", GearNames(), c.Gear)
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
	return nil
}

// LoadSnapshot carries measured scheduler load for diagnostics. The fixed gear
// planner records it but does not use it to automatically change the PD ratio.
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
}

// Input is one planner round input.
type Input struct {
	Now time.Time
	// Serving lists instances that already hold a PD role.
	Serving []Candidate
	// Idle lists prepared instances without a role.
	Idle []Candidate
	// Load is reserved for milestone 3.
	Load LoadSnapshot
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
)

// Action is one planned change.
type Action struct {
	Kind       ActionKind  `json:"kind"`
	InstanceID string      `json:"instance_id"`
	Role       domain.Role `json:"role,omitempty"`
	FromRole   domain.Role `json:"from_role,omitempty"`
	Reason     string      `json:"reason"`
}

// Decision is the planner output for one round.
type Decision struct {
	ObservedLoad  LoadSnapshot `json:"observed_load"`
	Gear          string       `json:"gear"`
	TargetRatio   float64      `json:"target_ratio"`
	CurrentRatio  float64      `json:"current_ratio"`
	TargetPrefill int          `json:"target_prefill"`
	TargetDecode  int          `json:"target_decode"`
	ServingCount  int          `json:"serving_count"`
	IdleCount     int          `json:"idle_count"`
	Actions       []Action     `json:"actions"`
	Notes         []string     `json:"notes,omitempty"`
	PlannedAt     time.Time    `json:"planned_at"`
}

// Planner applies a fixed ratio gear to the current worker population.
type Planner struct {
	lastLoad LoadSnapshot
	config   Config
	mu       sync.Mutex
	// lastChangeAt is the timestamp of the last round that produced changes.
	lastChangeAt time.Time
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

// LastObservedLoad is the input used by the last plan, with its sample timestamp.
func (p *Planner) LastObservedLoad() LoadSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastLoad
}

// SetGear switches the manual gear.
func (p *Planner) SetGear(gear string) error {
	if _, ok := Gears[gear]; !ok {
		return fmt.Errorf("planner gear must be one of %v, got %q", GearNames(), gear)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.config.Gear = gear
	return nil
}

// LastChangeAt returns when the planner last produced an action.
func (p *Planner) LastChangeAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastChangeAt
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
func (p *Planner) Plan(input Input) (Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastLoad = input.Load

	now := input.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ratio, ok := Gears[p.config.Gear]
	if !ok {
		return Decision{}, fmt.Errorf("planner gear %q is not supported", p.config.Gear)
	}

	decision := Decision{
		ObservedLoad: input.Load,
		Gear:         p.config.Gear,
		TargetRatio:  ratio,
		ServingCount: len(input.Serving),
		IdleCount:    len(input.Idle),
		PlannedAt:    now,
	}

	prefillNow := 0
	for _, candidate := range input.Serving {
		if candidate.Role == domain.RolePrefill {
			prefillNow++
		}
	}
	if len(input.Serving) > 0 {
		decision.CurrentRatio = float64(prefillNow) / float64(len(input.Serving))
	}

	if p.config.Cooldown > 0 && !p.lastChangeAt.IsZero() && now.Sub(p.lastChangeAt) < p.config.Cooldown {
		decision.Notes = append(decision.Notes, fmt.Sprintf(
			"cooldown active for another %s", (p.config.Cooldown-now.Sub(p.lastChangeAt)).Round(time.Second)))
		return decision, nil
	}

	total := len(input.Serving) + len(input.Idle)
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
	prefillPlanned := prefillNow
	decodePlanned := len(input.Serving) - prefillNow
	drainBudget := len(input.Serving) - total
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
	for len(actions) < p.config.MaxChangePerRound {
		switch {
		case prefillPlanned < decision.TargetPrefill:
			converted := false
			for _, candidate := range movable(input.Serving, now, p.config.MinHold) {
				if candidate.Role != domain.RoleDecode || alreadyPlanned(actions, candidate.ID) {
					continue
				}
				actions = append(actions, Action{
					Kind:       ActionReassign,
					InstanceID: candidate.ID,
					Role:       domain.RolePrefill,
					FromRole:   domain.RoleDecode,
					Reason:     "no idle capacity left; rebalance a decode worker to prefill",
				})
				prefillPlanned++
				decodePlanned--
				converted = true
				break
			}
			if !converted {
				decision.Notes = append(decision.Notes, "no movable decode worker is available for the prefill deficit")
				prefillPlanned = decision.TargetPrefill
			}
		case decodePlanned < decision.TargetDecode:
			converted := false
			for _, candidate := range movable(input.Serving, now, p.config.MinHold) {
				if candidate.Role != domain.RolePrefill || alreadyPlanned(actions, candidate.ID) {
					continue
				}
				actions = append(actions, Action{
					Kind:       ActionReassign,
					InstanceID: candidate.ID,
					Role:       domain.RoleDecode,
					FromRole:   domain.RolePrefill,
					Reason:     "no idle capacity left; rebalance a prefill worker to decode",
				})
				decodePlanned++
				prefillPlanned--
				converted = true
				break
			}
			if !converted {
				decision.Notes = append(decision.Notes, "no movable prefill worker is available for the decode deficit")
				decodePlanned = decision.TargetDecode
			}
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
	return fmt.Sprintf("gear=%s target=%d/%d serving=%d idle=%d actions=%d",
		d.Gear, d.TargetPrefill, d.TargetDecode, d.ServingCount, d.IdleCount, len(d.Actions))
}
