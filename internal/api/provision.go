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
	// Two shapes of on-demand creation exist. A partner whose containers carry
	// the bootstrap only needs creating; a partner that hands over a bare
	// container needs the SSH pipeline behind it.
	selfProvisioner, selfOK := adapter.(partner.SelfProvisioningAdapter)
	sshProvisioner, sshOK := adapter.(partner.ProvisioningAdapter)
	if !selfOK && !sshOK {
		writeJSON(w, 400, map[string]string{"message": "该合作商不支持按需创建实例"})
		return
	}
	if sshOK && s.opts.Onboarding == nil {
		writeJSON(w, 503, map[string]string{"message": "控制节点尚未配置自动安装制品"})
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
		if selfOK {
			go s.runSelfProvision(r.Context(), selfProvisioner, payload.PartnerID, name)
		} else {
			go s.runProvision(r.Context(), sshProvisioner, payload.PartnerID, name)
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "jobs": accepted})
}

// provisionTimeout bounds one end-to-end creation. A partner whose pool is
// full can leave a creation queued for hours and the platform never cancels it,
// so this is the operator's dial between "give up and free the slot" and "wait
// for capacity". Unset keeps the historical 45 minutes.
func (s *Server) provisionTimeout() time.Duration {
	if d := s.opts.Config.Controller.ProvisionTimeout.Duration(); d > 0 {
		return d
	}
	return 45 * time.Minute
}

// runSelfProvision drives create → wait → enrol. Nothing is installed inside
// the container: the image entrypoint already started the bootstrap with the
// shared secret, so the endpoints the Pod reports are usable the moment it is
// running. This is the path a platform without a login takes.
func (s *Server) runSelfProvision(parent context.Context, provisioner partner.SelfProvisioningAdapter, partnerID, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), s.provisionTimeout())
	defer cancel()

	logger := slog.Default().With("provision", name, "partner", partnerID)
	provisionJobs.update(name, "CREATING", "请求容器平台创建实例")
	node, err := provisioner.ProvisionNode(ctx, name)
	if err != nil {
		logger.Error("container creation failed", "error", err)
		provisionJobs.fail(name, err)
		return
	}
	provisionJobs.mu.Lock()
	if stored, ok := provisionJobs.byName[name]; ok {
		stored.InstanceID = node.ID
	}
	provisionJobs.mu.Unlock()
	logger = logger.With("instance", node.ID)

	provisionJobs.update(name, "HANDING_OVER", "容器自带引导程序，直接纳入容量")
	if err := s.opts.Controller.EnrollPrepared(ctx, node.ToInstance(partnerID)); err != nil {
		logger.Error("enrolment failed", "error", err)
		provisionJobs.fail(name, err)
		return
	}
	provisionJobs.update(name, "MANAGED", "已纳入容量，等待分配 P/D 角色")
	logger.Info("container enrolled", "endpoint", node.Endpoint, "service_endpoint", node.ServiceEndpoint)
}

// runProvision drives create → wait → ssh → hand over to the onboarding
// pipeline. Every step is persisted in the in-memory record; the onboarding
// job itself survives control plane restarts through its own state files.
//
// The contract is all-or-nothing: once the partner has accepted a creation
// request the container exists and is billed, so a failure that leaves it
// unusable releases it again instead of leaking the partner's quota.
func (s *Server) runProvision(parent context.Context, provisioner partner.ProvisioningAdapter, partnerID, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), s.provisionTimeout())
	defer cancel()

	logger := slog.Default().With("provision", name, "partner", partnerID)
	// abandon releases a container that never became a usable node. It is
	// deliberately best-effort: the job still reports its original error, and a
	// failed release leaves the container visible to the poll, which is exactly
	// the state an operator can reclaim from the console.
	abandon := func(instanceID string, cause error) {
		if instanceID == "" {
			return
		}
		adapter, ok := provisioner.(partner.PartnerAdapter)
		if !ok {
			return
		}
		callCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(parent), time.Minute)
		defer releaseCancel()
		if err := adapter.ReleaseInstance(callCtx, instanceID); err != nil {
			logger.Warn("abandoned instance was not released", "instance", instanceID, "error", err.Error())
			return
		}
		logger.Info("abandoned instance released", "instance", instanceID, "cause", cause.Error())
	}

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
		abandon(instanceID, err)
		return
	}

	// Enrol under the identity the partner's snapshot reports, not under the
	// id the platform minted: the poll keys containers by name on every
	// platform whose create call names the container, and a mismatched
	// enrolment would leave two rows describing one physical box.
	enrolID, leaseID := instanceID, ""
	if identity, ok := provisioner.(partner.InstanceIdentity); ok {
		enrolID, leaseID = identity.ProvisionIdentity(name)
	}

	provisionJobs.update(name, "HANDING_OVER", "移交自动铺装管道")
	request := onboarding.Request{
		RequestID: fmt.Sprintf("provision-%s-%d", name, s.now().UnixNano()),
		ID:        enrolID,
		PartnerID: partnerID,
		Host:      access.Host,
		Port:      access.Port,
		User:      access.User,
		Password:  access.Password,
		LeaseID:   leaseID,
	}
	if _, err := s.opts.Onboarding.Submit(ctx, request); err != nil {
		logger.Error("onboarding submission failed", "error", err)
		provisionJobs.fail(name, err)
		abandon(instanceID, err)
		return
	}
	provisionJobs.update(name, "MANAGED", "铺装任务已提交，进度见 onboarding 任务列表")
	logger.Info("provisioned instance handed to onboarding", "host", access.Host, "port", access.Port, "enrol_id", enrolID)
}

var errProvisionNotFound = errors.New("provision job not found")
