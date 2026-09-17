package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
)

const (
	testBootstrapVersion = "tai-talea-bootstrap/1"
	testModelID          = "model-a"
	testBootstrapToken   = "bootstrap-token-0123456789"
)

// ---------------------------------------------------------------- fakes

type bootstrapFake struct {
	mu           sync.Mutex
	phase        string
	role         string
	servicePID   int
	startCalls   int
	stopCalls    int
	statusCalls  int
	unauthorized int
	failStart    bool
	down         bool
	env          launcher.Environment
	server       *httptest.Server
}

func newBootstrapFake(env launcher.Environment) *bootstrapFake {
	fake := &bootstrapFake{phase: launcher.PhaseIdle, env: env}
	mux := http.NewServeMux()
	mux.HandleFunc(launcher.PathHealth, fake.health)
	mux.HandleFunc(launcher.PathStatus, fake.status)
	mux.HandleFunc(launcher.PathStart, fake.start)
	mux.HandleFunc(launcher.PathStop, fake.stop)
	// The real bootstrap refuses every request that does not carry the shared
	// secret, so the fake does too. That is what makes these tests prove the
	// control plane actually sends the header instead of merely tolerating it.
	fake.server = httptest.NewServer(fake.requireToken(mux))
	return fake
}

// requireToken mimics §9's fail-closed interface: no valid secret, no service.
func (f *bootstrapFake) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get(launcher.HeaderToken) != testBootstrapToken {
			f.mu.Lock()
			f.unauthorized++
			f.mu.Unlock()
			writeTestJSON(writer, http.StatusUnauthorized,
				map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(writer, request)
	})
}

// rejected counts requests the fake turned away for a missing secret.
func (f *bootstrapFake) rejected() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unauthorized
}

func (f *bootstrapFake) Close() { f.server.Close() }

func (f *bootstrapFake) Endpoint() string { return f.server.URL }

// SetDown simulates a container that stopped answering.
func (f *bootstrapFake) SetDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

// SetEnvironment simulates a container built from a different image.
func (f *bootstrapFake) SetEnvironment(env launcher.Environment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.env = env
}

func (f *bootstrapFake) health(writer http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		writeTestJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "container unavailable"})
		return
	}
	writeTestJSON(writer, http.StatusOK, launcher.Health{
		Status:  "ok",
		Phase:   f.phase,
		Version: testBootstrapVersion,
	})
}

func (f *bootstrapFake) status(writer http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	if f.down {
		writeTestJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "container unavailable"})
		return
	}
	writeTestJSON(writer, http.StatusOK, launcher.Status{
		Phase:       f.phase,
		Role:        f.role,
		ServicePID:  f.servicePID,
		Environment: f.env,
	})
}

func (f *bootstrapFake) start(writer http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	if f.down {
		writeTestJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "container unavailable"})
		return
	}
	if f.failStart {
		writeTestJSON(writer, http.StatusInternalServerError, map[string]string{"error": "sglang did not start"})
		return
	}
	var payload launcher.StartRequest
	_ = json.NewDecoder(request.Body).Decode(&payload)
	f.role = string(payload.Role)
	f.phase = launcher.PhaseRunning
	f.servicePID = 4242
	writer.WriteHeader(http.StatusAccepted)
}

func (f *bootstrapFake) stop(writer http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls++
	if f.down {
		writeTestJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "container unavailable"})
		return
	}
	f.phase = launcher.PhaseStopped
	f.servicePID = 0
	writeTestJSON(writer, http.StatusOK, launcher.StopResult{Phase: launcher.PhaseStopped, ExitCode: 0})
}

func (f *bootstrapFake) counts() (start, stop, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startCalls, f.stopCalls, f.statusCalls
}

type fakeWorker struct {
	ID         string            `json:"id"`
	URL        string            `json:"url"`
	WorkerType string            `json:"worker_type"`
	Healthy    bool              `json:"is_healthy"`
	Load       int64             `json:"load"`
	State      string            `json:"state"`
	Generation uint64            `json:"generation"`
	Reason     string            `json:"unavailable_reason,omitempty"`
	labels     map[string]string `json:"-"`
}

type routerFake struct {
	mu            sync.Mutex
	workers       map[string]*fakeWorker
	byURL         map[string]string
	sequence      int
	generation    uint64
	deleteCalls   int
	registerCalls int
	readinessSets []string
	server        *httptest.Server
}

func newRouterFake() *routerFake {
	fake := &routerFake{workers: map[string]*fakeWorker{}, byURL: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/workers", fake.workersHandler)
	mux.HandleFunc("/workers/", fake.workerHandler)
	mux.HandleFunc("/state/workers", fake.readinessList)
	mux.HandleFunc("/state/workers/", fake.readinessItem)
	mux.HandleFunc("/get_loads", fake.loads)
	fake.server = httptest.NewServer(mux)
	return fake
}

func (f *routerFake) Close() { f.server.Close() }

func (f *routerFake) URL() string { return f.server.URL }

// setLoad makes the Router report in-flight requests for one worker url. It
// returns false when no such worker is registered.
func (f *routerFake) setLoad(url string, load int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, worker := range f.workers {
		if domain.NormalizeEndpoint(worker.URL) == domain.NormalizeEndpoint(url) {
			worker.Load = load
			return true
		}
	}
	return false
}

// Reset empties the worker pool, which is what a Router restart looks like.
func (f *routerFake) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workers = map[string]*fakeWorker{}
	f.byURL = map[string]string{}
}

func (f *routerFake) workerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.workers)
}

func (f *routerFake) deletes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleteCalls
}

