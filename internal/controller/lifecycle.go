package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/instancemanager"
	"github.com/tai-core/tai-talea/internal/launcher"
	"github.com/tai-core/tai-talea/internal/obs"
	"github.com/tai-core/tai-talea/internal/partner"
	"github.com/tai-core/tai-talea/internal/routeradapter"
	"github.com/tai-core/tai-talea/internal/store"
)

// runPrepare executes §7.1 steps 1-6 for one instance. It is safe to call again
// after a failure: attempts are tracked on the instance row.
func (c *Controller) runPrepare(ctx context.Context, instanceID, operationID string) error {
	instance, err := c.store.GetInstance(ctx, instanceID)
	if err != nil {
		return err
	}
	if instance.InstanceState != domain.InstancePreparing {
		return fmt.Errorf("instance %s is %s, cannot prepare", instanceID, instance.InstanceState)
	}
	// Every retry belongs to the same lease operation; otherwise an older
	// pending row can bypass the newest retry delay after a restart.
	operationID = lifecycleOperationID(instance, store.OpPrepare)

	startedAt := c.now()
	callCtx, cancel := c.callContext(ctx)
	report, prepareErr := c.manager.Prepare(callCtx, instance)
	cancel()

	next := instance
	if prepareErr != nil {
		reason, details := instancemanager.DescribeFailure(prepareErr)
		next.PrepareAttempts = instance.PrepareAttempts + 1
		next.LastError = prepareErr.Error()
		next.LastSeenAt = c.now()
		// Service state stays NONE: nothing was started, and §4.2 only allows
		// PREPARING + NONE or PREPARING + FAILED.

		exhausted := next.PrepareAttempts >= c.cfg.Controller.PrepareMaxAttempts
		operation := &store.Operation{
			OperationID: operationID,
			InstanceID:  instanceID,
			Type:        store.OpPrepare,
			Attempt:     next.PrepareAttempts,
			LastError:   prepareErr.Error(),
			CreatedAt:   instance.CreatedAt,
		}
		if exhausted {
			// §7.1: exceeding the retry budget moves the container to LOST so
			// that it can no longer be scheduled.
			next.InstanceState = domain.InstanceLost
			operation.Status = store.OpFailed
		} else {
			operation.Status = store.OpPending
			operation.NextRetryAt = c.now().Add(c.cfg.Controller.OperationRetry.Duration())
		}
		if err := c.transition(ctx, store.Transition{
			InstanceID: instanceID,
			Next:       next,
			Operation:  operation,
			Audit: store.AuditEntry{
				Action: "instance_prepare_failed", InstanceID: instanceID, PartnerID: instance.PartnerID,
				Details: mergeDetails(map[string]string{
					"attempt":   fmt.Sprintf("%d", next.PrepareAttempts),
					"reason":    reason,
					"exhausted": fmt.Sprintf("%t", exhausted),
					"error":     prepareErr.Error(),
				}, details),
			},
		}); err != nil {
			return err
		}

		if c.metrics != nil {
			c.metrics.Observe(obs.MetricInstancePrepareSeconds,
				"Container preparation duration in seconds.",
				[]string{"outcome"}, c.now().Sub(startedAt).Seconds(), "outcome", "failed")
		}
		alertName := obs.AlertBootstrapFailure
		if errors.Is(prepareErr, instancemanager.ErrCompatMismatch) {
			alertName = obs.AlertEnvironmentMismatch
		}
		if details["alert"] != "" {
			alertName = details["alert"]
		}
		c.alert(ctx, obs.Alert{
			Name:       alertName,
			Severity:   obs.SeverityWarning,
			InstanceID: instanceID,
			PartnerID:  instance.PartnerID,
			Message:    "container preparation failed",
			Details: mergeDetails(map[string]string{
				"attempt":   fmt.Sprintf("%d", next.PrepareAttempts),
				"max":       fmt.Sprintf("%d", c.cfg.Controller.PrepareMaxAttempts),
				"exhausted": fmt.Sprintf("%t", exhausted),
			}, details),
		})
		if exhausted {
			c.metrics.AddCounter(obs.MetricLostInstancesTotal,
				"Instances that became unreachable or lost their lease.", obs.LabelsReason,
				1, "reason", reason)
		}
		return prepareErr
	}

	next.InstanceState = domain.InstanceIdle
	// Preparation may be recovering an already running process after LOST.
	// Keep its assigned role and enter the ordinary retry/adoption path.
	if instance.Role.Valid() && report.Status.Phase == launcher.PhaseRunning {
		next.ServiceState = domain.ServiceFailed
	} else {
		next.ServiceState = domain.ServiceNone
		next.Role = domain.RoleNone
		next.RoleAssignedAt = time.Time{}
	}
	next.PrepareAttempts = 0
	next.LastError = ""
	next.LastSeenAt = report.PreparedAt
	operation := &store.Operation{
		OperationID: operationID,
		InstanceID:  instanceID,
		Type:        store.OpPrepare,
		Status:      store.OpSucceeded,
		Attempt:     instance.PrepareAttempts + 1,
		CreatedAt:   instance.CreatedAt,
	}
	if err := c.transition(ctx, store.Transition{
		InstanceID: instanceID,
		Next:       next,
		Operation:  operation,
		Audit: store.AuditEntry{
			Action: "instance_prepared", InstanceID: instanceID, PartnerID: instance.PartnerID,
			Details: map[string]string{
				"endpoint":          instance.Endpoint,
				"bootstrap_version": report.Health.Version,
				"container_os":      report.Status.Environment.OS,
				"container_cuda":    report.Status.Environment.CUDA,
				"container_python":  report.Status.Environment.Python,
				"container_sglang":  report.Status.Environment.SGLang,
				"wheelhouse":        report.Status.Environment.Wheelhouse,
				"virtualenv":        report.Status.Environment.Virtualenv,
			},
		},
	}); err != nil {
		return err
	}
	if c.metrics != nil {
		c.metrics.Observe(obs.MetricInstancePrepareSeconds,
			"Container preparation duration in seconds.",
			[]string{"outcome"}, c.now().Sub(startedAt).Seconds(), "outcome", "prepared")
	}
	c.log().Info("instance prepared", "instance_id", instanceID, "endpoint", instance.Endpoint, "partner_id", instance.PartnerID)
	return nil
}

