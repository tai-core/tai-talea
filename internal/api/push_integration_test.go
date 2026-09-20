package api_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/api"
	"github.com/tai-core/tai-talea/internal/config"
	"github.com/tai-core/tai-talea/internal/controller"
	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/instancemanager"
	"github.com/tai-core/tai-talea/internal/launcher"
	"github.com/tai-core/tai-talea/internal/obs"
	"github.com/tai-core/tai-talea/internal/partners"
	"github.com/tai-core/tai-talea/internal/planner"
	"github.com/tai-core/tai-talea/internal/routeradapter"
	"github.com/tai-core/tai-talea/internal/store"
)

const (
	partnerToken  = "partner-a-push-token"
	partnerSecret = "partner-a-push-secret-0123456789"
	adminToken    = "admin-token-0123456789"
	modelID       = "model-a"
	// bootstrapTokenTest is the shared secret of the stub container.
	bootstrapTokenTest = "bootstrap-token-0123456789"
)

// stubBootstrap answers the four /bootstrap endpoints for one container. It
// demands the shared secret exactly like the real bootstrap does, so the whole
// Push pipeline below only completes when the control plane sends it.
func stubBootstrap(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	env := launcher.Environment{
		OS: "ubuntu-22.04", CUDA: "12.4", Python: "3.11", SGLang: "0.4.6",
		Wheelhouse: "/opt/tai-talea/wheelhouse", Virtualenv: "/opt/tai-talea/venv",
	}
	mux.HandleFunc(launcher.PathHealth, func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, map[string]any{"status": "ok", "phase": launcher.PhaseIdle, "version": "tai-talea-bootstrap/1"})
	})
	mux.HandleFunc(launcher.PathStatus, func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, map[string]any{"phase": launcher.PhaseIdle, "environment": env})
	})
	mux.HandleFunc(launcher.PathStart, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc(launcher.PathStop, func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, map[string]any{"phase": launcher.PhaseStopped, "exit_code": 0, "timed_out": false})
	})
	server := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			if request.Header.Get(launcher.HeaderToken) != bootstrapTokenTest {
				writer.WriteHeader(http.StatusUnauthorized)
				return
			}
			mux.ServeHTTP(writer, request)
		}))
	t.Cleanup(server.Close)
	return server
}

type stubRouter struct {
	mu         sync.Mutex
	workers    map[string]string
	readiness  map[string]string
	generation uint64
	server     *httptest.Server
}