func (f *routerFake) workerByURL(url string) (fakeWorker, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, worker := range f.workers {
		if domain.NormalizeEndpoint(worker.URL) == domain.NormalizeEndpoint(url) {
			return *worker, true
		}
	}
	return fakeWorker{}, false
}

func (f *routerFake) workerHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodDelete {
		// The control plane must never delete a worker to drain it.
		f.mu.Lock()
		f.deleteCalls++
		f.mu.Unlock()
		writeTestJSON(writer, http.StatusAccepted, map[string]string{"worker_id": "deleted"})
		return
	}
	id := strings.TrimPrefix(request.URL.Path, "/workers/")
	f.mu.Lock()
	defer f.mu.Unlock()
	worker, ok := f.workers[id]
	if !ok {
		writeTestJSON(writer, http.StatusNotFound, map[string]string{"error": "worker_not_found"})
		return
	}
	writeTestJSON(writer, http.StatusOK, worker)
}

func (f *routerFake) workersHandler(writer http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if request.Method == http.MethodGet {
		workers := make([]fakeWorker, 0, len(f.workers))
		for _, worker := range f.workers {
			workers = append(workers, *worker)
		}
		writeTestJSON(writer, http.StatusOK, map[string]any{"workers": workers})
		return
	}
	if request.Method != http.MethodPost {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		URL        string `json:"url"`
		WorkerType string `json:"worker_type"`
		ModelID    string `json:"model_id"`
	}
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		writeTestJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_payload"})
		return
	}
	if existing, ok := f.byURL[payload.URL]; ok {
		writeTestJSON(writer, http.StatusConflict, map[string]string{
			"error":   "worker_exists",
			"message": "worker already exists",
			"id":      existing,
		})
		return
	}
	f.registerCalls++
	f.sequence++
	f.generation++
	id := fmt.Sprintf("w-%d", f.sequence)
	worker := &fakeWorker{
		ID: id, URL: payload.URL, WorkerType: payload.WorkerType,
		Healthy: true, Load: 0, State: "ready", Generation: f.generation,
	}
	f.workers[id] = worker
	f.byURL[payload.URL] = id
	writeTestJSON(writer, http.StatusAccepted, map[string]any{"worker_id": id, "url": payload.URL})
}

func (f *routerFake) readinessList(writer http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	records := make([]map[string]any, 0, len(f.workers))
	for _, worker := range f.workers {
		records = append(records, readinessPayload(worker))
	}
	writeTestJSON(writer, http.StatusOK, records)
}

func (f *routerFake) readinessItem(writer http.ResponseWriter, request *http.Request) {
	id := strings.TrimPrefix(request.URL.Path, "/state/workers/")
	f.mu.Lock()
	defer f.mu.Unlock()
	worker, ok := f.workers[id]
	if !ok {
		writeTestJSON(writer, http.StatusNotFound, map[string]string{"error": "worker_not_found"})
		return
	}
	switch request.Method {
	case http.MethodGet:
		writeTestJSON(writer, http.StatusOK, readinessPayload(worker))
	case http.MethodPut:
		var payload struct {
			State  string `json:"state"`
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			writeTestJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_payload"})
			return
		}
		previous := worker.State
		f.generation++
		worker.Generation = f.generation
		worker.State = payload.State
		worker.Reason = ""
		if payload.State != "ready" {
			worker.Reason = payload.Reason
			if worker.Reason == "" {
				worker.Reason = "manual"
			}
		}
		worker.Load = 0
		f.readinessSets = append(f.readinessSets, id+":"+payload.State)
		writeTestJSON(writer, http.StatusOK, map[string]any{
			"previous_state": previous,
			"record":         readinessPayload(worker),
			"changed":        previous != payload.State,
		})
	default:
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *routerFake) loads(writer http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	loads := make([]map[string]any, 0, len(f.workers))
	for _, worker := range f.workers {
		loads = append(loads, map[string]any{
			"worker":      worker.URL,
			"worker_type": worker.WorkerType,
			"load":        worker.Load,
		})
	}
	writeTestJSON(writer, http.StatusOK, map[string]any{"loads": loads})
}

func (f *routerFake) readinessOf(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if worker, ok := f.workers[id]; ok {
		return worker.State
	}
	return ""
}

func readinessPayload(worker *fakeWorker) map[string]any {
	payload := map[string]any{
		"worker_id":  worker.ID,
		"worker_url": worker.URL,
		"state":      worker.State,
		"generation": worker.Generation,
	}
	if worker.State != "ready" {
		reason := worker.Reason
		if reason == "" {
			reason = "manual"
		}
		payload["unavailable_reason"] = reason
	}
	return payload
}

// routingTransport sends every request to the fake container regardless of the
// address the control plane was asked to talk to.
type routingTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t *routingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	clone.Host = t.target.Host
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

type alertRecorder struct {
	mu     sync.Mutex
	alerts []obs.Alert
}

func (r *alertRecorder) Fire(_ context.Context, alert obs.Alert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alerts = append(r.alerts, alert)
	return nil
}

func (r *alertRecorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.alerts))
	for _, alert := range r.alerts {
		names = append(names, alert.Name)
	}
	return names
}

func (r *alertRecorder) has(name string) bool {
	for _, candidate := range r.names() {
		if candidate == name {
			return true
		}
	}
	return false
}

func (r *alertRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alerts = nil
}

// ---------------------------------------------------------------- harness

