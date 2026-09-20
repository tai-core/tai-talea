package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/routeradapter"
)

type fakeInventory []domain.Instance

func (f fakeInventory) ListInstances(context.Context) ([]domain.Instance, error) { return f, nil }

type fakeRouter []routeradapter.WorkerLoad

func (f fakeRouter) GetLoads(context.Context) ([]routeradapter.WorkerLoad, error) { return f, nil }

type gatedRouter struct {
	fakeRouter
	worker routeradapter.Worker
	ready  routeradapter.ReadinessRecord
}

func (f *gatedRouter) GetWorker(context.Context, string) (routeradapter.Worker, error) {
	return f.worker, nil
}
func (f *gatedRouter) GetReadiness(context.Context, string) (routeradapter.ReadinessRecord, error) {
	return f.ready, nil
}

func TestRouterGatesDoNotReportHealthyBalancedCapacity(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/server_info" {
			fmt.Fprint(w, `{"disaggregation_mode":"prefill","tp_size":1,"dp_size":1,"internal_states":[{"effective_max_running_requests_per_dp":8}]}`)
			return
		}
		fmt.Fprintln(w, "sglang:num_running_reqs 0\nsglang:num_queue_reqs 0\nsglang:num_prefill_prealloc_queue_reqs 0\nsglang:num_prefill_inflight_queue_reqs 0")
	}))
	defer worker.Close()
	router := &gatedRouter{worker: routeradapter.Worker{URL: worker.URL, WorkerType: "prefill", Healthy: true}, ready: routeradapter.ReadinessRecord{WorkerURL: worker.URL, State: "ready"}}
	c := New(fakeInventory{{ID: "p", ServiceEndpoint: worker.URL, Role: domain.RolePrefill, ServiceState: domain.ServiceServing}}, router)
	c.Sample(t.Context())
	if !c.Snapshot("").Nodes[0].Available {
		t.Fatal("healthy worker unavailable")
	}
	for _, gate := range []string{"draining", "unhealthy", "unavailable"} {
		t.Run(gate, func(t *testing.T) {
			router.worker.Healthy = gate != "unhealthy"
			router.worker.Metadata = map[string]string{}
			router.ready.State = "ready"
			if gate == "draining" {
				router.worker.Metadata["__pd_state"] = "draining"
			}
			if gate == "unavailable" {
				router.ready.State = "unavailable"
			}
			c.Sample(t.Context())
			s := c.Snapshot("")
			if s.Nodes[0].Available || s.Prefill.Pressure != nil {
				t.Fatal("unroutable worker counted as healthy idle capacity")
			}
		})
	}
}

func TestMetricTotalsAndReset(t *testing.T) {
	data := `# totals and priority breakdown
sglang:num_queue_reqs{model_name="x y",priority="",dp_rank="0"} 2
sglang:num_queue_reqs{model_name="x y",priority="1",dp_rank="0"} 2
sglang:num_queue_reqs{model_name="x y",priority="",dp_rank="1"} 3
sglang:token_usage{dp_rank="0"} .2
sglang:token_usage{dp_rank="1"} .7
sglang:prompt_tokens_total 200
`
	m, err := parseMetrics(strings.NewReader(data))
	if err != nil || m["num_queue_reqs"] != 5 || m["token_usage"] != .7 {
		t.Fatalf("%v %v", m, err)
	}
	if r := delta(m, map[string]float64{"prompt_tokens_total": 100}, "prompt_tokens_total", 2); r == nil || *r != 50 {
		t.Fatal(r)
	}
	for _, before := range []map[string]float64{{}, {"prompt_tokens_total": 201}} {
		if delta(m, before, "prompt_tokens_total", 2) != nil {
			t.Fatal("missing/reset counter must be unknown")
		}
	}
	if delta(m, m, "prompt_tokens_total", 21) != nil {
		t.Fatal("stale delta must be unknown")
	}
}
func TestCollectionRoleMismatchAndMissingMetrics(t *testing.T) {
	role := "prefill"
	failed := false
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failed {
			http.Error(w, "unavailable", 503)
			return
		}
		if r.URL.Path == "/server_info" {
			fmt.Fprintf(w, `{"disaggregation_mode":%q,"tp_size":1,"dp_size":1,"internal_states":[{"effective_max_running_requests_per_dp":8}]}`, role)
			return
		}
		fmt.Fprintln(w, "sglang:num_running_reqs 2\nsglang:num_queue_reqs 1\nsglang:num_prefill_prealloc_queue_reqs 1\nsglang:num_prefill_inflight_queue_reqs 0\nsglang:token_usage .2\nsglang:prompt_tokens_total 100")
	}))
	defer worker.Close()
	c := New(fakeInventory{{ID: "p", PartnerID: "owner", ServiceEndpoint: worker.URL, Role: domain.RolePrefill, ServiceState: domain.ServiceServing}}, fakeRouter{{Worker: worker.URL, Load: 3}})
	c.Sample(t.Context())
	s := c.Snapshot("owner")
	n := s.Nodes[0]
	if !n.Available || *n.Pressure != .5 || *n.RouterLoad != 3 || n.InputRate != nil {
		t.Fatalf("%+v", n)
	}
	c.Sample(t.Context())
	if r := c.Snapshot("").Nodes[0].InputRate; r == nil || *r != 0 {
		t.Fatal("idle valid counters should produce zero", r)
	}
	if hidden := c.Snapshot("other"); len(hidden.Nodes) != 0 || hidden.Prefill.Nodes != 0 || hidden.History[0].Prefill.Nodes != 0 {
		t.Fatal("cross-partner leak")
	}
	role = "decode"
	c.Sample(t.Context())
	if c.Snapshot("").Nodes[0].Available {
		t.Fatal("mismatched roles accepted")
	}
	failed = true
	c.Sample(t.Context())
	if s = c.Snapshot(""); s.Prefill.Pressure != nil || s.Nodes[0].Available {
		t.Fatal("failure reported as zero or old data")
	}
}
func TestStaleAndPartialAggregates(t *testing.T) {
	at := time.Now().UTC()
	n := Node{ID: "p", Role: domain.RolePrefill, Available: true, SampledAt: at, Running: number(2), Queue: number(1), Prealloc: number(0), Transfer: number(0), MaxRunning: number(8), InputRate: number(20)}
	if r := aggregate([]Node{n}, domain.RolePrefill, at.Add(20*time.Second)); r.Pressure != nil || r.TokenRate != nil {
		t.Fatal("stale samples accepted")
	}
	bad := n
	bad.Available = false
	if r := aggregate([]Node{n, bad}, domain.RolePrefill, at); r.Pressure != nil || r.Valid != 1 {
		t.Fatal("partial sum disguised as total")
	}
	c := New(nil, nil)
	c.frames = []frame{{at: at.Add(-time.Minute), nodes: []Node{n}}}
	c.frames[0].nodes[0].SampledAt = at.Add(-time.Minute)
	if s := c.Snapshot(""); s.Complete || s.Nodes[0].Available {
		t.Fatal("stale latest frame accepted")
	}
}