func newStubRouter(t *testing.T) *stubRouter {
	t.Helper()
	stub := &stubRouter{workers: map[string]string{}, readiness: map[string]string{}, generation: 1}
	mux := http.NewServeMux()
	mux.HandleFunc("/workers", func(writer http.ResponseWriter, request *http.Request) {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		if request.Method == http.MethodGet {
			list := []map[string]any{}
			for id, url := range stub.workers {
				list = append(list, map[string]any{"id": id, "url": url, "is_healthy": true, "load": 0})
			}
			writeJSON(writer, map[string]any{"workers": list})
			return
		}
		var payload map[string]string
		_ = json.NewDecoder(request.Body).Decode(&payload)
		id := "w-" + strconv.Itoa(len(stub.workers)+1)
		stub.workers[id] = payload["url"]
		stub.readiness[id] = "ready"
		writeJSONStatus(writer, http.StatusAccepted, map[string]any{"worker_id": id, "url": payload["url"]})
	})
	mux.HandleFunc("/state/workers/", func(writer http.ResponseWriter, request *http.Request) {
		id := strings.TrimPrefix(request.URL.Path, "/state/workers/")
		stub.mu.Lock()
		defer stub.mu.Unlock()
		url, ok := stub.workers[id]
		if !ok {
			writeJSONStatus(writer, http.StatusNotFound, map[string]any{"error": "worker_not_found"})
			return
		}
		if request.Method == http.MethodPut {
			var payload map[string]string
			_ = json.NewDecoder(request.Body).Decode(&payload)
			stub.generation++
			stub.readiness[id] = payload["state"]
		}
		record := map[string]any{
			"worker_id": id, "worker_url": url, "state": stub.readiness[id], "generation": stub.generation,
		}
		if stub.readiness[id] != "ready" {
			record["unavailable_reason"] = "manual"
		}
		writeJSON(writer, record)
	})
	mux.HandleFunc("/get_loads", func(writer http.ResponseWriter, _ *http.Request) {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		loads := []map[string]any{}
		for id, url := range stub.workers {
			loads = append(loads, map[string]any{"worker": url, "load": 0})
			_ = id
		}
		writeJSON(writer, map[string]any{"loads": loads})
	})
	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

func writeJSON(writer http.ResponseWriter, payload any) {
	writeJSONStatus(writer, http.StatusOK, payload)
}

func writeJSONStatus(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}

// newServer wires a complete control plane over real HTTP with stub containers.
func newServer(t *testing.T, configure ...func(*api.Options)) (*httptest.Server, *store.SQLiteStore, string) {
	t.Helper()
	container := stubBootstrap(t)
	router := newStubRouter(t)

	cfg := config.Default()
	cfg.Server.AdminToken = adminToken
	cfg.Storage.SQLitePath = filepath.Join(t.TempDir(), "tai-talea.db")
	cfg.Router.Name = "primary"
	cfg.Router.URL = router.server.URL
	cfg.Router.Mode = routeradapter.ModeReadinessV1
	cfg.Controller.ModelID = modelID
	cfg.Controller.BootstrapToken = bootstrapTokenTest
	cfg.ImageProfile = config.ImageProfile{
		OS: "ubuntu-22.04", CUDA: "12.4", Python: "3.11", SGLang: "0.4.6",
		Wheelhouse: "/opt/tai-talea/wheelhouse", Virtualenv: "/opt/tai-talea/venv",
	}
	cfg.Controller.CallTimeout = config.Duration(2 * time.Second)
	cfg.Controller.ReconcileInterval = config.Duration(50 * time.Millisecond)
	cfg.Partners = []config.PartnerConfig{{
		ID: "partner-a", Adapter: "static", PushToken: partnerToken, PushSecret: partnerSecret,
		HMACTolerance: config.Duration(5 * time.Minute),
		Instances: []config.StaticInstanceConfig{
			{ID: "container-1", Endpoint: container.URL, LeaseID: "lease-1"},
		},
	}}

	persistence, err := store.OpenSQLite(cfg.Storage.SQLitePath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { persistence.Close() })

	registry, err := partners.Build(cfg)
	if err != nil {
		t.Fatalf("partner registry: %v", err)
	}
	routerClient, err := routeradapter.New(cfg.Router.ToAdapter())
	if err != nil {
		t.Fatalf("router adapter: %v", err)
	}
	launcherClient, err := launcher.New(
		cfg.Controller.CallTimeout.Duration(), cfg.Controller.BootstrapToken)
	if err != nil {
		t.Fatalf("launcher: %v", err)
	}
	manager, err := instancemanager.New(launcherClient, instancemanager.Profile{
		OS: cfg.ImageProfile.OS, CUDA: cfg.ImageProfile.CUDA, Python: cfg.ImageProfile.Python,
		SGLang: cfg.ImageProfile.SGLang, Wheelhouse: cfg.ImageProfile.Wheelhouse,
		Virtualenv: cfg.ImageProfile.Virtualenv, BootstrapVersion: "tai-talea-bootstrap/1",
		ModelID: modelID, MaxLeaseAge: cfg.Controller.LeaseWarning.Duration(),
	})
	if err != nil {
		t.Fatalf("instance manager: %v", err)
	}
	plannerInstance, err := planner.New(cfg.Planner.ToPlanner())
	if err != nil {
		t.Fatalf("planner: %v", err)
	}
	metrics := obs.NewRegistry()
	ctrl, err := controller.New(controller.Deps{
		Config: cfg, Store: persistence, Router: routerClient, Launcher: launcherClient,
		Manager: manager, Partners: registry, Planner: plannerInstance, Metrics: metrics,
		Alerter: obs.NewAlerter(metrics, nil, 0),
	})
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	options := api.Options{
		Config: cfg, Store: persistence, Controller: ctrl, Metrics: metrics,
		Ready: func() bool { return true },
	}
	for _, apply := range configure {
		apply(&options)
	}
	apiServer, err := api.New(options)
	if err != nil {
		t.Fatalf("api server: %v", err)
	}
	server := httptest.NewServer(apiServer.Handler())
	t.Cleanup(server.Close)
	// The stub container is the endpoint the partner advertises, so the whole
	// ADDED -> IDLE pipeline runs for real.
	return server, persistence, container.URL
}

func sign(t *testing.T, timestamp string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(partnerSecret))
	mac.Write(append(append([]byte(timestamp), '.'), body...))
	return hex.EncodeToString(mac.Sum(nil))
}

func push(t *testing.T, base string, body []byte) (int, map[string]any) {
	t.Helper()
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	request, err := http.NewRequest(http.MethodPost, base+"/v1/capacity/events", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+partnerToken)
	request.Header.Set(api.HeaderTimestamp, timestamp)
	request.Header.Set(api.HeaderSignature, sign(t, timestamp, body))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode response %s: %v", payload, err)
	}
	return response.StatusCode, decoded
}

func adminGet(t *testing.T, base, path string) map[string]any {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+adminToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return decoded
}