type harness struct {
	t         *testing.T
	cfg       config.Config
	store     *store.SQLiteStore
	bootstrap *bootstrapFake
	router    *routerFake
	adapter   *partner.StaticAdapter
	recorder  *alertRecorder
	metrics   *obs.Registry
	ctrl      *Controller
	clock     *testClock
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(delta time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(delta)
}

func newHarness(t *testing.T, instances ...partner.CapacityInstance) *harness {
	t.Helper()
	if len(instances) == 0 {
		instances = []partner.CapacityInstance{{ID: "container-1", Endpoint: "http://10.0.0.1:8080", LeaseID: "lease-1"}}
	}

	clock := &testClock{now: time.Now().UTC()}
	env := launcher.Environment{
		OS: "ubuntu-22.04", CUDA: "12.4", Python: "3.11", SGLang: "0.4.6",
		Wheelhouse: "/opt/tai-talea/wheelhouse", Virtualenv: "/opt/tai-talea/venv",
	}
	bootstrapFake := newBootstrapFake(env)
	t.Cleanup(bootstrapFake.Close)
	routerFake := newRouterFake()
	t.Cleanup(routerFake.Close)

	cfg := config.Default()
	cfg.Server.AdminToken = "admin-token-0123456789"
	cfg.Storage.SQLitePath = filepath.Join(t.TempDir(), "tai-talea.db")
	cfg.Router.Name = "primary"
	cfg.Router.URL = routerFake.URL()
	cfg.Router.Mode = routeradapter.ModeReadinessV1
	cfg.Router.Timeout = config.Duration(5 * time.Second)
	cfg.Controller.ModelID = testModelID
	cfg.Controller.CallTimeout = config.Duration(3 * time.Second)
	cfg.Controller.BootstrapToken = testBootstrapToken
	cfg.Controller.ReconcileInterval = config.Duration(10 * time.Millisecond)
	cfg.Controller.DrainGrace = config.Duration(60 * time.Second)
	cfg.Controller.OperationRetry = config.Duration(0)
	cfg.Controller.LeaseWarning = config.Duration(10 * time.Minute)
	cfg.ImageProfile = config.ImageProfile{
		OS: "ubuntu-22.04", CUDA: "12.4", Python: "3.11", SGLang: "0.4.6",
		Wheelhouse: "/opt/tai-talea/wheelhouse", Virtualenv: "/opt/tai-talea/venv",
	}
	cfg.Planner = config.PlannerConfig{Gear: "1:1", MaxChangePerRound: 8}
	cfg.Pull.Enabled = false

	staticInstances := make([]config.StaticInstanceConfig, 0, len(instances))
	for _, instance := range instances {
		staticInstances = append(staticInstances, config.StaticInstanceConfig{
			ID: instance.ID, Endpoint: instance.Endpoint, LeaseID: instance.LeaseID, Spec: instance.Spec,
		})
	}
	// The static adapter is built directly here so the harness owns the value
	// and no configuration round trip is needed.
	adapterInstances := make([]partner.CapacityInstance, 0, len(instances))
	for _, instance := range instances {
		instance.Spec = domain.InstanceSpec{GPU: "H100", GPUCount: 1, ModelSupport: []string{testModelID}}
		adapterInstances = append(adapterInstances, instance)
	}
	// Keep the configured capacity in step with the adapter.
	staticInstances = staticInstances[:0]
	for _, instance := range adapterInstances {
		staticInstances = append(staticInstances, config.StaticInstanceConfig{
			ID: instance.ID, Endpoint: instance.Endpoint, LeaseID: instance.LeaseID, Spec: instance.Spec,
		})
	}
	adapter, err := partner.NewStaticAdapter("partner-a", adapterInstances)
	if err != nil {
		t.Fatalf("static adapter: %v", err)
	}
	cfg.Partners = []config.PartnerConfig{{
		ID: "partner-a", Adapter: "static", PushToken: "partner-a-token",
		PushSecret: "partner-a-secret-0123456789", HMACTolerance: config.Duration(5 * time.Minute),
		Instances: staticInstances,
	}}

	persistence, err := store.OpenSQLite(cfg.Storage.SQLitePath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { persistence.Close() })

	metrics := obs.NewRegistry()
	recorder := &alertRecorder{}
	ctrl := buildController(t, cfg, persistence, routerFake, bootstrapFake, adapter, metrics, recorder, clock)
	return &harness{
		t: t, cfg: cfg, store: persistence, bootstrap: bootstrapFake, router: routerFake,
		adapter: adapter, recorder: recorder, metrics: metrics, ctrl: ctrl, clock: clock,
	}
}

// staticRegistry satisfies PartnerResolver with a single adapter.
type staticRegistry struct {
	adapter partner.PartnerAdapter
}

func (r *staticRegistry) Adapter(partnerID string) (partner.PartnerAdapter, bool) {
	if partnerID == r.adapter.PartnerID() {
		return r.adapter, true
	}
	return nil, false
}

func buildController(
	t *testing.T,
	cfg config.Config,
	persistence store.Store,
	routerFake *routerFake,
	bootstrapFake *bootstrapFake,
	adapter partner.PartnerAdapter,
	metrics *obs.Registry,
	recorder *alertRecorder,
	clock *testClock,
) *Controller {
	t.Helper()
	routerClient, err := routeradapter.New(cfg.Router.ToAdapter())
	if err != nil {
		t.Fatalf("router adapter: %v", err)
	}
	// Container endpoints are arbitrary addresses in the tests, so every
	// bootstrap call is routed to the fake container.
	target, err := url.Parse(bootstrapFake.Endpoint())
	if err != nil {
		t.Fatalf("parse fake endpoint: %v", err)
	}
	launcherClient, err := launcher.NewWithHTTPClient(&http.Client{
		Timeout:   cfg.Controller.CallTimeout.Duration(),
		Transport: &routingTransport{target: target, base: http.DefaultTransport},
	}, cfg.Controller.BootstrapToken)
	if err != nil {
		t.Fatalf("launcher: %v", err)
	}
	manager, err := instancemanager.New(launcherClient, instancemanager.Profile{
		OS: cfg.ImageProfile.OS, CUDA: cfg.ImageProfile.CUDA, Python: cfg.ImageProfile.Python,
		SGLang: cfg.ImageProfile.SGLang, Wheelhouse: cfg.ImageProfile.Wheelhouse,
		Virtualenv: cfg.ImageProfile.Virtualenv, BootstrapVersion: testBootstrapVersion,
		ModelID: cfg.Controller.ModelID, MaxLeaseAge: cfg.Controller.LeaseWarning.Duration(),
	})
	if err != nil {
		t.Fatalf("instance manager: %v", err)
	}
	plannerInstance, err := planner.New(cfg.Planner.ToPlanner())
	if err != nil {
		t.Fatalf("planner: %v", err)
	}
	alerter := obs.NewAlerter(metrics, nil, 0, recorder)
	ctrl, err := New(Deps{
		Config:   cfg,
		Store:    persistence,
		Router:   routerClient,
		Launcher: launcherClient,
		Manager:  manager,
		Partners: &staticRegistry{adapter: adapter},
		Planner:  plannerInstance,
		Metrics:  metrics,
		Alerter:  alerter,
		Now:      clock.Now,
	})
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	return ctrl
}

func (h *harness) addedEvent(id string, endpoint string, lease string, at time.Time) domain.CapacityEvent {
	spec := domain.InstanceSpec{GPU: "H100", GPUCount: 1, ModelSupport: []string{testModelID}}
	return domain.CapacityEvent{
		EventID:    "partner-a:add:" + id,
		PartnerID:  "partner-a",
		Type:       domain.EventCapacityAdded,
		OccurredAt: at,
		Instance:   &domain.EventInstance{ID: id, Endpoint: endpoint, LeaseID: lease, Spec: &spec},
		Source:     domain.SourcePush,
		ReceivedAt: at,
	}
}

func (h *harness) revokedEvent(id string, grace int, at time.Time) domain.CapacityEvent {
	return domain.CapacityEvent{
		EventID:      "partner-a:revoke:" + id,
		PartnerID:    "partner-a",
		Type:         domain.EventCapacityRevoked,
		OccurredAt:   at,
		Instance:     &domain.EventInstance{ID: id},
		GraceSeconds: grace,
		Source:       domain.SourcePush,
		ReceivedAt:   at,
	}
}

func (h *harness) instance(id string) domain.Instance {
	h.t.Helper()
	instance, err := h.store.GetInstance(context.Background(), id)
	if err != nil {
		h.t.Fatalf("get instance %s: %v", id, err)
	}
	return instance
}

func (h *harness) handle(event domain.CapacityEvent) domain.EventOutcome {
	h.t.Helper()
	outcome, err := h.ctrl.HandleCapacityEvent(context.Background(), event)
	if err != nil {
		h.t.Fatalf("handle event %s: %v", event.EventID, err)
	}
	return outcome
}

func (h *harness) reconcile() ReconcileReport {
	h.t.Helper()
	report, err := h.ctrl.ReconcileOnce(context.Background())
	if err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
	return report
}

func writeTestJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}

