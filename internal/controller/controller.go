// Package controller implements the capacity plane's control loop: it turns
// normalized capacity events into container preparation, SGLang service start,
// Router registration, readiness based draining and partner release.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tai-core/tai-talea/internal/config"
	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/instancemanager"
	"github.com/tai-core/tai-talea/internal/launcher"
	"github.com/tai-core/tai-talea/internal/obs"
	"github.com/tai-core/tai-talea/internal/partner"
	"github.com/tai-core/tai-talea/internal/planner"
	"github.com/tai-core/tai-talea/internal/routeradapter"
	"github.com/tai-core/tai-talea/internal/store"
	"github.com/tai-core/tai-talea/internal/telemetry"
)

// ActorName is the audit actor for control plane driven actions.
const ActorName = "tai-talea"

// RouterAdapter is the Router control surface required by the controller.
// Draining is only ever expressed through SetReadiness: the interface
// keeps stale-role cleanup separate from the ordinary drain surface (§8).
type RouterAdapter interface {
	RegisterWorker(ctx context.Context, request routeradapter.RegisterRequest) (routeradapter.Registration, error)
	GetWorker(ctx context.Context, workerID string) (routeradapter.Worker, error)
	FindWorkerByURL(ctx context.Context, workerURL string) (routeradapter.Worker, bool, error)
	GetReadiness(ctx context.Context, workerID string) (routeradapter.ReadinessRecord, error)
	SetReadiness(ctx context.Context, workerID string, ready bool) (routeradapter.ReadinessTransition, error)
	GetLoads(ctx context.Context) ([]routeradapter.WorkerLoad, error)
}

// Launcher is the in-container runtime launcher surface.
type Launcher interface {
	Health(ctx context.Context, endpoint string) (launcher.Health, error)
	Status(ctx context.Context, endpoint string) (launcher.Status, error)
	Start(ctx context.Context, endpoint string, request launcher.StartRequest) error
	Stop(ctx context.Context, endpoint string, request launcher.StopRequest) (launcher.StopResult, error)
}

// InstanceManager performs §7.1 container preparation.
type InstanceManager interface {
	Prepare(ctx context.Context, instance domain.Instance) (instancemanager.Report, error)
}

// PartnerResolver resolves a partner id to its adapter.
type PartnerResolver interface {
	Adapter(partnerID string) (partner.PartnerAdapter, bool)
}

// Deps are the controller collaborators.
type Deps struct {
	Config   config.Config
	Store    store.Store
	Router   RouterAdapter
	Launcher Launcher
	Manager  InstanceManager
	Partners PartnerResolver
	Planner  *planner.Planner
	Metrics  *obs.Registry
	Alerter  *obs.Alerter
	Logger   *slog.Logger
	Now      func() time.Time
}

// Controller owns the dual-layer state machine.
type Controller struct {
	telemetry *telemetry.Collector
	cfg       config.Config
	store     store.Store
	router    RouterAdapter
	launcher  Launcher
	manager   InstanceManager
	partners  PartnerResolver
	planner   *planner.Planner
	metrics   *obs.Registry
	alerts    *obs.Alerter
	logger    *slog.Logger
	now       func() time.Time

	mu                 sync.Mutex
	ratioStreak        int
	instanceOperations sync.Map
	eventOperations    [64]sync.Mutex
	capacityAdmissions sync.Mutex
	pullOperations     sync.Map
	eventRecovery      sync.Mutex
	eventRecoveryAfter string
	eventRecoveryTime  time.Time
}

func (c *Controller) eventOperation(id string) *sync.Mutex {
	// A bounded stripe table avoids retaining one lock forever for every
	// historical event. Collisions only serialize unrelated deliveries.
	var hash uint64
	for _, value := range []byte(id) {
		hash = hash*1099511628211 ^ uint64(value)
	}
	return &c.eventOperations[hash%uint64(len(c.eventOperations))]
}

