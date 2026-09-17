package routeradapter_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/config"
	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/routeradapter"
)

// routerStub mimics the sglang-router control plane surface documented in
// sglang-router/README.md.
type routerStub struct {
	workers    map[string]string // worker id -> url
	types      map[string]string
	readiness  map[string]string
	generation uint64
	deletes    int
	conflict   bool
	server     *httptest.Server
}

func newRouterStub() *routerStub {
	stub := &routerStub{
		workers: map[string]string{}, types: map[string]string{},
		readiness: map[string]string{}, generation: 1,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/workers", stub.workersHandler)
	mux.HandleFunc("/workers/", stub.worker)
	mux.HandleFunc("/state/workers", stub.readinessList)
	mux.HandleFunc("/state/workers/", stub.readinessItem)
	mux.HandleFunc("/get_loads", stub.loads)
	stub.server = httptest.NewServer(mux)
	return stub
}

func (s *routerStub) Close() { s.server.Close() }

func (s *routerStub) clients(t *testing.T) *routeradapter.Client {
	t.Helper()
	client, err := routeradapter.New(routeradapter.Config{
		Name: "primary", URL: s.server.URL, Mode: routeradapter.ModeReadinessV1, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("router adapter: %v", err)
	}
	return client
}

func (s *routerStub) workersHandler(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodPost:
		if s.conflict {
			writeStubJSON(writer, http.StatusConflict, map[string]string{"error": "worker_exists", "message": "already exists"})
			return
		}
		var payload map[string]string
		_ = json.NewDecoder(request.Body).Decode(&payload)
		id := "w-" + payload["url"]
		s.workers[id] = payload["url"]
		s.types[id] = payload["worker_type"]
		s.readiness[id] = "ready"
		writeStubJSON(writer, http.StatusAccepted, map[string]string{"worker_id": id, "url": payload["url"]})
	case http.MethodGet:
		list := make([]map[string]any, 0, len(s.workers))
		for id, url := range s.workers {
			list = append(list, map[string]any{
				"id": id, "url": url, "worker_type": s.types[id], "is_healthy": true, "load": 0,
			})
		}
		writeStubJSON(writer, http.StatusOK, map[string]any{"workers": list})
	default:
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *routerStub) worker(writer http.ResponseWriter, request *http.Request) {
	id := strings.TrimPrefix(request.URL.Path, "/workers/")
	switch request.Method {
	case http.MethodDelete:
		s.deletes++
		writeStubJSON(writer, http.StatusAccepted, map[string]string{"worker_id": id})
	case http.MethodGet:
		url, ok := s.workers[id]
		if !ok {
			writeStubJSON(writer, http.StatusNotFound, map[string]string{"error": "worker_not_found"})
			return
		}
		writeStubJSON(writer, http.StatusOK, map[string]any{
			"id": id, "url": url, "worker_type": s.types[id], "is_healthy": true, "load": 0,
		})
	default:
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *routerStub) readinessList(writer http.ResponseWriter, _ *http.Request) {
	list := make([]map[string]any, 0, len(s.workers))
	for id := range s.workers {
		list = append(list, s.record(id))
	}
	writeStubJSON(writer, http.StatusOK, list)
}

func (s *routerStub) readinessItem(writer http.ResponseWriter, request *http.Request) {
	id := strings.TrimPrefix(request.URL.Path, "/state/workers/")
	if _, ok := s.workers[id]; !ok {
		writeStubJSON(writer, http.StatusNotFound, map[string]string{"error": "worker_not_found"})
		return
	}
	switch request.Method {
	case http.MethodGet:
		writeStubJSON(writer, http.StatusOK, s.record(id))
	case http.MethodPut:
		var payload map[string]string
		_ = json.NewDecoder(request.Body).Decode(&payload)
		previous := s.readiness[id]
		s.generation++
		s.readiness[id] = payload["state"]
		writeStubJSON(writer, http.StatusOK, map[string]any{
			"previous_state": previous, "record": s.record(id), "changed": previous != payload["state"],
		})
	default:
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *routerStub) loads(writer http.ResponseWriter, _ *http.Request) {
	loads := make([]map[string]any, 0, len(s.workers))
	for id, url := range s.workers {
		loads = append(loads, map[string]any{"worker": url, "worker_type": s.types[id], "load": 3})
	}
	writeStubJSON(writer, http.StatusOK, map[string]any{"loads": loads, "total_workers": len(s.workers)})
}

func (s *routerStub) record(id string) map[string]any {
	record := map[string]any{
		"worker_id": id, "worker_url": s.workers[id], "state": s.readiness[id], "generation": s.generation,
	}
	if s.readiness[id] != "ready" {
		record["unavailable_reason"] = "manual"
	}
	return record
}

func writeStubJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}

func TestAdapterRegistersReadsAndDrainsThroughReadiness(t *testing.T) {
	stub := newRouterStub()
	defer stub.Close()
	client := stub.clients(t)
	ctx := context.Background()

	registration, err := client.RegisterWorker(ctx, routeradapter.RegisterRequest{
		WorkerURL: "http://prefill-a:31000", WorkerType: domain.RolePrefill, ModelID: "model-a",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if registration.WorkerID == "" || registration.WorkerURL != "http://prefill-a:31000" {
		t.Fatalf("unexpected registration: %+v", registration)
	}

	worker, err := client.GetWorker(ctx, registration.WorkerID)
	if err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if worker.URL != registration.WorkerURL {
		t.Fatalf("worker url=%s, want %s", worker.URL, registration.WorkerURL)
	}

	record, err := client.GetReadiness(ctx, registration.WorkerID)
	if err != nil {
		t.Fatalf("get readiness: %v", err)
	}
	if !record.Ready() || record.Generation == 0 {
		t.Fatalf("a fresh worker must be routable: %+v", record)
	}

	transition, err := client.SetReadiness(ctx, registration.WorkerID, false)
	if err != nil {
		t.Fatalf("close readiness: %v", err)
	}
	if transition.Record.Ready() {
		t.Fatalf("readiness must be unavailable after the drain: %+v", transition.Record)
	}
	if transition.Record.Generation <= record.Generation {
		t.Fatal("a readiness change must advance the generation")
	}

	loads, err := client.GetLoads(ctx)
	if err != nil {
		t.Fatalf("get loads: %v", err)
	}
	if len(loads) != 1 || loads[0].Load != 3 {
		t.Fatalf("unexpected loads: %+v", loads)
	}

	if stub.deletes != 0 {
		t.Fatal("registering, reading and draining must never issue DELETE /workers")
	}
}

func TestAdapterRecoversWhenTheRouterAlreadyKnowsTheURL(t *testing.T) {
	stub := newRouterStub()
	defer stub.Close()
	client := stub.clients(t)
	ctx := context.Background()

	first, err := client.RegisterWorker(ctx, routeradapter.RegisterRequest{
		WorkerURL: "http://decode-a:32000", WorkerType: domain.RoleDecode, ModelID: "model-a",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	stub.conflict = true
	_, err = client.RegisterWorker(ctx, routeradapter.RegisterRequest{
		WorkerURL: "http://decode-a:32000", WorkerType: domain.RoleDecode, ModelID: "model-a",
	})
	if !errors.Is(err, routeradapter.ErrAlreadyRegistered) {
		t.Fatalf("a repeated url must surface ErrAlreadyRegistered, got %v", err)
	}
	existing, found, err := client.FindWorkerByURL(ctx, "http://decode-a:32000")
	if err != nil || !found {
		t.Fatalf("lookup: found=%v err=%v", found, err)
	}
	if existing.ID != first.WorkerID {
		t.Fatalf("worker id=%s, want %s", existing.ID, first.WorkerID)
	}
}

func TestAdapterRejectsUnknownWorkers(t *testing.T) {
	stub := newRouterStub()
	defer stub.Close()
	client := stub.clients(t)
	ctx := context.Background()

	if _, err := client.GetWorker(ctx, "missing"); !errors.Is(err, routeradapter.ErrWorkerNotFound) {
		t.Fatalf("unknown worker must surface ErrWorkerNotFound, got %v", err)
	}
	if _, err := client.GetReadiness(ctx, "missing"); !errors.Is(err, routeradapter.ErrReadinessNotVisible) {
		t.Fatalf("unknown readiness must surface ErrReadinessNotVisible, got %v", err)
	}
}

func TestAdapterConfigValidation(t *testing.T) {
	cases := map[string]routeradapter.Config{
		"missing name": {URL: "http://127.0.0.1:30001", Mode: routeradapter.ModeReadinessV1},
		"bad scheme":   {Name: "primary", URL: "ftp://127.0.0.1:30001", Mode: routeradapter.ModeReadinessV1},
		"with path":    {Name: "primary", URL: "http://127.0.0.1:30001/api", Mode: routeradapter.ModeReadinessV1},
		"bad mode":     {Name: "primary", URL: "http://127.0.0.1:30001", Mode: "observe"},
		"slow timeout": {Name: "primary", URL: "http://127.0.0.1:30001", Mode: routeradapter.ModeReadinessV1, Timeout: time.Minute},
	}
	for name, cfg := range cases {
		if _, err := routeradapter.New(cfg); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}

	// The default mode is the only supported control mode.
	if (routeradapter.Config{}).EffectiveMode() != routeradapter.ModeReadinessV1 {
		t.Fatal("the default router mode must be readiness-v1")
	}
}

func TestAdapterRejectsInvalidRegistrationRequests(t *testing.T) {
	stub := newRouterStub()
	defer stub.Close()
	client := stub.clients(t)
	ctx := context.Background()

	cases := map[string]routeradapter.RegisterRequest{
		"missing role":  {WorkerURL: "http://a:1", ModelID: "model-a"},
		"plain url":     {WorkerURL: "a:1", WorkerType: domain.RolePrefill, ModelID: "model-a"},
		"missing model": {WorkerURL: "http://a:1", WorkerType: domain.RolePrefill},
	}
	for name, request := range cases {
		if _, err := client.RegisterWorker(ctx, request); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
}

// The config package must be able to feed the adapter without losing the mode.
func TestRouterConfigFeedsTheAdapter(t *testing.T) {
	cfg := config.Default()
	cfg.Router.URL = "http://127.0.0.1:30001"
	cfg.Router.Name = "primary"
	cfg.Router.Mode = routeradapter.ModeReadinessV1
	cfg.Router.Timeout = config.Duration(3 * time.Second)
	if err := cfg.Router.ToAdapter().Validate(); err != nil {
		t.Fatalf("router config must validate: %v", err)
	}
}