// ------------------------------------------------------------- acceptance

// Acceptance: §17 M1 "新实例能从 ADDED 进入 IDLE".
func TestAcceptanceAddedEventReachesIdle(t *testing.T) {
	h := newHarness(t)
	event := h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now())
	outcome := h.handle(event)
	if !outcome.Accepted || outcome.Duplicate {
		t.Fatalf("the first ADDED must be accepted exactly once: %+v", outcome)
	}

	instance := h.instance("container-1")
	if instance.InstanceState != domain.InstanceIdle {
		t.Fatalf("instance_state=%s, want IDLE", instance.InstanceState)
	}
	if instance.ServiceState != domain.ServiceNone {
		t.Fatalf("service_state=%s, want NONE", instance.ServiceState)
	}
	if instance.LeaseID != "lease-1" {
		t.Fatalf("lease=%s, want lease-1", instance.LeaseID)
	}

	entries, err := h.store.ListAudit(context.Background(), "container-1", 20)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	assertAuditContains(t, entries, "capacity_added", "instance_allocating", "instance_prepared")

	// The container must be readable and legal in its current combination.
	if verdict := domain.CheckCombination(instance.InstanceState, instance.ServiceState, instance.Role); !verdict.Legal {
		t.Fatalf("IDLE/NONE must be legal: %+v", verdict)
	}
}

// Acceptance: §17 M1 "重复事件不会重复启动或回收实例".
func TestAcceptanceDuplicateEventDoesNotRepeatLifecycle(t *testing.T) {
	h := newHarness(t)
	event := h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now())
	h.handle(event)

	_, _, statusCallsBefore := h.bootstrap.counts()
	entriesBefore, err := h.store.ListAudit(context.Background(), "container-1", 50)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}

	// The exact same event id is delivered again by the partner.
	outcome := h.handle(event)
	if !outcome.Accepted {
		t.Fatalf("a duplicate must still be accepted: %+v", outcome)
	}
	if !outcome.Duplicate {
		t.Fatalf("the second delivery must be reported as a duplicate: %+v", outcome)
	}
	if !strings.Contains(outcome.Reason, "not repeated") {
		t.Fatalf("the outcome must explain that the lifecycle was not repeated: %q", outcome.Reason)
	}

	_, _, statusCallsAfter := h.bootstrap.counts()
	if statusCallsAfter != statusCallsBefore {
		t.Fatalf("a duplicate event must not probe the container again: before=%d after=%d",
			statusCallsBefore, statusCallsAfter)
	}
	entriesAfter, err := h.store.ListAudit(context.Background(), "container-1", 50)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(entriesAfter) != len(entriesBefore) {
		t.Fatalf("a duplicate event must not write lifecycle audit rows: %d -> %d",
			len(entriesBefore), len(entriesAfter))
	}
	if instance := h.instance("container-1"); instance.InstanceState != domain.InstanceIdle {
		t.Fatalf("instance_state=%s, want IDLE after the duplicate", instance.InstanceState)
	}
}

