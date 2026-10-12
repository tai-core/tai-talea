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
	bootstrap  map[string]int // worker id -> bootstrap port (0 = none sent)
	readiness  map[string]string
	generation uint64
	deletes    int
	conflict   bool
	server     *httptest.Server

	// load is the in-flight request count /get_loads reports for every worker.
	load int64
	// gets counts GET /workers/{id}, so a test can observe the poll cadence.
	gets int
	// removalDelay is how many reads of a deleted membership still find it.
	// The Router drops a membership through its own workflow, so the delete is
	// acknowledged long before the entry disappears; a negative value models a
	// Router whose removal never lands at all.
	removalDelay int
	// pendingDrop holds the countdown per deleted worker id.
	pendingDrop map[string]int
}

func newRouterStub() *routerStub {
	stub := &routerStub{
		workers: map[string]string{}, types: map[string]string{},
		bootstrap: map[string]int{}, readiness: map[string]string{}, generation: 1,
		load: 3, removalDelay: 1, pendingDrop: map[string]int{},
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
	return s.clientWithTimeout(t, 5*time.Second)
}

func (s *routerStub) clientWithTimeout(t *testing.T, timeout time.Duration) *routeradapter.Client {
	t.Helper()
	client, err := routeradapter.New(routeradapter.Config{
		Name: "primary", URL: s.server.URL, Mode: routeradapter.ModeReadinessV1, Timeout: timeout,
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
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			writeStubJSON(writer, http.StatusBadRequest, map[string]string{"error": "bad_json"})
			return
		}
		url, _ := payload["url"].(string)
		workerType, _ := payload["worker_type"].(string)
		port, _ := payload["bootstrap_port"].(float64)
		id := "w-" + url
		s.workers[id] = url
		s.types[id] = workerType
		s.bootstrap[id] = int(port)
		s.readiness[id] = "ready"
		writeStubJSON(writer, http.StatusAccepted, map[string]string{"worker_id": id, "url": url})
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
		s.pendingDrop[id] = s.removalDelay
		writeStubJSON(writer, http.StatusAccepted, map[string]string{"worker_id": id})
	case http.MethodGet:
		s.gets++
		if remaining, waiting := s.pendingDrop[id]; waiting {
			switch {
			case remaining < 0:
				// The workflow never drops the membership.
			case remaining <= 1:
				delete(s.pendingDrop, id)
				delete(s.workers, id)
				delete(s.types, id)
				delete(s.bootstrap, id)
				delete(s.readiness, id)
			default:
				s.pendingDrop[id] = remaining - 1
			}
		}
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
		loads = append(loads, map[string]any{"worker": url, "worker_type": s.types[id], "load": s.load})
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
		BootstrapPort: 8998,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// PD routing needs the prefill's bootstrap port: the Router injects it into
	// every routed request so the decode side knows where to pull KV from.
	if got := stub.bootstrap["w-http://prefill-a:31000"]; got != 8998 {
		t.Fatalf("bootstrap_port=%d, want 8998", got)
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

func TestLoadsWireFormats(t *testing.T) {
	for _, body := range []string{`{"workers":[{"worker":"http://p:1","load":7}]}`, `{"loads":[{"worker":"http://p:1","load":7}]}`, `{"workers":[]}`, `{}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			client, err := routeradapter.New(routeradapter.Config{Name: "test", URL: server.URL, Mode: routeradapter.ModeReadinessV1})
			if err != nil {
				t.Fatal(err)
			}
			loads, err := client.GetLoads(t.Context())
			if body == `{}` {
				if err == nil {
					t.Fatal("unknown envelope accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(body, `"load":7`) && (len(loads) != 1 || loads[0].Load != 7) {
				t.Fatal(loads)
			}
		})
	}
}

// A stale role cannot be replaced in place: the Router has no update path for a
// worker's type, so recovery is delete-then-add. The Router performs the delete
// through its own workflow, which means the membership is still listed when the
// delete is acknowledged, and the two tests below pin both halves of that: the
// wait must outlast the workflow without hammering the endpoint, and when the
// removal never lands the error must say so instead of reporting the transport
// deadline it happened to trip over.
func TestRemoveStaleMembershipWaitsWithoutHammeringTheRouter(t *testing.T) {
	stub := newRouterStub()
	defer stub.Close()
	stub.load = 0
	stub.removalDelay = 3 // the workflow needs two more reads to drop the entry
	client := stub.clients(t)
	// The budget lives on the context, not on the client: Config.Timeout bounds
	// one request, and only a deadline on ctx bounds the wait for a membership.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	registration, err := client.RegisterWorker(ctx, routeradapter.RegisterRequest{
		WorkerURL: "http://decode-a:32000", WorkerType: domain.RoleDecode, ModelID: "model-a",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := client.SetReadiness(ctx, registration.WorkerID, false); err != nil {
		t.Fatalf("close readiness: %v", err)
	}

	before := stub.gets
	if err := client.RemoveStaleMembership(ctx, registration.WorkerID,
		"http://decode-a:32000", domain.RolePrefill); err != nil {
		t.Fatalf("a replacement that lands must succeed: %v", err)
	}
	polls := stub.gets - before
	if stub.deletes != 1 {
		t.Fatalf("deletes=%d, want exactly one", stub.deletes)
	}
	// One type check, one read per remaining removalDelay, one confirming read.
	// A fixed 100ms cadence over the same window would spend thirty.
	if polls > 8 {
		t.Fatalf("replacement polled %d times; the cadence must back off", polls)
	}
	if _, found, err := client.FindWorkerByURL(ctx, "http://decode-a:32000"); err != nil || found {
		t.Fatalf("membership must be gone: found=%v err=%v", found, err)
	}
}

func TestRemoveStaleMembershipNamesAMembershipThatNeverDrops(t *testing.T) {
	stub := newRouterStub()
	defer stub.Close()
	stub.load = 0
	stub.removalDelay = -1 // accepted, then listed forever
	client := stub.clients(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	registration, err := client.RegisterWorker(ctx, routeradapter.RegisterRequest{
		WorkerURL: "http://decode-a:32000", WorkerType: domain.RoleDecode, ModelID: "model-a",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := client.SetReadiness(ctx, registration.WorkerID, false); err != nil {
		t.Fatalf("close readiness: %v", err)
	}

	before := stub.gets
	started := time.Now()
	err = client.RemoveStaleMembership(ctx, registration.WorkerID,
		"http://decode-a:32000", domain.RolePrefill)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a membership the Router never drops must not report success")
	}
	if elapsed < time.Second {
		t.Fatalf("gave up after %s; the Router's workflow needs the whole budget", elapsed)
	}
	// The diagnostic, not the transport. "Get http://.../workers/w-...: context
	// deadline exceeded" is what this used to say, and it sent an operator
	// looking for a slow endpoint instead of a membership that would not move.
	message := err.Error()
	if !strings.Contains(message, "was not dropped") ||
		!strings.Contains(message, `as "decode"`) ||
		!strings.Contains(message, "http://decode-a:32000") {
		t.Fatalf("error must name the endpoint and the stale role: %s", message)
	}
	if polls := stub.gets - before; polls > 8 {
		t.Fatalf("gave up after %d polls; the cadence must back off", polls)
	}
	if stub.deletes != 1 {
		t.Fatalf("deletes=%d, want exactly one", stub.deletes)
	}
}