func addedEventBody(t *testing.T, eventID, endpoint string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"event_id":    eventID,
		"partner_id":  "partner-a",
		"type":        "CAPACITY_ADDED",
		"occurred_at": time.Now().UTC().Format(time.RFC3339),
		"instance": map[string]any{
			"id": "container-1", "endpoint": endpoint, "lease_id": "lease-1",
			"spec": map[string]any{"gpu": "H100", "gpu_count": 1, "model_support": []string{modelID}},
		},
	})
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	return body
}

// TestPushAPIIsIdempotentOverHTTP is the boundary level proof of the M1
// acceptance criteria that concern the partner contract: a redelivery must be
// accepted idempotently and must not run the lifecycle twice.
func TestPushAPIIsIdempotentOverHTTP(t *testing.T) {
	server, persistence, endpoint := newServer(t)
	body := addedEventBody(t, "partner-a:evt-1", endpoint)

	status, payload := push(t, server.URL, body)
	if status != http.StatusAccepted {
		t.Fatalf("first push status=%d payload=%v", status, payload)
	}
	if payload["accepted"] != true || payload["duplicate"] != false {
		t.Fatalf("unexpected first outcome: %v", payload)
	}
	if payload["result"] != string(domain.ResultApplied) {
		t.Fatalf("result=%v, want APPLIED", payload["result"])
	}

	instance, err := persistence.GetInstance(context.Background(), "container-1")
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if instance.InstanceState != domain.InstanceIdle || instance.ServiceState != domain.ServiceNone {
		t.Fatalf("the container must reach IDLE/NONE, got %s/%s",
			instance.InstanceState, instance.ServiceState)
	}
	attempts := instance.PrepareAttempts

	status, payload = push(t, server.URL, body)
	if status != http.StatusOK {
		t.Fatalf("a redelivery must answer 200, got %d payload=%v", status, payload)
	}
	if payload["accepted"] != true || payload["duplicate"] != true {
		t.Fatalf("a redelivery must be an idempotent success: %v", payload)
	}
	if !strings.Contains(payload["reason"].(string), "not repeated") {
		t.Fatalf("the reason must state that nothing was repeated: %v", payload["reason"])
	}

	after, err := persistence.GetInstance(context.Background(), "container-1")
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if after.PrepareAttempts != attempts {
		t.Fatalf("a redelivery must not probe the container again: attempts %d -> %d",
			attempts, after.PrepareAttempts)
	}

	entries, err := persistence.ListAudit(context.Background(), "container-1", 50)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	prepared := 0
	for _, entry := range entries {
		if entry.Action == "capacity_added" {
			prepared++
		}
	}
	if prepared != 1 {
		t.Fatalf("the takeover must be audited exactly once, got %d", prepared)
	}
}

func TestPushAPIRejectsBadCredentialsAndBodies(t *testing.T) {
	server, _, endpoint := newServer(t)
	body := addedEventBody(t, "partner-a:evt-2", endpoint)

	// Wrong secret.
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/capacity/events", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+partnerToken)
	request.Header.Set(api.HeaderTimestamp, timestamp)
	request.Header.Set(api.HeaderSignature, "00")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a bad signature must answer 401, got %d", response.StatusCode)
	}

	// Stale timestamp.
	stale := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	request, err = http.NewRequest(http.MethodPost, server.URL+"/v1/capacity/events", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+partnerToken)
	request.Header.Set(api.HeaderTimestamp, stale)
	request.Header.Set(api.HeaderSignature, sign(t, stale, body))
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("a stale timestamp must answer 400, got %d", response.StatusCode)
	}

	// Trailing second JSON document.
	doubled := append(append([]byte{}, body...), []byte(`{"event_id":"extra"}`)...)
	if status, _ := push(t, server.URL, doubled); status != http.StatusBadRequest {
		t.Fatalf("a body with two json documents must answer 400, got %d", status)
	}

	// Unknown event type.
	badType, err := json.Marshal(map[string]any{
		"event_id": "partner-a:evt-3", "partner_id": "partner-a",
		"type": "CAPACITY_EXPLODED", "occurred_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if status, _ := push(t, server.URL, badType); status != http.StatusUnprocessableEntity {
		t.Fatalf("an unknown event type must answer 422, got %d", status)
	}
}

func TestAdminAPIRefusesUnprivilegedCallers(t *testing.T) {
	server, _, _ := newServer(t)
	response, err := http.Get(server.URL + "/v1/capacity/instances")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the admin API must answer 401 without a token, got %d", response.StatusCode)
	}

	payload := adminGet(t, server.URL, "/v1/capacity/instances")
	if payload["count"].(float64) != 0 {
		t.Fatalf("no instance is tracked yet: %v", payload)
	}

	plannerPayload := adminGet(t, server.URL, "/v1/capacity/planner")
	if plannerPayload["gear"] != "1:1" {
		t.Fatalf("gear=%v, want 1:1", plannerPayload["gear"])
	}
}