func (c *Controller) instanceOperation(id string) *sync.Mutex {
	lock, _ := c.instanceOperations.LoadOrStore(id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// New validates dependencies and builds a controller.
func New(deps Deps) (*Controller, error) {
	if deps.Store == nil {
		return nil, errors.New("controller requires a store")
	}
	if deps.Router == nil {
		return nil, errors.New("controller requires a router adapter")
	}
	if deps.Launcher == nil {
		return nil, errors.New("controller requires a launcher")
	}
	if deps.Manager == nil {
		return nil, errors.New("controller requires an instance manager")
	}
	if deps.Partners == nil {
		return nil, errors.New("controller requires a partner resolver")
	}
	if deps.Planner == nil {
		return nil, errors.New("controller requires a planner")
	}
	if err := deps.Config.Validate(); err != nil {
		return nil, err
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Controller{
		telemetry: telemetry.New(deps.Store, deps.Router),
		cfg:       deps.Config,
		store:     deps.Store,
		router:    deps.Router,
		launcher:  deps.Launcher,
		manager:   deps.Manager,
		partners:  deps.Partners,
		planner:   deps.Planner,
		metrics:   deps.Metrics,
		alerts:    deps.Alerter,
		logger:    logger,
		now:       now,
	}, nil
}

// Config exposes the active configuration.
func (c *Controller) Config() config.Config { return c.cfg }

// Planner exposes the PD planner so the admin API can switch gears.
func (c *Controller) Planner() *planner.Planner { return c.planner }

// Telemetry exposes a partner-filtered snapshot; collection runs independently.
func (c *Controller) Telemetry(owner string) telemetry.Snapshot { return c.telemetry.Snapshot(owner) }

// CallTimeout returns the per-call timeout used for container and Router calls.
func (c *Controller) CallTimeout() time.Duration { return c.cfg.Controller.CallTimeout.Duration() }

// StartTimeout bounds the whole wait for a freshly started SGLang to become
// healthy, which includes model loading. Deliberately separate from
// CallTimeout: that bounds one HTTP call, this bounds a multi-minute startup.
func (c *Controller) StartTimeout() time.Duration { return c.cfg.Controller.StartTimeout.Duration() }

func (c *Controller) callContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := c.CallTimeout()
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return context.WithTimeout(parent, timeout)
}

// operationID builds a unique, sortable operation identifier.
func (c *Controller) operationID(instanceID string, kind store.OperationType) string {
	buffer := make([]byte, 6)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%s-%s-%d", instanceID, kind, c.now().UnixNano())
	}
	return fmt.Sprintf("%s-%s-%d-%s", instanceID, kind, c.now().UnixNano(), hex.EncodeToString(buffer))
}

// audit records a lifecycle action. Audit failures are logged but never block
// the state machine, because the action already happened.
func (c *Controller) audit(ctx context.Context, entry store.AuditEntry) {
	if entry.OccurredAt.IsZero() {
		entry.OccurredAt = c.now()
	}
	if entry.Actor == "" {
		entry.Actor = ActorName
	}
	if err := c.store.AppendAudit(ctx, entry); err != nil {
		c.logger.Error("audit write failed", "action", entry.Action, "instance_id", entry.InstanceID, "error", err.Error())
	}
}

// transition applies one state change through the store, which re-validates the
// dual-layer rules inside the transaction.
func (c *Controller) transition(ctx context.Context, transition store.Transition) error {
	if err := c.store.ApplyTransition(ctx, transition); err != nil {
		var violation *domain.Violation
		if errors.As(err, &violation) {
			c.reportViolation(ctx, transition, violation)
		}
		return err
	}
	return nil
}

// reportViolation logs, counts and alerts an illegal state transition. Illegal
// transitions are never silently corrected (§4.2).
func (c *Controller) reportViolation(ctx context.Context, transition store.Transition, violation *domain.Violation) {
	if c.metrics != nil {
		c.metrics.IncCounter("capacity_illegal_transitions_total",
			"Refused state changes and illegal status combinations.",
			[]string{"code"}, "code", violation.Code)
	}
	if c.logger != nil {
		c.logger.Error("illegal state change refused",
			"code", violation.Code,
			"severity", string(violation.Severity),
			"instance_id", transition.InstanceID,
			"from_instance_state", string(transition.ExpectInstanceState),
			"from_service_state", string(transition.ExpectServiceState),
			"to_instance_state", string(transition.Next.InstanceState),
			"to_service_state", string(transition.Next.ServiceState),
			"reason", violation.Message)
	}
	c.alert(ctx, obs.Alert{
		Name:       obs.AlertIllegalStateTransition,
		Severity:   obs.SeverityCritical,
		InstanceID: transition.InstanceID,
		PartnerID:  transition.Next.PartnerID,
		Message:    violation.Message,
		Details: map[string]string{
			"code":              violation.Code,
			"severity":          string(violation.Severity),
			"to_instance_state": string(transition.Next.InstanceState),
			"to_service_state":  string(transition.Next.ServiceState),
		},
	})
}

func (c *Controller) alert(ctx context.Context, alert obs.Alert) {
	if c.alerts == nil {
		return
	}
	c.alerts.Fire(ctx, alert)
}

func (c *Controller) log() *slog.Logger {
	if c.logger == nil {
		return slog.Default()
	}
	return c.logger
}