// Acceptance: §17 M1 "非法状态转换会被拒绝并告警".
func TestAcceptanceIllegalTransitionIsRefusedAndAlerted(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
	h.recorder.reset()

	// IDLE + SERVING without an assigned role is the documented illegal pair.
	instance := h.instance("container-1")
	illegal := instance
	illegal.ServiceState = domain.ServiceServing

	err := h.ctrl.transition(context.Background(), store.Transition{InstanceID: instance.ID, Next: illegal})
	if err == nil {
		t.Fatal("an illegal state transition must be refused")
	}
	var violation *domain.Violation
	if !errors.As(err, &violation) {
		t.Fatalf("expected a structured violation, got %v", err)
	}
	if violation.Code != domain.CodeIllegalCombination && violation.Code != domain.CodeIllegalServiceTransition {
		t.Fatalf("unexpected violation code %s", violation.Code)
	}
	if violation.Severity != domain.SeverityReject {
		t.Fatalf("severity=%s, want REJECT", violation.Severity)
	}
	if !h.recorder.has(obs.AlertIllegalStateTransition) {
		t.Fatalf("an illegal transition must raise an alert, got %v", h.recorder.names())
	}

	// Nothing may be silently corrected on disk.
	stored := h.instance("container-1")
	if stored.ServiceState != domain.ServiceNone || stored.InstanceState != domain.InstanceIdle {
		t.Fatalf("the refused change must not be persisted: %s/%s", stored.InstanceState, stored.ServiceState)
	}
	if h.metrics.PointInTime("capacity_illegal_transitions_total", "code", violation.Code) == 0 {
		t.Fatal("the refused transition must be counted")
	}
}

// Acceptance: §17 M1 "服务能完成启动、健康检查和 Router 注册".
func TestAcceptanceServiceStartHealthAndRegistration(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))

	if err := h.ctrl.StartService(context.Background(), "container-1", domain.RolePrefill); err != nil {
		t.Fatalf("start service: %v", err)
	}

	instance := h.instance("container-1")
	if instance.ServiceState != domain.ServiceServing {
		t.Fatalf("service_state=%s, want SERVING", instance.ServiceState)
	}
	if instance.Role != domain.RolePrefill {
		t.Fatalf("role=%s, want prefill", instance.Role)
	}
	if instance.RouterWorkerID == "" {
		t.Fatal("SERVING requires a registered router worker id")
	}
	if instance.ReadinessGeneration == 0 {
		t.Fatal("SERVING requires a readiness generation")
	}

	worker, ok := h.router.workerByURL(h.bootstrap.Endpoint())
	if !ok {
		t.Fatalf("the router must know the worker url %s", h.bootstrap.Endpoint())
	}
	if worker.ID != instance.RouterWorkerID {
		t.Fatalf("router worker id=%s, instance recorded %s", worker.ID, instance.RouterWorkerID)
	}
	if worker.WorkerType != "prefill" {
		t.Fatalf("router worker type=%s, want prefill", worker.WorkerType)
	}
	if h.router.readinessOf(worker.ID) != "ready" {
		t.Fatalf("readiness=%s, want ready", h.router.readinessOf(worker.ID))
	}

	startCalls, _, _ := h.bootstrap.counts()
	if startCalls != 1 {
		t.Fatalf("bootstrap start calls=%d, want 1", startCalls)
	}
	if h.router.deletes() != 0 {
		t.Fatal("the control plane must never delete a router worker")
	}
}

// Acceptance: §17 M1 "摘流不会使用 DELETE".
func TestAcceptanceRevocationDrainsThroughReadinessOnly(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
	if err := h.ctrl.StartService(context.Background(), "container-1", domain.RoleDecode); err != nil {
		t.Fatalf("start service: %v", err)
	}
	workerID := h.instance("container-1").RouterWorkerID

	outcome := h.handle(h.revokedEvent("container-1", 60, h.clock.Now()))
	if !outcome.Accepted {
		t.Fatalf("the revoke must be accepted: %+v", outcome)
	}

	draining := h.instance("container-1")
	if draining.ServiceState != domain.ServiceDraining {
		t.Fatalf("service_state=%s, want DRAINING", draining.ServiceState)
	}
	if draining.InstanceState != domain.InstanceIdle {
		t.Fatalf("instance_state=%s, the container must stay in the control plane while draining", draining.InstanceState)
	}
	if h.router.readinessOf(workerID) != "unavailable" {
		t.Fatalf("readiness=%s, want unavailable", h.router.readinessOf(workerID))
	}
	if h.router.deletes() != 0 {
		t.Fatal("draining must not use DELETE /workers")
	}
	if draining.DrainDeadlineAt.IsZero() {
		t.Fatal("a drain must carry a deadline derived from grace_seconds")
	}

	// The reconciler observes zero in-flight load and finishes the drain.
	h.reconcile()
	released := h.instance("container-1")
	if released.ServiceState != domain.ServiceNone {
		t.Fatalf("service_state=%s, want NONE after the drain", released.ServiceState)
	}
	if released.InstanceState != domain.InstanceReleased {
		t.Fatalf("instance_state=%s, want RELEASED", released.InstanceState)
	}
	if _, stopCalls, _ := h.bootstrap.counts(); stopCalls != 1 {
		t.Fatalf("bootstrap stop calls=%d, want 1", stopCalls)
	}
	if !h.adapter.Released("container-1") {
		t.Fatal("the container must be handed back to the partner after the service exited")
	}
	if h.router.deletes() != 0 {
		t.Fatal("the whole revoke path must not delete router workers")
	}

	// The revoke must be idempotent: a second delivery changes nothing.
	second := h.handle(h.revokedEvent("container-1", 60, h.clock.Now()))
	if !second.Duplicate {
		t.Fatalf("a repeated revoke must be a duplicate: %+v", second)
	}
}

