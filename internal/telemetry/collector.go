package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/routeradapter"
)

const Interval = 3 * time.Second
const MaxAge = 15 * time.Second

type inventory interface {
	ListInstances(context.Context) ([]domain.Instance, error)
}
type router interface {
	GetLoads(context.Context) ([]routeradapter.WorkerLoad, error)
}

type Node struct {
	ID           string      `json:"id"`
	PartnerID    string      `json:"partner_id"`
	Role         domain.Role `json:"role"`
	ObservedRole domain.Role `json:"observed_role,omitempty"`
	RouterRole   string      `json:"router_role,omitempty"`
	SampledAt    time.Time   `json:"sampled_at"`
	Available    bool        `json:"available"`
	Error        string      `json:"error,omitempty"`
	TP           int         `json:"tp"`
	DP           int         `json:"dp"`
	Running      *float64    `json:"running"`
	Queue        *float64    `json:"queue"`
	Prealloc     *float64    `json:"prealloc"`
	Transfer     *float64    `json:"transfer"`
	MaxRunning   *float64    `json:"max_running"`
	Cache        *float64    `json:"cache"`
	RouterLoad   *float64    `json:"router_load"`
	InputRate    *float64    `json:"input_tokens_per_second"`
	OutputRate   *float64    `json:"output_tokens_per_second"`
	Pressure     *float64    `json:"pressure"`
	// identity is private: it invalidates counter baselines after role/lease changes.
	identity string
	counters map[string]float64
}
type Role struct {
	Nodes     int      `json:"nodes"`
	Valid     int      `json:"valid"`
	Running   *float64 `json:"running"`
	Queue     *float64 `json:"queue"`
	Transfer  *float64 `json:"transfer"`
	Pressure  *float64 `json:"pressure"`
	TokenRate *float64 `json:"tokens_per_second"`
}
type Point struct {
	At      time.Time `json:"at"`
	Prefill Role      `json:"prefill"`
	Decode  Role      `json:"decode"`
}
type Snapshot struct {
	Point
	Nodes       []Node   `json:"nodes"`
	History     []Point  `json:"history"`
	Complete    bool     `json:"complete"`
	PressureGap *float64 `json:"pressure_gap"`
	Advice      string   `json:"advice"`
}
type frame struct {
	at    time.Time
	nodes []Node
}
type Collector struct {
	store  inventory
	router router
	client *http.Client
	mu     sync.RWMutex
	frames []frame
}