// StartService drives IDLE + role -> STARTING -> HEALTHY -> REGISTERING -> SERVING
// exactly as required by §7.2. SERVING is only declared once the container is
// reachable, SGLang is healthy, the worker is registered with the right role and
// readiness is routable.
func (c *Controller) StartService(ctx context.Context, instanceID string, role domain.Role) error {
	lock := c.instanceOperation(instanceID)
	lock.Lock()
	defer lock.Unlock()
	if !role.Valid() {
		return fmt.Errorf("instance %s cannot start without a valid PD role", instanceID)
	}
	instance, err := c.store.GetInstance(ctx, instanceID)
	if err != nil {
		return err
	}
	if instance.InstanceState != domain.InstanceIdle {
		return fmt.Errorf("instance %s is %s, service start requires IDLE", instanceID, instance.InstanceState)
	}
	if instance.PendingRelease || instance.PendingUpdate != nil {
		return fmt.Errorf("instance %s is pending release or identity update", instanceID)
	}
	if instance.ServiceState == domain.ServiceServing {
		return nil
	}

	startedAt := c.now()
	if instance.Role != role {
		instance.RoleAssignedAt = startedAt
	}
	instance.Role = role
	instance.ServiceState = domain.ServiceStarting
	instance.StartAttempts++
	operation := &store.Operation{
		OperationID: lifecycleOperationID(instance, store.OpStart),
		InstanceID:  instanceID,
		Type:        store.OpStart,
		Status:      store.OpInProgress,
		Attempt:     instance.StartAttempts,
		CreatedAt:   c.now(),
	}
	if err := c.transition(ctx, store.Transition{
		InstanceID: instanceID,
		Next:       instance,
		Operation:  operation,
		Audit: store.AuditEntry{
			Action: "service_starting", InstanceID: instanceID, PartnerID: instance.PartnerID,
			Details: map[string]string{"role": string(role), "attempt": fmt.Sprintf("%d", instance.StartAttempts)},
		},
	}); err != nil {
		return err
	}

	request := launcher.StartRequest{
		Role:      role,
		ModelID:   c.cfg.Controller.ModelID,
		ModelPath: c.cfg.Controller.ModelPath,
		ExtraArgs: c.cfg.Controller.ExtraSGLangArgs,
		Port:      servicePort(instance),
	}
	callCtx, cancel := c.callContext(ctx)
	startErr := c.launcher.Start(callCtx, instance.Endpoint, request)
	cancel()
	if startErr != nil {
		// A control-plane restart leaves a healthy service behind. Starting
		// must be idempotent: adopt the running service and let the health
		// wait decide, instead of failing the attempt (a retry would only hit
		// the same 422 and the instance would never recover).
		if !errors.Is(startErr, launcher.ErrAlreadyRunning) {
			return c.failStart(ctx, instance, operation, "bootstrap start failed", startErr)
		}
		c.log().Info("adopting the service the bootstrap already supervises",
			"instance_id", instanceID, "role", string(role))
	}

	if err := c.waitHealthy(ctx, instance); err != nil {
		return c.failStart(ctx, instance, operation, "sglang health check failed", err)
	}
	callCtx, cancel = c.callContext(ctx)
	status, statusErr := c.launcher.Status(callCtx, instance.Endpoint)
	cancel()
	if statusErr == nil && (status.Phase != launcher.PhaseRunning || status.Role != string(role) ||
		(status.ModelID != "" && status.ModelID != c.cfg.Controller.ModelID)) {
		statusErr = fmt.Errorf("bootstrap runs phase %s role %s model %s, expected RUNNING/%s/%s",
			status.Phase, status.Role, status.ModelID, role, c.cfg.Controller.ModelID)
	}
	if statusErr != nil {
		return c.failStart(ctx, instance, operation, "running service identity mismatch", statusErr)
	}
	instance.ServiceState = domain.ServiceHealthy
	if err := c.transition(ctx, store.Transition{
		InstanceID: instanceID,
		Next:       instance,
		Audit: store.AuditEntry{
			Action: "service_healthy", InstanceID: instanceID, PartnerID: instance.PartnerID,
			Details: map[string]string{"role": string(role)},
		},
	}); err != nil {
		return err
	}

	instance.ServiceState = domain.ServiceRegistering
	if err := c.transition(ctx, store.Transition{
		InstanceID: instanceID,
		Next:       instance,
		Audit: store.AuditEntry{
			Action: "service_registering", InstanceID: instanceID, PartnerID: instance.PartnerID,
			Details: map[string]string{"router": c.cfg.Router.Name, "role": string(role)},
		},
	}); err != nil {
		return err
	}

	registration, err := c.register(ctx, instance, role)
	if err != nil {
		return c.failStart(ctx, instance, operation, "router registration failed", err)
	}
	instance.RouterWorkerID = registration.WorkerID
	generation, ready, readinessErr := c.ensureReadiness(ctx, registration.WorkerID)
	if readinessErr != nil {
		return c.failStart(ctx, instance, operation, "router readiness failed", readinessErr)
	}
	if !ready {
		return c.failStart(ctx, instance, operation, "router readiness not routable",
			fmt.Errorf("worker %s readiness is not routable (generation %d)", registration.WorkerID, generation))
	}
	instance.ReadinessGeneration = generation
	instance.ServiceState = domain.ServiceServing
	instance.StartAttempts = 0
	instance.LastError = ""
	instance.LastSeenAt = c.now()
	operation.Status = store.OpSucceeded
	if err := c.transition(ctx, store.Transition{
		InstanceID: instanceID,
		Next:       instance,
		Operation:  operation,
		Audit: store.AuditEntry{
			Action: "service_serving", InstanceID: instanceID, PartnerID: instance.PartnerID,
			Details: map[string]string{
				"router":               c.cfg.Router.Name,
				"role":                 string(role),
				"router_worker_id":     registration.WorkerID,
				"readiness_generation": fmt.Sprintf("%d", generation),
				"worker_url":           registration.WorkerURL,
			},
		},
	}); err != nil {
		return err
	}
	if c.metrics != nil {
		c.metrics.Observe(obs.MetricServiceStartSeconds,
			"SGLang service start duration in seconds.",
			obs.LabelsPlannerRole, c.now().Sub(startedAt).Seconds(), "role", string(role))
	}
	c.log().Info("service serving",
		"instance_id", instanceID, "role", string(role),
		"router_worker_id", registration.WorkerID, "endpoint", instance.Endpoint)
	return nil
}