// A Router restart or a lost registration must not wedge the drain: the worker
// is already absent, so closing its readiness is a no-op by definition and the
// service must still be retired. Found during the 2026-09-17 live run, where
// FAILED instances kept stale worker ids from the replaced Router and every
// drain attempt aborted with "router worker not found".
func TestDrainProceedsWhenTheRouterWorkerIsGone(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
	if err := h.ctrl.StartService(context.Background(), "container-1", domain.RoleDecode); err != nil {
		t.Fatalf("start service: %v", err)
	}

	// Exactly what replacing the Router looks like: the pool is empty, but the
	// control plane still remembers the old worker id.
	h.router.Reset()

	if err := h.ctrl.BeginDrain(context.Background(), "container-1", "service failed with a stale worker id", 0); err != nil {
		t.Fatalf("drain must tolerate an absent router worker: %v", err)
	}
	draining := h.instance("container-1")
	if draining.ServiceState != domain.ServiceDraining {
		t.Fatalf("service_state=%s, want DRAINING", draining.ServiceState)
	}
	if h.router.deletes() != 0 {
		t.Fatal("draining must not use DELETE /workers even for an absent worker")
	}

	// The reconciler observes zero in-flight load and finishes the drain.
	h.reconcile()
	idle := h.instance("container-1")
	if idle.ServiceState != domain.ServiceNone {
		t.Fatalf("service_state=%s, want NONE after the drain", idle.ServiceState)
	}
	if idle.InstanceState != domain.InstanceIdle {
		t.Fatalf("instance_state=%s, want IDLE (no release was requested)", idle.InstanceState)
	}
	if idle.Role != domain.RoleNone {
		t.Fatalf("role=%s, want NONE so the planner can re-assign", idle.Role)
	}
	if idle.StartAttempts != 0 {
		t.Fatalf("start_attempts=%d, want the retry budget reset", idle.StartAttempts)
	}
}

// Acceptance: §17 M1 "控制面重启后能通过 reconcile 恢复状态".
func TestAcceptanceReconcileRecoversAfterRestart(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
	if err := h.ctrl.StartService(context.Background(), "container-1", domain.RolePrefill); err != nil {
		t.Fatalf("start service: %v", err)
	}
	originalWorker := h.instance("container-1").RouterWorkerID

	// A restart must never trust the database alone: the Router pool is emptied
	// first, which is exactly what a Router restart looks like.
	h.router.Reset()

	restarted := buildController(t, h.cfg, h.store, h.router, h.bootstrap, h.adapter, h.metrics, h.recorder, h.clock)
	report, resumed, err := restarted.Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.Checked == 0 {
		t.Fatal("recovery must inspect the tracked instances")
	}
	_ = resumed

	recovered := h.instance("container-1")
	if recovered.ServiceState != domain.ServiceServing {
		t.Fatalf("service_state=%s, want SERVING after recovery", recovered.ServiceState)
	}
	if recovered.RouterWorkerID == "" || recovered.RouterWorkerID == originalWorker {
		t.Fatalf("the worker must be re-registered with a new identity, got %q (previous %q)",
			recovered.RouterWorkerID, originalWorker)
	}
	if h.router.readinessOf(recovered.RouterWorkerID) != "ready" {
		t.Fatal("the recovered worker must be routable")
	}
	if h.router.deletes() != 0 {
		t.Fatal("recovery must not delete router workers")
	}

	// A second recovery round observes no change: the state is stable.
	second, _, err := restarted.Recover(context.Background())
	if err != nil {
		t.Fatalf("second recovery: %v", err)
	}
	if len(second.Reregister) != 0 {
		t.Fatalf("a stable instance must not be re-registered again: %+v", second.Reregister)
	}
	stable := h.instance("container-1")
	if stable.RouterWorkerID != recovered.RouterWorkerID {
		t.Fatalf("worker identity changed after a stable round: %s -> %s",
			recovered.RouterWorkerID, stable.RouterWorkerID)
	}
}