func New(store inventory, router router) *Collector {
	return &Collector{store: store, router: router, client: &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Collector) Run(ctx context.Context) {
	timer := time.NewTicker(Interval)
	defer timer.Stop()
	for {
		c.Sample(ctx)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}
func (c *Collector) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if len(data) > 4<<20 {
		return nil, fmt.Errorf("metrics response too large")
	}
	return data, err
}
func (c *Collector) scrape(ctx context.Context, instance domain.Instance, old Node) Node {
	n := Node{ID: instance.ID, PartnerID: instance.PartnerID, Role: instance.Role, SampledAt: time.Now().UTC(), identity: instance.LeaseID + instance.RoleAssignedAt.String() + instance.ServiceURL()}
	raw, err := c.get(ctx, strings.TrimRight(instance.ServiceURL(), "/")+"/metrics")
	if err != nil {
		n.Error = "指标不可用（请检查 --enable-metrics）"
		return n
	}
	n.counters, err = parseMetrics(strings.NewReader(string(raw)))
	if err != nil {
		n.Error = "指标格式不受支持"
		return n
	}
	n.Running = value(n.counters, "num_running_reqs")
	n.Queue = value(n.counters, "num_queue_reqs")
	n.Cache = value(n.counters, "token_usage")
	if n.Role == domain.RolePrefill {
		n.Prealloc = value(n.counters, "num_prefill_prealloc_queue_reqs")
		n.Transfer = value(n.counters, "num_prefill_inflight_queue_reqs")
	} else {
		n.Prealloc = value(n.counters, "num_decode_prealloc_queue_reqs")
		n.Transfer = value(n.counters, "num_decode_transfer_queue_reqs")
	}
	raw, err = c.get(ctx, strings.TrimRight(instance.ServiceURL(), "/")+"/server_info")
	var info struct {
		Role   domain.Role `json:"disaggregation_mode"`
		TP     int         `json:"tp_size"`
		DP     int         `json:"dp_size"`
		States []struct {
			Max float64 `json:"effective_max_running_requests_per_dp"`
		} `json:"internal_states"`
	}
	if err != nil || json.Unmarshal(raw, &info) != nil {
		n.Error = "无法核实服务角色和并发上限"
		return n
	}
	n.ObservedRole = info.Role
	n.TP = info.TP
	n.DP = info.DP
	if n.Role != n.ObservedRole {
		n.Error = "运行角色与 Talea 分配不一致"
		return n
	}
	if checker, ok := c.router.(interface {
		GetWorker(context.Context, string) (routeradapter.Worker, error)
	}); ok {
		worker, err := checker.GetWorker(ctx, instance.RouterWorkerID)
		n.RouterRole = worker.WorkerType
		if err != nil || worker.URL != instance.ServiceURL() || worker.WorkerType != string(n.Role) {
			n.Error = "Router 注册角色或地址与运行服务不一致"
			return n
		}
		if !worker.Healthy || worker.Metadata["__pd_state"] == "draining" {
			n.Error = "Router 中节点不健康或 P/D 池仍在摘流"
			return n
		}
	}
	if checker, ok := c.router.(interface {
		GetReadiness(context.Context, string) (routeradapter.ReadinessRecord, error)
	}); ok {
		ready, err := checker.GetReadiness(ctx, instance.RouterWorkerID)
		if err != nil || ready.WorkerURL != instance.ServiceURL() || !ready.Ready() {
			n.Error = "Router 尚未开放此节点路由"
			return n
		}
	}
	// Use actual scheduler limits, not max_running_requests (which is adjusted by DP/memory).
	for _, s := range info.States {
		if s.Max > 0 {
			if n.MaxRunning == nil {
				n.MaxRunning = number(0)
			}
			*n.MaxRunning += s.Max
		}
	}
	n.Available = n.Running != nil && n.Queue != nil && n.MaxRunning != nil && n.Prealloc != nil && n.Transfer != nil
	if !n.Available {
		n.Error = "缺少调度指标或有效并发上限"
		return n
	}
	n.Pressure = number((*n.Running + *n.Queue + *n.Prealloc) / *n.MaxRunning)
	if old.Available && old.identity == n.identity && old.Role == n.Role {
		seconds := n.SampledAt.Sub(old.SampledAt).Seconds()
		// Completion counters include cached input; collect only the appropriate
		// side of PD so prompt/generation counters cannot be double counted.
		if n.Role == domain.RolePrefill {
			n.InputRate = delta(n.counters, old.counters, "prompt_tokens_total", seconds)
		} else {
			n.OutputRate = delta(n.counters, old.counters, "generation_tokens_total", seconds)
		}
	}
	return n
}
func (c *Collector) Sample(ctx context.Context) {
	instances, err := c.store.ListInstances(ctx)
	if err != nil {
		return
	}
	old := map[string]Node{}
	c.mu.RLock()
	if len(c.frames) > 0 {
		for _, n := range c.frames[len(c.frames)-1].nodes {
			old[n.ID] = n
		}
	}
	c.mu.RUnlock()
	var active []domain.Instance
	for _, i := range instances {
		if i.Role.Valid() && i.HasService() && i.InstanceState != domain.InstanceReleased {
			active = append(active, i)
		}
	}
	nodes := make([]Node, len(active))
	var wg sync.WaitGroup
	limit := make(chan struct{}, 8)
	var loads []routeradapter.WorkerLoad
	wg.Add(1)
	go func() {
		defer wg.Done()
		call, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		loads, _ = c.router.GetLoads(call)
	}()
	for idx, i := range active {
		wg.Add(1)
		go func(idx int, i domain.Instance) {
			defer wg.Done()
			select {
			case limit <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-limit }()
			call, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			nodes[idx] = c.scrape(call, i, old[i.ID])
		}(idx, i)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	for idx, i := range active {
		for _, l := range loads {
			if strings.TrimRight(l.Worker, "/") == strings.TrimRight(i.ServiceURL(), "/") && l.Load >= 0 {
				nodes[idx].RouterLoad = number(float64(l.Load))
			}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, frame{time.Now().UTC(), nodes})
	if len(c.frames) > 100 {
		c.frames = c.frames[len(c.frames)-100:]
	}
}
func aggregate(nodes []Node, role domain.Role, at time.Time) Role {
	r := Role{}
	running, queued, transfer, capacity, tokens := 0.0, 0.0, 0.0, 0.0, 0.0
	rates := 0
	for _, n := range nodes {
		if n.Role != role {
			continue
		}
		r.Nodes++
		if !n.Available || at.Sub(n.SampledAt) > MaxAge {
			continue
		}
		r.Valid++
		running += *n.Running
		queued += *n.Queue + *n.Prealloc
		transfer += *n.Transfer
		capacity += *n.MaxRunning
		rate := n.InputRate
		if role == domain.RoleDecode {
			rate = n.OutputRate
		}
		if rate != nil {
			tokens += *rate
			rates++
		}
	}
	if r.Nodes > 0 && r.Valid == r.Nodes {
		r.Running = number(running)
		r.Queue = number(queued)
		r.Transfer = number(transfer)
		r.Pressure = number((running + queued) / capacity)
		if rates == r.Nodes {
			r.TokenRate = number(tokens)
		}
	}
	return r
}
func (c *Collector) Snapshot(owner string) Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := Snapshot{Nodes: []Node{}, History: []Point{}, Advice: "等待 P/D 完整指标"}
	for idx, f := range c.frames {
		nodes := []Node{}
		for _, n := range f.nodes {
			if owner == "" || n.PartnerID == owner {
				nodes = append(nodes, n)
			}
		}
		at := f.at
		if idx == len(c.frames)-1 {
			at = time.Now().UTC()
		}
		p := Point{At: f.at, Prefill: aggregate(nodes, domain.RolePrefill, at), Decode: aggregate(nodes, domain.RoleDecode, at)}
		out.History = append(out.History, p)
		out.Point = p
		out.Nodes = nodes
	}
	for idx := range out.Nodes {
		n := &out.Nodes[idx]
		if time.Since(n.SampledAt) > MaxAge {
			n.Available = false
			n.Error = "采样已过期"
			n.RouterLoad = nil
			n.Pressure = nil
			n.InputRate = nil
			n.OutputRate = nil
		}
	}
	p, d := out.Prefill, out.Decode
	out.Complete = p.Pressure != nil && d.Pressure != nil
	if out.Complete {
		gap := *p.Pressure - *d.Pressure
		out.PressureGap = &gap
		switch {
		case *p.Transfer+*d.Transfer > 0:
			out.Advice = "存在 KV 传输在途任务，结合持续排队和 TTFT 判断传输压力"
		case *p.Queue > 0 && gap > 0.2:
			out.Advice = "P 侧排队压力较高；持续出现时考虑增加 Prefill 容量"
		case *d.Queue > 0 && gap < -0.2:
			out.Advice = "D 侧排队压力较高；持续出现时考虑增加 Decode 容量"
		case *p.Queue+*d.Queue > 0:
			out.Advice = "两侧存在排队；结合 TTFT / TPOT 和持续负载判断容量"
		case math.Max(*p.Pressure, *d.Pressure) == 0:
			out.Advice = "当前空闲；需要持续请求才能判断 P/D 配比"
		default:
			out.Advice = "暂无明显排队；压力差仅表示并发占用差异，不代表 GPU 利用率"
		}
	}
	return out
}
