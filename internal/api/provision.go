package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/tai-core/tai-talea/internal/onboarding"
	"github.com/tai-core/tai-talea/internal/partner"
)

// ProvisionRecord tracks one on-demand creation from API call to the moment
// the SSH provisioning pipeline takes over.
type ProvisionRecord struct {
	Name       string    `json:"name"`
	PartnerID  string    `json:"partner_id"`
	State      string    `json:"state"` // QUEUED CREATING WAITING_SSH HANDING_OVER MANAGED FAILED
	Stage      string    `json:"stage,omitempty"`
	InstanceID string    `json:"instance_id,omitempty"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type provisionTracker struct {
	mu     sync.Mutex
	byName map[string]*ProvisionRecord
}

var provisionJobs = &provisionTracker{byName: make(map[string]*ProvisionRecord)}

func (t *provisionTracker) snapshot(name string) (ProvisionRecord, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	record, ok := t.byName[name]
	if !ok {
		return ProvisionRecord{}, false
	}
	return *record, true
}

func (t *provisionTracker) list() []ProvisionRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ProvisionRecord, 0, len(t.byName))
	for _, record := range t.byName {
		out = append(out, *record)
	}
	return out
}

func (t *provisionTracker) update(name, state, stage string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	record, ok := t.byName[name]
	if !ok {
		return
	}
	record.State, record.Stage, record.UpdatedAt = state, stage, time.Now().UTC()
}

func (t *provisionTracker) fail(name string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	record, ok := t.byName[name]
	if !ok {
		return
	}
	record.State, record.Error, record.UpdatedAt = "FAILED", err.Error(), time.Now().UTC()
}

// registerProvision exposes the on-demand creation entry (development document
// §7.1: the operator configures the platform once, the control loop does the rest).
func (s *Server) registerProvision(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/capacity/console/provision", s.consoleAuth(s.handleProvision))
	mux.HandleFunc("GET /v1/capacity/console/provision", s.consoleAuth(s.handleListProvision))
}

type provisionRequest struct {
	PartnerID string `json:"partner_id"`
	Name      string `json:"name"`
	Count     int    `json:"count"`
}

func (s *Server) handleListProvision(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": provisionJobs.list()})
}

func (s *Server) handleProvision(w http.ResponseWriter, r *http.Request) {
	if s.opts.Onboarding == nil {
		writeJSON(w, 503, map[string]string{"message": "控制节点尚未配置自动安装制品"})
		return
	}
	var payload provisionRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeJSON(w, 400, map[string]string{"message": "创建参数格式不正确"})
		return
	}
	owner := consolePartner(r)
	if owner != "" {
		payload.PartnerID = owner
	}
	if payload.PartnerID == "" {
		writeJSON(w, 400, map[string]string{"message": "缺少 partner_id"})
		return
	}
	adapter, ok := s.opts.Partners.Adapter(payload.PartnerID)
	if !ok {
		writeJSON(w, 400, map[string]string{"message": "未配置的合作商"})
		return
	}
	provisioner, ok := adapter.(partner.ProvisioningAdapter)
	if !ok {
		writeJSON(w, 400, map[string]string{"message": "该合作商不支持按需创建实例"})
		return
	}
	if payload.Count < 1 {
		payload.Count = 1
	}
	if payload.Count > 4 {
		writeJSON(w, 400, map[string]string{"message": "单次最多创建 4 台"})
		return
	}

	accepted := make([]ProvisionRecord, 0, payload.Count)
	for i := 0; i < payload.Count; i++ {
		name := payload.Name
		if payload.Count > 1 || name == "" {
			name = fmt.Sprintf("talea-%s-%s", payload.PartnerID, s.now().UTC().Format("0102-150405"))
			if i > 0 {
				name = fmt.Sprintf("%s-%d", name, i)
			}
		}
		if _, exists := provisionJobs.snapshot(name); exists {
			writeJSON(w, 409, map[string]string{"message": "实例名已在创建中: " + name})
			return
		}
		now := s.now().UTC()
		record := &ProvisionRecord{Name: name, PartnerID: payload.PartnerID, State: "QUEUED", CreatedAt: now, UpdatedAt: now}
		provisionJobs.byName[name] = record
		accepted = append(accepted, *record)
		go s.runProvision(r.Context(), provisioner, payload.PartnerID, name)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "jobs": accepted})
}

// runProvision drives create → wait → ssh → hand over to the onboarding
// pipeline. Every step is persisted in the in-memory record; the onboarding
// job itself survives control plane restarts through its own state files.
func (s *Server) runProvision(parent context.Context, provisioner partner.ProvisioningAdapter, partnerID, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 45*time.Minute)
	defer cancel()

	logger := slog.Default().With("provision", name, "partner", partnerID)
	provisionJobs.update(name, "CREATING", "提交平台创建请求")
	key, err := provisioner.CreateInstance(ctx, name)
	if err != nil {
		logger.Error("creation rejected", "error", err)
		provisionJobs.fail(name, err)
		return
	}
	provisionJobs.update(name, "CREATING", "等待平台受理")
	instanceID, err := provisioner.WaitCreation(ctx, key)
	if err != nil {
		logger.Error("creation failed", "error", err)
		provisionJobs.fail(name, err)
		return
	}
	record, _ := provisionJobs.snapshot(name)
	record.InstanceID = instanceID
	provisionJobs.mu.Lock()
	if stored, ok := provisionJobs.byName[name]; ok {
		stored.InstanceID = instanceID
	}
	provisionJobs.mu.Unlock()
	logger = logger.With("instance", instanceID)

	provisionJobs.update(name, "WAITING_SSH", "等待容器 RUNNING 并获取连接信息")
	access, err := provisioner.AwaitSSHAccess(ctx, instanceID)
	if err != nil {
		logger.Error("ssh access failed", "error", err)
		provisionJobs.fail(name, err)
		return
	}

	provisionJobs.update(name, "HANDING_OVER", "移交自动铺装管道")
	request := onboarding.Request{
		RequestID: fmt.Sprintf("provision-%s-%d", name, s.now().UnixNano()),
		ID:        instanceID,
		PartnerID: partnerID,
		Host:      access.Host,
		Port:      access.Port,
		User:      access.User,
		Password:  access.Password,
	}
	if _, err := s.opts.Onboarding.Submit(ctx, request); err != nil {
		logger.Error("onboarding submission failed", "error", err)
		provisionJobs.fail(name, err)
		return
	}
	provisionJobs.update(name, "MANAGED", "铺装任务已提交，进度见 onboarding 任务列表")
	logger.Info("provisioned instance handed to onboarding", "host", access.Host, "port", access.Port)
}

var errProvisionNotFound = errors.New("provision job not found")