// §17 M1 "固定 P/D 比例和手动档位" plus idle-first assignment.
func TestAcceptancePlannerAppliesTheFixedRatioGear(t *testing.T) {
	h := newHarness(t,
		partner.CapacityInstance{ID: "container-1", Endpoint: "http://10.0.0.1:8080", LeaseID: "lease-1"},
		partner.CapacityInstance{ID: "container-2", Endpoint: "http://10.0.0.2:8080", LeaseID: "lease-2"},
	)
	for _, id := range []string{"container-1", "container-2"} {
		suffix := strings.TrimPrefix(id, "container-")
		endpoint := "http://10.0.0." + suffix + ":8080"
		h.handle(h.addedEvent(id, endpoint, "lease-"+suffix, h.clock.Now()))
		if instance := h.instance(id); instance.InstanceState != domain.InstanceIdle {
			t.Fatalf("%s must reach IDLE before the planner runs, got %s", id, instance.InstanceState)
		}
	}

	decision, err := h.ctrl.Rebalance(context.Background())
	if err != nil {
		t.Fatalf("rebalance: %v", err)
	}
	if decision.Gear != "1:1" {
		t.Fatalf("gear=%s, want 1:1", decision.Gear)
	}
	if len(decision.Actions) != 2 {
		t.Fatalf("two idle instances under a 1:1 gear must produce two assignments, got %+v", decision.Actions)
	}
	prefill, decode := 0, 0
	for _, instance := range []domain.Instance{h.instance("container-1"), h.instance("container-2")} {
		switch instance.Role {
		case domain.RolePrefill:
			prefill++
		case domain.RoleDecode:
			decode++
		}
	}
	if prefill != 1 || decode != 1 {
		t.Fatalf("assignment split p=%d d=%d, want 1/1", prefill, decode)
	}
	if h.metrics.PointInTime(obs.MetricPDRatioTarget) != 0.5 {
		t.Fatalf("pd_ratio_target=%v, want 0.5", h.metrics.PointInTime(obs.MetricPDRatioTarget))
	}
}

func TestRevokeOfUnknownInstanceIsIdempotent(t *testing.T) {
	h := newHarness(t)
	outcome := h.handle(h.revokedEvent("never-seen", 60, h.clock.Now()))
	if !outcome.Accepted || outcome.Duplicate {
		t.Fatalf("revoking an unknown instance must be an idempotent success: %+v", outcome)
	}
	if outcome.Result != domain.ResultApplied {
		t.Fatalf("result=%s, want APPLIED", outcome.Result)
	}
	entries, err := h.store.ListAudit(context.Background(), "never-seen", 10)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the idempotent revoke must still be audited, got %d entries", len(entries))
	}
}

func TestStaleRevokeDoesNotOverrideNewerLease(t *testing.T) {
	h := newHarness(t)
	now := h.clock.Now()
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", now))

	// A lease observation newer than the revoke must win (§5).
	h.clock.Advance(2 * time.Minute)
	updated := domain.CapacityEvent{
		EventID:    "partner-a:update:container-1",
		PartnerID:  "partner-a",
		Type:       domain.EventCapacityUpdated,
		OccurredAt: h.clock.Now(),
		Instance:   &domain.EventInstance{ID: "container-1", Endpoint: h.bootstrap.Endpoint(), LeaseID: "lease-2"},
		Source:     domain.SourcePush,
		ReceivedAt: h.clock.Now(),
	}
	h.handle(updated)

	outcome := h.handle(h.revokedEvent("container-1", 60, now))
	if outcome.Result != domain.ResultRejected {
		t.Fatalf("a revoke older than the newest lease must be rejected, got %+v", outcome)
	}
	if instance := h.instance("container-1"); instance.InstanceState == domain.InstanceReleased {
		t.Fatal("a stale revoke must not release a container with a newer lease")
	}
	if !h.recorder.has(obs.AlertIllegalStateTransition) {
		t.Fatalf("a stale revoke must be alerted, got %v", h.recorder.names())
	}
}

func TestUnreachableContainerBecomesLost(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
	h.recorder.reset()

	// The container stops answering.
	h.bootstrap.SetDown(true)

	report := h.reconcile()
	if len(report.Lost) != 1 || report.Lost[0] != "container-1" {
		t.Fatalf("the unreachable container must be reported as LOST: %+v", report)
	}
	instance := h.instance("container-1")
	if instance.InstanceState != domain.InstanceLost {
		t.Fatalf("instance_state=%s, want LOST", instance.InstanceState)
	}
	if !h.recorder.has(obs.AlertInstanceLostRateHigh) {
		t.Fatalf("a LOST instance must be alerted, got %v", h.recorder.names())
	}
	if h.metrics.PointInTime(obs.MetricLostInstancesTotal, "reason", "unreachable") == 0 {
		t.Fatal("lost_instances_total must be counted")
	}
}

func TestPrepareFailureKeepsTheContainerAndReports(t *testing.T) {
	h := newHarness(t)
	// The container was reachable when the partner advertised it, then went away.
	h.bootstrap.SetDown(true)
	outcome := h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))
	if !outcome.Accepted {
		t.Fatalf("the event itself must be accepted even when preparation fails: %+v", outcome)
	}
	instance := h.instance("container-1")
	if instance.InstanceState != domain.InstancePreparing {
		t.Fatalf("instance_state=%s, want PREPARING after a retryable failure", instance.InstanceState)
	}
	if instance.ServiceState != domain.ServiceNone {
		t.Fatalf("service_state=%s, a failed preparation must not invent a service state", instance.ServiceState)
	}
	if instance.PrepareAttempts != 1 {
		t.Fatalf("prepare_attempts=%d, want 1", instance.PrepareAttempts)
	}
	if !h.recorder.has(obs.AlertBootstrapFailure) {
		t.Fatalf("a preparation failure must be alerted, got %v", h.recorder.names())
	}
	if h.metrics.SeriesCount(obs.MetricInstancePrepareSeconds, "outcome", "failed") == 0 {
		t.Fatal("instance_prepare_duration_seconds must observe the failed attempt")
	}
}