// waitHealthy polls the bootstrap health endpoint until SGLang answers or the
// call timeout expires.
func (c *Controller) waitHealthy(ctx context.Context, instance domain.Instance) error {
	deadline := c.now().Add(c.StartTimeout())
	var lastErr error
	for {
		current, err := c.store.GetInstance(ctx, instance.ID)
		if err != nil {
			return err
		}
		if current.PendingRelease {
			return fmt.Errorf("instance %s was reclaimed during startup", instance.ID)
		}
		callCtx, cancel := c.callContext(ctx)
		health, err := c.launcher.Health(callCtx, instance.Endpoint)
		cancel()
		if err == nil && health.Healthy() && (health.Phase == "" || health.Phase == launcher.PhaseRunning) {
			return nil
		}
		if err == nil && health.Phase == launcher.PhaseFailed {
			return fmt.Errorf("sglang process failed during startup: %s", health.Detail)
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("bootstrap reports phase %s status %s", health.Phase, health.Status)
		}
		if c.now().After(deadline) {
			return fmt.Errorf("health check did not pass within %s: %w", c.StartTimeout(), lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// register registers the worker with the Router, recovering when the Router
// already tracks the URL from a previous control plane lifetime.
func (c *Controller) register(ctx context.Context, instance domain.Instance, role domain.Role) (routeradapter.Registration, error) {
	// Router POST is asynchronous and may deduplicate a previous add by URL.
	// Check the existing role before reuse; a healthy HTTP process alone cannot
	// establish that the Router's P/D pool matches the launched process.
	lookupCtx, lookupCancel := c.callContext(ctx)
	existing, found, lookupErr := c.router.FindWorkerByURL(lookupCtx, instance.ServiceURL())
	lookupCancel()
	if lookupErr != nil {
		return routeradapter.Registration{}, lookupErr
	}
	if found {
		if existing.WorkerType == string(role) {
			return routeradapter.Registration{WorkerID: existing.ID, WorkerURL: existing.URL}, nil
		}
		closeCtx, cancel := c.callContext(ctx)
		_, err := c.router.SetReadiness(closeCtx, existing.ID, false)
		cancel()
		if err != nil {
			return routeradapter.Registration{}, err
		}
		cleaner, ok := c.router.(interface {
			RemoveStaleMembership(context.Context, string, string, domain.Role) error
		})
		if !ok {
			return routeradapter.Registration{}, fmt.Errorf("router role mismatch: %s != %s", existing.WorkerType, role)
		}
		cleanCtx, cleanCancel := c.callContext(ctx)
		err = cleaner.RemoveStaleMembership(cleanCtx, existing.ID, instance.ServiceURL(), role)
		cleanCancel()
		if err != nil {
			return routeradapter.Registration{}, err
		}
		c.audit(ctx, store.AuditEntry{Action: "stale_router_role_removed", InstanceID: instance.ID, PartnerID: instance.PartnerID, Details: map[string]string{"from_role": existing.WorkerType, "role": string(role)}})
	}
	callCtx, cancel := c.callContext(ctx)
	// The Router routes inference traffic, so it needs the service address.
	// Using the bootstrap endpoint here would register a URL that answers 404
	// to every request; on a partner platform the two are different ports.
	serviceURL := instance.ServiceURL()
	registration, err := c.router.RegisterWorker(callCtx, routeradapter.RegisterRequest{
		WorkerURL:     serviceURL,
		WorkerType:    role,
		ModelID:       c.cfg.Controller.ModelID,
		BootstrapPort: c.cfg.Router.PrefillBootstrapPort,
	})
	cancel()
	if err == nil {
		return c.waitMembership(ctx, registration, role)
	}
	if !errors.Is(err, routeradapter.ErrAlreadyRegistered) {
		return routeradapter.Registration{}, err
	}
	lookupCtx, lookupCancel = c.callContext(ctx)
	defer lookupCancel()
	existing, found, lookupErr = c.router.FindWorkerByURL(lookupCtx, serviceURL)
	if lookupErr != nil {
		return routeradapter.Registration{}, lookupErr
	}
	if !found {
		return routeradapter.Registration{}, err
	}
	if existing.WorkerType != string(role) {
		return routeradapter.Registration{}, fmt.Errorf("router role mismatch: %s != %s", existing.WorkerType, role)
	}
	c.log().Warn("router already tracked the worker url; reusing the existing membership",
		"instance_id", instance.ID, "worker_id", existing.ID, "endpoint", serviceURL)
	return routeradapter.Registration{WorkerID: existing.ID, WorkerURL: existing.URL}, nil
}

func (c *Controller) waitMembership(ctx context.Context, registration routeradapter.Registration, role domain.Role) (routeradapter.Registration, error) {
	call, cancel := c.callContext(ctx)
	defer cancel()
	for {
		worker, err := c.router.GetWorker(call, registration.WorkerID)
		if err == nil && worker.URL == registration.WorkerURL && worker.WorkerType == string(role) {
			return registration, nil
		}
		if err != nil && !errors.Is(err, routeradapter.ErrWorkerNotFound) {
			return routeradapter.Registration{}, err
		}
		select {
		case <-call.Done():
			return routeradapter.Registration{}, fmt.Errorf("router membership did not acquire role %s: %w", role, call.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ensureReadiness makes sure readiness is routable and returns the generation.
func (c *Controller) ensureReadiness(ctx context.Context, workerID string) (int64, bool, error) {
	callCtx, cancel := c.callContext(ctx)
	defer cancel()
	worker, err := c.router.GetWorker(callCtx, workerID)
	if err != nil {
		return 0, false, err
	}
	if !worker.Healthy || worker.Metadata["__pd_state"] == "draining" {
		_, closeErr := c.router.SetReadiness(callCtx, workerID, false)
		if closeErr != nil {
			return 0, false, closeErr
		}
		return 0, false, fmt.Errorf("router worker is not routable: healthy=%t pd_state=%s", worker.Healthy, worker.Metadata["__pd_state"])
	}
	record, err := c.router.GetReadiness(callCtx, workerID)
	if err != nil {
		if !errors.Is(err, routeradapter.ErrReadinessNotVisible) {
			return 0, false, err
		}
		transition, setErr := c.router.SetReadiness(callCtx, workerID, true)
		if setErr != nil {
			return 0, false, setErr
		}
		return int64(transition.Record.Generation), transition.Record.Ready(), nil
	}
	if record.Ready() {
		return int64(record.Generation), true, nil
	}
	transition, err := c.router.SetReadiness(callCtx, workerID, true)
	if err != nil {
		return 0, false, err
	}
	return int64(transition.Record.Generation), transition.Record.Ready(), nil
}

// servicePort is the port SGLang must bind for this instance: the one its
// service endpoint advertises. Starting the service on any other port leaves
// the Router health-checking an address nothing listens on, which evicts the
// worker seconds after registration - and with it the whole readiness step.
func servicePort(instance domain.Instance) int {
	parsed, err := url.Parse(instance.ServiceURL())
	if err != nil {
		return 0
	}
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			return 443
		}
		return 80
	}
	value, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return value
}

func (c *Controller) failStart(ctx context.Context, instance domain.Instance, operation *store.Operation, message string, cause error) error {
	operation.OperationID = lifecycleOperationID(instance, operation.Type)
	next := instance
	next.ServiceState = domain.ServiceFailed
	next.LastError = message + ": " + cause.Error()
	next.LastSeenAt = c.now()
	exhausted := next.StartAttempts >= c.cfg.Controller.StartMaxAttempts

	operation.Status = store.OpPending
	operation.LastError = next.LastError
	operation.Attempt = next.StartAttempts
	operation.CreatedAt = c.now()
	if exhausted {
		operation.Status = store.OpFailed
	} else {
		operation.NextRetryAt = c.now().Add(c.cfg.Controller.OperationRetry.Duration())
	}

	transition := store.Transition{
		InstanceID: instance.ID,
		Next:       next,
		Operation:  operation,
		Audit: store.AuditEntry{
			Action: "service_start_failed", InstanceID: instance.ID, PartnerID: instance.PartnerID,
			Details: map[string]string{
				"role":      string(instance.Role),
				"attempt":   fmt.Sprintf("%d", next.StartAttempts),
				"exhausted": fmt.Sprintf("%t", exhausted),
				"error":     cause.Error(),
			},
		},
	}
	if applyErr := c.transition(ctx, transition); applyErr != nil {
		return applyErr
	}

	if c.metrics != nil {
		c.metrics.Observe(obs.MetricServiceStartSeconds,
			"SGLang service start duration in seconds.",
			obs.LabelsPlannerRole, 0, "role", string(instance.Role))
		c.metrics.IncCounter(obs.MetricRouterRegFailures,
			"Router worker registration failures.",
			obs.LabelsPartner, "partner", instance.PartnerID)
	}
	c.alert(ctx, obs.Alert{
		Name:       obs.AlertServiceStartFailed,
		Severity:   obs.SeverityWarning,
		InstanceID: instance.ID,
		PartnerID:  instance.PartnerID,
		Message:    message,
		Details: map[string]string{
			"role":      string(instance.Role),
			"attempt":   fmt.Sprintf("%d", next.StartAttempts),
			"max":       fmt.Sprintf("%d", c.cfg.Controller.StartMaxAttempts),
			"exhausted": fmt.Sprintf("%t", exhausted),
			"error":     cause.Error(),
		},
	})
	return fmt.Errorf("%s: %w", message, cause)
}

// BeginDrain implements §7.3: close Router readiness, enter DRAINING and let the
// reconciler observe in-flight traffic reaching zero. DELETE /workers is never
// used as a drain primitive.
//
// The container stays in the control plane after draining so the planner can
// re-assign it a different role.
func (c *Controller) BeginDrain(ctx context.Context, instanceID, reason string, grace time.Duration) error {
	lock := c.instanceOperation(instanceID)
	lock.Lock()
	defer lock.Unlock()
	return c.beginDrain(ctx, instanceID, reason, grace, false)
}

// DrainForRelease drains and then hands the container back to the partner. It is
// the path taken when the partner revoked capacity (§5 and §7.3).
func (c *Controller) DrainForRelease(ctx context.Context, instanceID, reason string, grace time.Duration) error {
	if err := c.RequestReclaim(ctx, instanceID, reason, grace); err != nil {
		return err
	}
	lock := c.instanceOperation(instanceID)
	lock.Lock()
	defer lock.Unlock()
	return c.beginDrain(ctx, instanceID, reason, grace, true)
}

// RequestReclaim records intent only; the periodic reconciler owns network calls
// and retries, including after a process restart or an HTTP client disconnect.
func (c *Controller) RequestReclaim(ctx context.Context, instanceID, reason string, grace time.Duration) error {
	if grace < 0 {
		return errors.New("reclaim grace must not be negative")
	}
	return c.transition(ctx, store.Transition{
		InstanceID: instanceID, RequestRelease: true,
		Next: domain.Instance{DrainDeadlineAt: c.now().Add(grace)},
		Audit: store.AuditEntry{Action: "instance_reclaim_requested", InstanceID: instanceID,
			Details: map[string]string{"reason": reason, "grace": grace.String()}},
	})
}

func (c *Controller) beginDrain(ctx context.Context, instanceID, reason string, grace time.Duration, release bool) error {
	instance, err := c.store.GetInstance(ctx, instanceID)
	if err != nil {
		return err
	}
	if instance.ServiceState == domain.ServiceDraining {
		if release && !instance.PendingRelease {
			// Escalate an in-progress drain into a release intent.
			next := instance
			next.PendingRelease = true
			return c.transition(ctx, store.Transition{
				InstanceID: instanceID,
				Next:       next,
				Audit: store.AuditEntry{
					Action: "drain_escalated_to_release", InstanceID: instanceID, PartnerID: instance.PartnerID,
					Details: map[string]string{"reason": reason},
				},
			})
		}
		return nil
	}
	if !instance.HasService() {
		if release {
			return c.BeginRelease(ctx, instanceID, reason)
		}
		return nil
	}
	if grace < 0 {
		grace = 0
	}
	deadline := c.now().Add(grace)
	if instance.PendingRelease && !instance.DrainDeadlineAt.IsZero() {
		deadline = instance.DrainDeadlineAt
	}

	// Close readiness first: this is the only supported drain primitive (§8).
	if instance.RouterWorkerID != "" {
		callCtx, cancel := c.callContext(ctx)
		_, readinessErr := c.router.SetReadiness(callCtx, instance.RouterWorkerID, false)
		cancel()
		if errors.Is(readinessErr, routeradapter.ErrWorkerNotFound) {
			// The worker is already absent from the Router (router restarted,
			// registration lost, or the service already died). Readiness is
			// closed by definition — draining must proceed, not abort.
			c.log().Info("router worker already absent; skipping readiness close",
				"instance_id", instanceID, "worker_id", instance.RouterWorkerID)
			readinessErr = nil
		}
		if readinessErr != nil {
			if c.metrics != nil {
				c.metrics.IncCounter("readiness_drain_failures_total",
					"Failed attempts to close Router readiness.", obs.LabelsReason, "reason", "router_error")
			}
			c.alert(ctx, obs.Alert{
				Name:       obs.AlertReadinessDrainFailed,
				Severity:   obs.SeverityCritical,
				InstanceID: instanceID,
				PartnerID:  instance.PartnerID,
				Message:    "failed to close router readiness; instance stays serving",
				Details:    map[string]string{"error": readinessErr.Error(), "reason": reason},
			})
			return readinessErr
		}
	}
	// Registration may have reached the Router before its ID was committed to
	// SQLite (for example a crash during readiness). Resolve that membership so
	// reclaim still closes admission and waits for its real outstanding load.
	if instance.RouterWorkerID == "" {
		call, cancel := c.callContext(ctx)
		worker, found, err := c.router.FindWorkerByURL(call, instance.ServiceURL())
		cancel()
		if err != nil {
			return err
		}
		if found {
			instance.RouterWorkerID = worker.ID
			if err := c.closeReadiness(ctx, instance); err != nil {
				return err
			}
		}
	}

	next := instance
	next.ServiceState = domain.ServiceDraining
	next.DrainDeadlineAt = deadline
	next.PendingRelease = release
	next.LastError = ""
	if err := c.transition(ctx, store.Transition{
		InstanceID: instanceID,
		Next:       next,
		Operation: &store.Operation{
			OperationID: drainOperationID(instance),
			InstanceID:  instanceID,
			Type:        store.OpDrain,
			Status:      store.OpInProgress,
			CreatedAt:   c.now(),
		},
		Audit: store.AuditEntry{
			Action: "service_draining", InstanceID: instanceID, PartnerID: instance.PartnerID,
			Details: map[string]string{
				"reason":            reason,
				"grace_seconds":     fmt.Sprintf("%d", int(grace.Seconds())),
				"drain_deadline_at": deadline.Format(time.RFC3339),
				"router_worker_id":  instance.RouterWorkerID,
				"pending_release":   fmt.Sprintf("%t", release),
			},
		},
	}); err != nil {
		return err
	}
	c.log().Info("service draining",
		"instance_id", instanceID, "reason", reason, "grace", grace.String(), "pending_release", release)
	return nil
}

// FinishDrain stops the SGLang service once in-flight traffic reached zero or
// the drain deadline expired, then leaves the instance idle and unreleased so
// the caller can decide between release and re-assignment.
func (c *Controller) FinishDrain(ctx context.Context, instance domain.Instance) error {
	lock := c.instanceOperation(instance.ID)
	lock.Lock()
	defer lock.Unlock()
	return c.finishDrain(ctx, instance)
}

func (c *Controller) finishDrain(ctx context.Context, instance domain.Instance) error {
	current, err := c.store.GetInstance(ctx, instance.ID)
	if err != nil {
		return err
	}
	if current.LeaseID != instance.LeaseID {
		return store.ErrConflict
	}
	instance = current
	if instance.ServiceState != domain.ServiceDraining {
		return nil
	}
	load, loadKnown, err := c.workerLoad(ctx, instance)
	if err != nil {
		c.log().Warn("drain load probe failed", "instance_id", instance.ID, "error", err.Error())
	}
	deadline := instance.DrainDeadlineAt
	timedOut := !deadline.IsZero() && !c.now().Before(deadline)
	if (!loadKnown || load > 0) && !timedOut {
		return nil
	}

	force := false
	if timedOut && (!loadKnown || load > 0) {
		force = true
		if c.metrics != nil {
			c.metrics.IncCounter(obs.MetricServiceDrainTimeout,
				"Drain operations that exceeded their grace period.", obs.LabelsReason, "reason", "deadline")
		}
		c.alert(ctx, obs.Alert{
			Name:       obs.AlertDrainTimeout,
			Severity:   obs.SeverityCritical,
			InstanceID: instance.ID,
			PartnerID:  instance.PartnerID,
			Message:    "drain deadline expired with outstanding or unknown load; forcing service stop",
			Details: map[string]string{
				"in_flight_load": fmt.Sprintf("%d", load),
				"load_known":     fmt.Sprintf("%t", loadKnown),
				"deadline":       deadline.Format(time.RFC3339),
			},
		})
	}

	callCtx, cancel := c.callContext(ctx)
	result, stopErr := c.launcher.Stop(callCtx, instance.Endpoint, launcher.StopRequest{
		Force:  force,
		Reason: "drain complete",
	})
	cancel()
	if stopErr != nil {
		return c.failDrain(ctx, instance, stopErr)
	}
	if result.Phase != launcher.PhaseStopped {
		return c.failDrain(ctx, instance, fmt.Errorf("bootstrap stop did not confirm process exit: phase=%s detail=%s", result.Phase, result.Detail))
	}

	next := instance
	next.ServiceState = domain.ServiceNone
	next.Role = domain.RoleNone
	next.RoleAssignedAt = time.Time{}
	next.StartAttempts = 0
	next.DrainDeadlineAt = time.Time{}
	next.LastSeenAt = c.now()
	next.LastError = ""
	pendingRelease := instance.PendingRelease
	next.PendingRelease = false
	if instance.PendingUpdate != nil && !pendingRelease {
		applyInstanceUpdate(&next, *instance.PendingUpdate)
		next.InstanceState = domain.InstancePreparing
		next.RouterWorkerID = ""
		next.ReadinessGeneration = 0
	}
	if err := c.transition(ctx, store.Transition{
		InstanceID:         instance.ID,
		Next:               next,
		ApplyPendingUpdate: instance.PendingUpdate != nil && !pendingRelease,
		Operation: &store.Operation{
			OperationID: drainOperationID(instance),
			InstanceID:  instance.ID,
			Type:        store.OpDrain,
			Status:      store.OpSucceeded,
			CreatedAt:   c.now(),
		},
		Audit: store.AuditEntry{
			Action: "service_drained", InstanceID: instance.ID, PartnerID: instance.PartnerID,
			Details: map[string]string{
				"exit_code":        fmt.Sprintf("%d", result.ExitCode),
				"forced":           fmt.Sprintf("%t", force),
				"router_worker_id": instance.RouterWorkerID,
			},
		},
	}); err != nil {
		return err
	}
	if c.metrics != nil {
		duration := c.now().Sub(instance.UpdatedAt).Seconds()
		if duration < 0 {
			duration = 0
		}
		c.metrics.Observe(obs.MetricServiceDrainSeconds,
			"Service drain duration in seconds.", obs.LabelsPlannerRole, duration, "role", string(instance.Role))
	}
	c.log().Info("service drained", "instance_id", instance.ID, "forced", force, "exit_code", result.ExitCode)

	if pendingRelease {
		// §7.3: the container is only handed back after the service exited.
		if err := c.BeginRelease(ctx, instance.ID, "drain complete after capacity revocation"); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) failDrain(ctx context.Context, instance domain.Instance, cause error) error {
	next := instance
	next.ServiceState = domain.ServiceDraining
	next.LastError = "drain stop failed: " + cause.Error()
	if err := c.transition(ctx, store.Transition{
		InstanceID: instance.ID,
		Next:       next,
		Operation: &store.Operation{
			OperationID: drainOperationID(instance),
			InstanceID:  instance.ID,
			Type:        store.OpDrain,
			Status:      store.OpPending,
			Attempt:     1,
			NextRetryAt: c.now().Add(c.cfg.Controller.OperationRetry.Duration()),
			LastError:   cause.Error(),
			CreatedAt:   c.now(),
		},
		Audit: store.AuditEntry{
			Action: "service_drain_failed", InstanceID: instance.ID, PartnerID: instance.PartnerID,
			Details: map[string]string{"error": cause.Error()},
		},
	}); err != nil {
		return err
	}
	c.alert(ctx, obs.Alert{
		Name:       obs.AlertReadinessDrainFailed,
		Severity:   obs.SeverityWarning,
		InstanceID: instance.ID,
		PartnerID:  instance.PartnerID,
		Message:    "failed to stop SGLang during drain",
		Details:    map[string]string{"error": cause.Error()},
	})
	return cause
}

// workerLoad reads the Router load of one worker. loadKnown is false when the
// Router could not observe the worker, in which case callers must not assume
// traffic is zero.
func (c *Controller) workerLoad(ctx context.Context, instance domain.Instance) (int64, bool, error) {
	if instance.RouterWorkerID == "" {
		return 0, false, nil
	}
	callCtx, cancel := c.callContext(ctx)
	defer cancel()
	loads, err := c.router.GetLoads(callCtx)
	if err != nil {
		return 0, false, err
	}
	// The Router reports the service URL it was registered with, so the load
	// match must use the same address. Comparing against the bootstrap endpoint
	// made every worker look absent, which reported zero in-flight traffic and
	// let draining finish without waiting.
	target := domain.NormalizeEndpoint(instance.ServiceURL())
	for _, load := range loads {
		if domain.NormalizeEndpoint(load.Worker) != target {
			continue
		}
		if load.Load < 0 {
			return 0, false, fmt.Errorf("router could not read the load of %s", target)
		}
		return load.Load, true, nil
	}
	// Missing membership cannot prove that a previously dispatched request ended.
	return 0, false, nil
}

// BeginRelease hands the container back to the partner (§7.3).
func (c *Controller) BeginRelease(ctx context.Context, instanceID, reason string) error {
	instance, err := c.store.GetInstance(ctx, instanceID)
	if err != nil {
		return err
	}
	if instance.InstanceState == domain.InstanceReleased {
		return nil
	}
	if instance.HasService() {
		return fmt.Errorf("instance %s still runs service state %s; drain it first", instanceID, instance.ServiceState)
	}
	adapter, ok := c.partners.Adapter(instance.PartnerID)
	if !ok {
		return fmt.Errorf("no partner adapter for %q", instance.PartnerID)
	}

	next := instance
	next.InstanceState = domain.InstanceReleasing
	next.LastSeenAt = c.now()
	operation := &store.Operation{
		OperationID: lifecycleOperationID(instance, store.OpRelease),
		InstanceID:  instanceID,
		Type:        store.OpRelease,
		Status:      store.OpInProgress,
		Attempt:     1,
		CreatedAt:   c.now(),
	}
	if previous, err := c.store.GetOperation(ctx, operation.OperationID); err == nil {
		operation.Attempt = previous.Attempt + 1
		operation.CreatedAt = previous.CreatedAt
		if !previous.Status.Terminal() && !previous.NextRetryAt.IsZero() && c.now().Before(previous.NextRetryAt) {
			return nil
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err := c.transition(ctx, store.Transition{
		InstanceID: instanceID,
		Next:       next,
		Operation:  operation,
		Audit: store.AuditEntry{
			Action: "instance_releasing", InstanceID: instanceID, PartnerID: instance.PartnerID,
			Details: map[string]string{"reason": reason, "lease_id": instance.LeaseID},
		},
	}); err != nil {
		return err
	}

	callCtx, cancel := c.callContext(ctx)
	releaseErr := adapter.ReleaseInstance(callCtx, instanceID)
	cancel()
	if releaseErr != nil {
		operation.Status = store.OpPending
		operation.NextRetryAt = c.now().Add(c.cfg.Controller.OperationRetry.Duration())
		operation.LastError = releaseErr.Error()
		next.LastError = "release failed: " + releaseErr.Error()
		if err := c.transition(ctx, store.Transition{
			InstanceID: instanceID,
			Next:       next,
			Operation:  operation,
			Audit: store.AuditEntry{
				Action: "instance_release_failed", InstanceID: instanceID, PartnerID: instance.PartnerID,
				Details: map[string]string{"attempt": fmt.Sprint(operation.Attempt), "error": releaseErr.Error()},
			},
		}); err != nil {
			return err
		}
		c.alert(ctx, obs.Alert{
			Name:       obs.AlertOperationRetryExhausted,
			Severity:   obs.SeverityWarning,
			InstanceID: instanceID,
			PartnerID:  instance.PartnerID,
			Message:    "partner release failed; queued for retry",
			Details:    map[string]string{"error": releaseErr.Error()},
		})
		return releaseErr
	}

	next.InstanceState = domain.InstanceReleased
	next.PendingRelease = false
	next.DrainDeadlineAt = time.Time{}
	next.Role = domain.RoleNone
	next.RoleAssignedAt = time.Time{}
	next.LastError = ""
	operation.Status = store.OpSucceeded
	if err := c.transition(ctx, store.Transition{
		InstanceID: instanceID,
		Next:       next,
		Operation:  operation,
		Audit: store.AuditEntry{
			Action: "instance_released", InstanceID: instanceID, PartnerID: instance.PartnerID,
			Details: map[string]string{"reason": reason, "router_worker_id": instance.RouterWorkerID},
		},
	}); err != nil {
		return err
	}
	c.log().Info("instance released", "instance_id", instanceID, "partner_id", instance.PartnerID, "reason", reason)
	return nil
}

func lifecycleOperationID(instance domain.Instance, kind store.OperationType) string {
	return fmt.Sprintf("%s:%d:%s:%s", kind, len(instance.ID), instance.ID, instance.LeaseID)
}

// markLost parks an instance whose container or lease cannot be trusted (§4.1).
func (c *Controller) markLost(ctx context.Context, instance domain.Instance, reason string) error {
	if instance.InstanceState == domain.InstanceReleased {
		return nil
	}
	// Bootstrap loss does not prove the model is dead. Close admission while
	// retaining the service state and the outstanding load for later recovery.
	closeErr := c.closeReadiness(ctx, instance)
	if instance.InstanceState == domain.InstanceLost {
		return closeErr
	}
	next := instance
	next.InstanceState = domain.InstanceLost
	next.LastError = reason
	next.LastSeenAt = c.now()
	if err := c.transition(ctx, store.Transition{
		InstanceID: instance.ID,
		Next:       next,
		Audit: store.AuditEntry{
			Action: "instance_lost", InstanceID: instance.ID, PartnerID: instance.PartnerID,
			Details: map[string]string{"reason": reason, "service_state": string(instance.ServiceState)},
		},
	}); err != nil {
		return err
	}
	if c.metrics != nil {
		c.metrics.AddCounter(obs.MetricLostInstancesTotal,
			"Instances that became unreachable or lost their lease.", obs.LabelsReason, 1, "reason", "unreachable")
	}
	c.alert(ctx, obs.Alert{
		Name:       obs.AlertInstanceLostRateHigh,
		Severity:   obs.SeverityCritical,
		InstanceID: instance.ID,
		PartnerID:  instance.PartnerID,
		Message:    "instance is LOST; it must not be scheduled",
		Details:    map[string]string{"reason": reason, "endpoint": instance.Endpoint},
	})
	return closeErr
}

func (c *Controller) closeReadiness(ctx context.Context, instance domain.Instance) error {
	workerID := instance.RouterWorkerID
	if workerID == "" && instance.HasService() {
		call, cancel := c.callContext(ctx)
		worker, found, err := c.router.FindWorkerByURL(call, instance.ServiceURL())
		cancel()
		if err != nil {
			return err
		}
		if found {
			workerID = worker.ID
		}
	}
	if workerID == "" {
		return nil
	}
	call, cancel := c.callContext(ctx)
	defer cancel()
	_, err := c.router.SetReadiness(call, workerID, false)
	if errors.Is(err, routeradapter.ErrWorkerNotFound) {
		return nil
	}
	return err
}

func applyInstanceUpdate(instance *domain.Instance, update domain.InstanceUpdate) {
	instance.Endpoint = update.Endpoint
	instance.ServiceEndpoint = update.ServiceEndpoint
	instance.LeaseID = update.LeaseID
	instance.Spec = update.Spec
	instance.LeaseUpdatedAt = update.ObservedAt
	instance.PrepareAttempts = 0
	instance.StartAttempts = 0
	instance.PendingUpdate = nil
}

// Probe verifies that a container is still reachable and reports its phase.
func (c *Controller) Probe(ctx context.Context, instance domain.Instance) (launcher.Health, error) {
	callCtx, cancel := c.callContext(ctx)
	defer cancel()
	return c.launcher.Health(callCtx, instance.Endpoint)
}

// PartnerAdapter resolves the adapter for an instance so the reconciler can use it.
func (c *Controller) PartnerAdapter(partnerID string) (partner.PartnerAdapter, bool) {
	return c.partners.Adapter(partnerID)
}

func mergeDetails(base, extra map[string]string) map[string]string {
	if len(extra) == 0 {
		return base
	}
	merged := make(map[string]string, len(base)+len(extra))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range extra {
		merged[key] = value
	}
	return merged
}