func TestEnvironmentMismatchIsDetectedBeforeStart(t *testing.T) {
	h := newHarness(t)
	// The partner offered a container built from an image the control plane was
	// never validated against.
	h.bootstrap.SetEnvironment(launcher.Environment{
		OS: "ubuntu-20.04", CUDA: "11.8", Python: "3.10", SGLang: "0.3.0",
	})
	h.handle(h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now()))

	instance := h.instance("container-1")
	if instance.InstanceState == domain.InstanceIdle {
		t.Fatal("a container that does not match the controlled image profile must not become IDLE")
	}
	if !h.recorder.has(obs.AlertEnvironmentMismatch) {
		t.Fatalf("an environment mismatch must be alerted, got %v", h.recorder.names())
	}
}

func assertAuditContains(t *testing.T, entries []store.AuditEntry, actions ...string) {
	t.Helper()
	seen := map[string]bool{}
	for _, entry := range entries {
		seen[entry.Action] = true
	}
	for _, action := range actions {
		if !seen[action] {
			t.Fatalf("audit is missing %q, got %v", action, seen)
		}
	}
}

// The bootstrap interface is the control plane's only way to touch a container,
// and partner platforms publish container ports to the internet. The fake
// container below refuses every request without the shared secret, so these two
// tests are what keeps the interface from silently becoming open.
func TestBootstrapCallsCarryTheSharedSecret(t *testing.T) {
	h := newHarness(t)
	h.handle(h.addedEvent("container-1", "http://10.0.0.1:8080", "lease-1", h.clock.Now()))

	instance := h.instance("container-1")
	if instance.InstanceState != domain.InstanceIdle {
		t.Fatalf("the instance must reach IDLE, got %s/%s",
			instance.InstanceState, instance.ServiceState)
	}
	if rejected := h.bootstrap.rejected(); rejected != 0 {
		t.Fatalf("the control plane made %d calls without the shared secret", rejected)
	}
}

func TestBootstrapClientRefusesToStartWithoutASecret(t *testing.T) {
	if _, err := launcher.New(time.Second, ""); err == nil {
		t.Fatal("launcher.New must refuse an empty shared secret")
	}
	if _, err := launcher.New(time.Second, "   "); err == nil {
		t.Fatal("launcher.New must refuse a blank shared secret")
	}
	if _, err := launcher.New(time.Second, testBootstrapToken); err != nil {
		t.Fatalf("launcher.New with a secret: %v", err)
	}
	if _, err := launcher.NewWithHTTPClient(http.DefaultClient, ""); err == nil {
		t.Fatal("launcher.NewWithHTTPClient must refuse an empty shared secret")
	}
}

// A partner platform publishes the bootstrap control interface and the SGLang
// service on different ports - 九章智算云 exposes container port 9001 as
// :30086 for the control interface and 9002 as :30093 for the service. The
// Router routes inference traffic, so it has to be handed the service address.
// Registering the bootstrap address sent every request to a process that
// answers 404, and it broke draining as well (see the test below).
func TestRouterRegistersTheServiceEndpointNotTheBootstrap(t *testing.T) {
	h := newHarness(t)
	const service = "http://10.9.9.9:9002"

	event := h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now())
	event.Instance.ServiceEndpoint = service
	h.handle(event)

	if err := h.ctrl.StartService(context.Background(), "container-1", domain.RolePrefill); err != nil {
		t.Fatalf("start service: %v", err)
	}

	if _, ok := h.router.workerByURL(service); !ok {
		t.Fatalf("the router must be given the service address %s", service)
	}
	if _, ok := h.router.workerByURL(h.bootstrap.Endpoint()); ok {
		t.Fatalf("the router must never be given the bootstrap address %s", h.bootstrap.Endpoint())
	}
	// The control interface is still the bootstrap: /bootstrap/start went there.
	if startCalls, _, _ := h.bootstrap.counts(); startCalls != 1 {
		t.Fatalf("bootstrap start calls=%d, want 1", startCalls)
	}
}

// Draining waits for in-flight requests to reach zero, and the wait only works
// when the address used to find the worker in /get_loads is the one it was
// registered with. Matching against the bootstrap endpoint made every worker
// look absent, which the control plane reads as "no traffic can arrive" - so a
// drain would finish immediately and stop a service that still had requests.
func TestDrainWaitsOnInFlightLoadReportedForTheServiceEndpoint(t *testing.T) {
	h := newHarness(t)
	const service = "http://10.9.9.9:9002"

	event := h.addedEvent("container-1", h.bootstrap.Endpoint(), "lease-1", h.clock.Now())
	event.Instance.ServiceEndpoint = service
	h.handle(event)
	if err := h.ctrl.StartService(context.Background(), "container-1", domain.RolePrefill); err != nil {
		t.Fatalf("start service: %v", err)
	}
	if !h.router.setLoad(service, 4) {
		t.Fatal("the router must know the service address it was given")
	}

	// A long grace period: the drain may only finish because traffic reached
	// zero, never because the deadline expired.
	outcome := h.handle(h.revokedEvent("container-1", 3600, h.clock.Now()))
	if outcome.Result != domain.ResultApplied {
		t.Fatalf("revoke result=%s reason=%q", outcome.Result, outcome.Reason)
	}

	instance := h.instance("container-1")
	if instance.ServiceState != domain.ServiceDraining {
		t.Fatalf("with 4 requests in flight the instance must stay DRAINING, got %s",
			instance.ServiceState)
	}
	if _, stopCalls, _ := h.bootstrap.counts(); stopCalls != 0 {
		t.Fatalf("bootstrap stop calls=%d, want 0 while requests are in flight", stopCalls)
	}
}
