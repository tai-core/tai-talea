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
	"github.com/tai-core/tai-talea/internal/store"
)

// ProvisionRecord tracks one on-demand creation from API call to the moment
// the SSH provisioning pipeline takes over.
type ProvisionRecord struct {
	Name      string `json:"name"`
	PartnerID string `json:"partner_id"`
	// State is QUEUED, CREATING, WAITING_SSH, HANDING_OVER, MANAGED, FAILED
	// or CANCELLED. QUEUED covers both "just submitted" and "the platform has
	// not scheduled a container yet": the second is where a request waits out
	// a contended pool, and it is not a failure.
	State      string    `json:"state"`
	Stage      string    `json:"stage,omitempty"`
	InstanceID string    `json:"instance_id,omitempty"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	// Path records which pipeline drives the job ("ssh" or "self"), so a
	// restart knows which resume path to take (plan P1-9).
	Path string `json:"path,omitempty"`
	// CreationKey is the opaque poll key the partner handed back for the
	// create request. It is persisted the moment CreateInstance returns: a
	// restart without it cannot follow a request the platform already
	// accepted, and asking again under a fresh name would allocate a second
	// container. It is an opaque subscription id, not a credential.
	CreationKey string `json:"creation_key,omitempty"`
}

type provisionTracker struct {
	mu     sync.Mutex
	byName map[string]*ProvisionRecord
	// cancels holds the goroutine context cancel for in-flight jobs, so an
	// operator cancel stops the pipeline instead of leaving a zombie create.
	cancels map[string]context.CancelFunc
	// persist, when set, mirrors every mutation into SQLite (plan P1-9). It
	// is nil in tests that build a bare tracker, which keeps them in-memory.
	persist store.ProvisionJobPersister
}

var provisionJobs = &provisionTracker{
	byName:  make(map[string]*ProvisionRecord),
	cancels: make(map[string]context.CancelFunc),
}

// terminalProvisionState reports whether a job needs no further tracking.
// OutstandingProvisions and the restart resume both key off this.
func terminalProvisionState(state string) bool {
	switch state {
	case "MANAGED", "FAILED", "CANCELLED":
		return true
	}
	return false
}

// storeRecord mirrors one record into SQLite. Called with the tracker lock
// released; a persist failure must not fail the job, but it is loud, because
// the job would then be lost on restart - the exact condition P1-9 removes.
func (t *provisionTracker) storeRecord(record ProvisionRecord) {
	if t.persist == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := t.persist.SaveProvisionJob(ctx, store.ProvisionJobRecord{
		Name:        record.Name,
		PartnerID:   record.PartnerID,
		Path:        record.Path,
		State:       record.State,
		Stage:       record.Stage,
		CreationKey: record.CreationKey,
		InstanceID:  record.InstanceID,
		LastError:   record.Error,
		CreatedAt:   record.CreatedAt,
		UpdatedAt:   record.UpdatedAt,
	})
	if err != nil {
		slog.Error("persisting the provision job failed; it may be lost on restart",
			"provision", record.Name, "state", record.State, "error", err)
	}
}

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

func (t *provisionTracker) add(record ProvisionRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()
	stored := record
	t.byName[record.Name] = &stored
	t.storeRecord(stored)
}

// track registers the pipeline context cancel for one job. It runs before the
// pipeline goroutine starts, so a cancel that arrives immediately still finds
// the handle.
func (t *provisionTracker) track(name string, cancel context.CancelFunc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cancels[name] = cancel
}

// done releases the cancel handle when the pipeline goroutine exits. A held
// job keeps its handle on purpose: the goroutine is gone but the job is still
// outstanding, and cancelling it must not depend on a live goroutine.
func (t *provisionTracker) done(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if record, ok := t.byName[name]; ok && terminalProvisionState(record.State) {
		delete(t.cancels, name)
	}
}

// setCreationKey persists the poll key the moment the platform returns it.
func (t *provisionTracker) setCreationKey(name, key string) {
	t.mu.Lock()
	record, ok := t.byName[name]
	if !ok {
		t.mu.Unlock()
		return
	}
	record.CreationKey = key
	stored := *record
	t.mu.Unlock()
	t.storeRecord(stored)
}

func (t *provisionTracker) setInstanceID(name, id string) {
	t.mu.Lock()
	record, ok := t.byName[name]
	if !ok {
		t.mu.Unlock()
		return
	}
	record.InstanceID = id
	stored := *record
	t.mu.Unlock()
	t.storeRecord(stored)
}

func (t *provisionTracker) update(name, state, stage string) {
	t.mu.Lock()
	record, ok := t.byName[name]
	if !ok {
		t.mu.Unlock()
		return
	}
	if record.State == "CANCELLED" {
		// CANCELLED is terminal: the pipeline notices the cancelled context a
		// moment later and must not overwrite the operator's decision.
		t.mu.Unlock()
		return
	}
	record.State, record.Stage, record.UpdatedAt = state, stage, time.Now().UTC()
	stored := *record
	t.mu.Unlock()
	t.storeRecord(stored)
}

func (t *provisionTracker) fail(name string, err error) {
	t.mu.Lock()
	record, ok := t.byName[name]
	if !ok {
		t.mu.Unlock()
		return
	}
	if record.State == "CANCELLED" {
		t.mu.Unlock()
		return
	}
	record.State, record.Error, record.UpdatedAt = "FAILED", err.Error(), time.Now().UTC()
	stored := *record
	t.mu.Unlock()
	t.storeRecord(stored)
}

// hold records an outcome that is neither a failure nor a completion: the
// partner holds the request but has not handed over a container for it. The job
// stays outstanding, which is what stops the planner from asking for the same
// capacity again every round, and it keeps the reason it is not moving - a
// request that looks queued while the platform is in fact unreachable is worse
// than one that says so.
func (t *provisionTracker) hold(name, state, stage string, cause error) {
	t.mu.Lock()
	record, ok := t.byName[name]
	if !ok {
		t.mu.Unlock()
		return
	}
	if record.State == "CANCELLED" {
		t.mu.Unlock()
		return
	}
	record.State, record.Stage, record.UpdatedAt = state, stage, time.Now().UTC()
	if cause != nil {
		record.Error = cause.Error()
	}
	stored := *record
	t.mu.Unlock()
	t.storeRecord(stored)
}

// cancel marks a job cancelled by an operator and stops its pipeline. A
// missing cancel handle is fine: held jobs outlive their goroutine.
func (t *provisionTracker) cancel(name, stage string) {
	t.mu.Lock()
	record, ok := t.byName[name]
	if !ok || terminalProvisionState(record.State) {
		t.mu.Unlock()
		return
	}
	cancelFunc := t.cancels[name]
	record.State, record.Stage, record.UpdatedAt = "CANCELLED", stage, time.Now().UTC()
	stored := *record
	t.mu.Unlock()
	if cancelFunc != nil {
		cancelFunc()
	}
	t.storeRecord(stored)
}

// rename moves a job onto the name the partner finally accepted, so the job
// describes the container that exists rather than the name it refused. It fails
// rather than clobbers when the target is already tracked, because two jobs
// under one name would make the tracker lie about which container is which.
func (t *provisionTracker) rename(from, to string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	record, ok := t.byName[from]
	if !ok {
		return false
	}
	if _, taken := t.byName[to]; taken {
		return false
	}
	delete(t.byName, from)
	record.Name = to
	record.UpdatedAt = time.Now().UTC()
	t.byName[to] = record
	// The cancel handle follows the rename, otherwise a cancel addressed to
	// the accepted name would miss the running pipeline.
	if cancelFunc, ok := t.cancels[from]; ok {
		delete(t.cancels, from)
		t.cancels[to] = cancelFunc
	}
	t.storeRecord(*record)
	if t.persist != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := t.persist.DeleteProvisionJob(ctx, from); err != nil {
			slog.Error("deleting the renamed provision job failed; the old row lingers",
				"provision", from, "error", err)
		}
	}
	return true
}

// seed loads one persisted record into the tracker without overwriting a job
// that is already tracked. It returns false for duplicates.
func (t *provisionTracker) seed(record ProvisionRecord) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.byName[record.Name]; exists {
		return false
	}
	stored := record
	t.byName[record.Name] = &stored
	return true
}

// registerProvision exposes the on-demand creation entry (development document
// §7.1: the operator configures the platform once, the control loop does the rest).
func (s *Server) registerProvision(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/capacity/console/provision", s.consoleAuth(s.handleProvision))
	mux.HandleFunc("GET /v1/capacity/console/provision", s.consoleAuth(s.handleListProvision))
	// Plan P1-9: the cancel entry. A queued request holds a planner slot
	// forever, so the operator needs a way to give it up on purpose.
	mux.HandleFunc("DELETE /v1/capacity/console/provision/{name}", s.consoleAuth(s.handleCancelProvision))
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
	if payload.Count < 1 {
		payload.Count = 1
	}
	if payload.Count > 4 {
		writeJSON(w, 400, map[string]string{"message": "单次最多创建 4 台"})
		return
	}

	accepted := make([]ProvisionRecord, 0, payload.Count)
	baseName := payload.Name
	for i := 0; i < payload.Count; i++ {
		name := baseName
		if i > 0 {
			name = fmt.Sprintf("%s-%d", baseName, i)
		}
		record, err := s.RequestCapacity(r.Context(), payload.PartnerID, name)
		if err != nil {
			writeJSON(w, provisionErrorStatus(err), map[string]string{"message": err.Error()})
			return
		}
		if baseName == "" {
			baseName = record.Name
		}
		accepted = append(accepted, record)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "jobs": accepted})
}

// handleCancelProvision gives up on one creation job. The job must be
// outstanding: the cancel of a finished job is a 409 rather than a silent
// success, because it almost always means the caller is acting on stale state.
//
// A container the platform already placed is billable, so the cancel releases
// it first - the same rule the abandon path follows. A request that is still
// only queued owns nothing; cancelling it gives up the queue position, which
// is exactly what the operator asked for.
func (s *Server) handleCancelProvision(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	record, ok := provisionJobs.snapshot(name)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "创建任务不存在"})
		return
	}
	if terminalProvisionState(record.State) {
		writeJSON(w, http.StatusConflict, map[string]string{"message": "任务已结束，无法取消"})
		return
	}
	if record.InstanceID != "" {
		s.releaseProvisionContainer(r.Context(), record, "operator cancel")
	}
	provisionJobs.cancel(name, "操作员取消创建任务")
	slog.Info("provision job cancelled by operator", "provision", name, "had_instance", record.InstanceID != "")
	writeJSON(w, http.StatusAccepted, map[string]any{"cancelled": true, "job": func() ProvisionRecord {
		job, _ := provisionJobs.snapshot(name)
		return job
	}()})
}

// provisionAdapter resolves a partner adapter for the provisioning paths.
// The indirection exists so tests can inject fakes without building a
// registry from configuration.
func (s *Server) provisionAdapter(partnerID string) (partner.PartnerAdapter, bool) {
	if s.resolveAdapter != nil {
		return s.resolveAdapter(partnerID)
	}
	if s.opts.Partners == nil {
		return nil, false
	}
	return s.opts.Partners.Adapter(partnerID)
}

// releaseProvisionContainer releases a container a provision job already
// placed. Best effort, like abandon: a failed release leaves the container
// visible to the poll, which is the state an operator can reclaim from the
// platform console.
func (s *Server) releaseProvisionContainer(ctx context.Context, record ProvisionRecord, cause string) {
	adapter, ok := s.provisionAdapter(record.PartnerID)
	if !ok {
		return
	}
	partnerAdapter, ok := adapter.(partner.PartnerAdapter)
	if !ok {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := partnerAdapter.ReleaseInstance(callCtx, record.InstanceID); err != nil {
		slog.Warn("cancelled provision job did not release its container",
			"provision", record.Name, "instance", record.InstanceID, "cause", cause, "error", err.Error())
		return
	}
	slog.Info("cancelled provision job released its container",
		"provision", record.Name, "instance", record.InstanceID, "cause", cause)
}

// provisionErrorStatus maps a creation failure to the status the console shows.
func provisionErrorStatus(err error) int {
	switch {
	case errors.Is(err, errProvisionUnsupportedPartner):
		return http.StatusBadRequest
	case errors.Is(err, errProvisionNoOnboarding):
		return http.StatusServiceUnavailable
	case errors.Is(err, errProvisionNameInUse):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// RequestCapacity starts one on-demand creation without an HTTP request behind
// it, so the console and the automatic capacity loop go through exactly one
// provisioning path. It returns as soon as the job is recorded: creating a
// container on a full pool can stay QUEUED for hours.
func (s *Server) RequestCapacity(ctx context.Context, partnerID, name string) (ProvisionRecord, error) {
	adapter, ok := s.provisionAdapter(partnerID)
	if !ok {
		return ProvisionRecord{}, fmt.Errorf("%w: %s", errProvisionUnsupportedPartner, partnerID)
	}
	// Two shapes of on-demand creation exist. A partner whose containers carry
	// the bootstrap only needs creating; a partner that hands over a bare
	// container needs the SSH pipeline behind it.
	selfProvisioner, selfOK := adapter.(partner.SelfProvisioningAdapter)
	sshProvisioner, sshOK := adapter.(partner.ProvisioningAdapter)
	if !selfOK && !sshOK {
		return ProvisionRecord{}, fmt.Errorf("%w: %s", errProvisionUnsupportedPartner, partnerID)
	}
	if name == "" {
		name = fmt.Sprintf("talea-%s-%s", partnerID, s.now().UTC().Format("0102-150405"))
	}
	// The name guard runs before the onboarding check on purpose: after a
	// restart the tracker holds the history of managed containers, and "this
	// name is already a live container" (409) is the honest answer even when
	// onboarding is not configured at all.
	if _, exists := provisionJobs.snapshot(name); exists {
		return ProvisionRecord{}, fmt.Errorf("%w: %s", errProvisionNameInUse, name)
	}
	if sshOK && s.opts.Onboarding == nil {
		return ProvisionRecord{}, errProvisionNoOnboarding
	}
	now := s.now().UTC()
	path := "ssh"
	if selfOK {
		path = "self"
	}
	record := ProvisionRecord{
		Name: name, PartnerID: partnerID, State: "QUEUED", Path: path,
		CreatedAt: now, UpdatedAt: now,
	}
	provisionJobs.add(record)
	// The pipeline context is detached from the request but stays cancelable,
	// so the operator cancel endpoint can stop it (plan P1-9).
	runCtx, runCancel := context.WithCancel(context.WithoutCancel(ctx))
	provisionJobs.track(name, runCancel)
	if selfOK {
		go s.runSelfProvision(runCtx, selfProvisioner, partnerID, name)
	} else {
		go s.runProvision(runCtx, sshProvisioner, partnerID, name)
	}
	return record, nil
}

// OutstandingProvisions counts creations that have not reached a terminal
// state. The planner uses it to avoid asking again while the platform is still
// deciding whether to hand over a container, which is the difference between
// one request and one request every planner round for hours.
func OutstandingProvisions() int {
	outstanding := 0
	for _, record := range provisionJobs.list() {
		if !terminalProvisionState(record.State) {
			outstanding++
		}
	}
	return outstanding
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

// provisionAbandon builds the best-effort release closure both provisioning
// pipelines share. It must only be called for a container that is known to
// exist: the platform lists a queued request too, so releasing on an
// unconfirmed existence silently trades our place in the queue for nothing.
// A failed release is deliberately non-fatal - the job keeps its original
// error, and a container that survives stays visible to the poll, which is
// exactly the state an operator can reclaim from the console.
func provisionAbandon(parent context.Context, provisioner partner.ProvisioningAdapter, name string, logger *slog.Logger) func(instanceID string, cause error) {
	return func(instanceID string, cause error) {
		if instanceID == "" {
			return
		}
		// A cancel owns the release: the cancel entry releases the container
		// and marks the job before the pipeline context unblocks, so an
		// abandon that runs afterwards would only double the release and
		// muddy who did what.
		if record, ok := provisionJobs.snapshot(name); ok && record.State == "CANCELLED" {
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
}

// runSelfProvision drives create → wait → enrol. Nothing is installed inside
// the container: the image entrypoint already started the bootstrap with the
// shared secret, so the endpoints the Pod reports are usable the moment it is
// running. This is the path a platform without a login takes.
func (s *Server) runSelfProvision(parent context.Context, provisioner partner.SelfProvisioningAdapter, partnerID, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), s.provisionTimeout())
	defer cancel()
	defer provisionJobs.done(name)

	logger := slog.Default().With("provision", name, "partner", partnerID)
	provisionJobs.update(name, "CREATING", "请求容器平台创建实例")
	node, err := provisioner.ProvisionNode(ctx, name)
	if err != nil {
		logger.Error("container creation failed", "error", err)
		provisionJobs.fail(name, err)
		return
	}
	provisionJobs.setInstanceID(name, node.ID)
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
// pipeline. Every step is mirrored into the durable provision_jobs row, so a
// restart can pick the request up where it stopped (plan P1-9); the onboarding
// job itself survives control plane restarts through its own state files.
//
// The contract is all-or-nothing once a container exists: the moment the
// partner has placed one, it is billed, so a failure that leaves it unusable
// releases it again instead of leaking the partner's quota. Accepting a request
// is not the same as placing a container, though. While the request is still
// queued nothing exists to bill and nothing exists to reclaim, so releasing it
// would only destroy our position in the queue - that case is held, not failed.
func (s *Server) runProvision(parent context.Context, provisioner partner.ProvisioningAdapter, partnerID, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), s.provisionTimeout())
	defer cancel()
	defer provisionJobs.done(name)

	logger := slog.Default().With("provision", name, "partner", partnerID)
	abandon := provisionAbandon(parent, provisioner, name, logger)

	// A name a released container already used is dead on this platform: the
	// name stays reserved while the container itself disappears from every
	// listing, so the clash surfaces only here, on the create call. Nothing has
	// been allocated at that point, which makes a fresh name a free retry
	// rather than a failure to report.
	baseName := name
	provisionJobs.update(name, "CREATING", "提交平台创建请求")
	key, err := provisioner.CreateInstance(ctx, name)
	for attempt := 2; err != nil && errors.Is(err, partner.ErrNameInUse) && attempt <= maxProvisionNameAttempts; attempt++ {
		candidate := fmt.Sprintf("%s-%d", baseName, attempt)
		logger.Warn("instance name is taken on the platform, retrying", "name", candidate, "cause", err.Error())
		if !provisionJobs.rename(name, candidate) {
			// The candidate is already tracked by another job. Step over it
			// rather than colliding with a job that is already in flight.
			continue
		}
		name = candidate
		logger = slog.Default().With("provision", name, "partner", partnerID)
		provisionJobs.update(name, "CREATING", "平台已有同名实例，改用新名字重新提交")
		key, err = provisioner.CreateInstance(ctx, name)
	}
	if err != nil {
		logger.Error("creation rejected", "error", err)
		provisionJobs.fail(name, err)
		return
	}
	provisionJobs.setCreationKey(name, key)
	provisionJobs.update(name, "CREATING", "等待平台受理")
	s.followProvision(ctx, provisioner, partnerID, name, key, "", logger, abandon)
}

// followProvision drives everything after the platform accepted the create
// request: poll the creation key, wait for the container to come up, hand over
// to the onboarding pipeline. It is also the restart resume path, which is why
// the creation key and the instance id arrive as arguments - a resumed job
// enters at the stage its persisted row describes instead of asking the
// platform for a second container.
func (s *Server) followProvision(ctx context.Context, provisioner partner.ProvisioningAdapter, partnerID, name, key, instanceID string, logger *slog.Logger, abandon func(string, error)) {
	if instanceID == "" {
		// Deliberately `=` on the outer variable: it feeds the logger, the
		// setInstanceID bookkeeping and abandon below, so a shadowed local
		// would quietly make abandon release nothing.
		created, err := provisioner.WaitCreation(ctx, key)
		if err != nil {
			logger.Error("creation failed", "error", err)
			provisionJobs.fail(name, err)
			return
		}
		provisionJobs.setInstanceID(name, created)
		instanceID = created
	}
	logger = logger.With("instance", instanceID)

	provisionJobs.update(name, "WAITING_SSH", "等待容器 RUNNING 并获取连接信息")
	access, err := provisioner.AwaitSSHAccess(ctx, instanceID)
	if err != nil {
		if errors.Is(err, partner.ErrAwaitingCapacity) {
			// The platform holds the request but never gave it a container. The
			// waiting budget expiring here says nothing about whether capacity
			// will arrive, and releasing would only put us back at the end of
			// the queue, so the job is held as outstanding instead.
			logger.Warn("creation is still queued at the partner", "error", err.Error())
			provisionJobs.hold(name, "QUEUED", "平台已受理创建请求，仍在排队等待算力（未占用容器、不计费；保留队列位次）", err)
			return
		}
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

// maxProvisionNameAttempts bounds the rename ladder a name collision can walk
// (the requested name, then -2 through -5). Every released container poisons
// one name forever and each retry only costs a round trip, so a handful of
// steps is enough to get past a handful of leftovers; a deeper ladder would
// mostly produce names nobody recognises in the platform console.
const maxProvisionNameAttempts = 5

// ResumeProvisions reloads the provision jobs an earlier process persisted and
// re-arms the ones that have not reached a terminal state (plan P1-9). main
// calls it once after startup recovery, before the listener opens.
//
// Resume semantics per persisted stage:
//   - a job with an instance id enters at AwaitSSHAccess. This covers
//     WAITING_SSH, HANDING_OVER and a held QUEUED job, whose instance id was
//     already assigned when the platform accepted the request.
//   - a job with only a creation key enters at WaitCreation.
//   - a job with neither is one whose create call may never have been
//     submitted, or whose key was lost in the crash window. It re-creates
//     under the SAME name. A fresh name would be wrong in the worst way: the
//     first request may still exist, and two requests are two containers. A
//     name-in-use refusal therefore fails the job with operator guidance
//     instead of walking the rename ladder.
//
// Each process lifetime grants the job a fresh timeout budget; the persisted
// created_at keeps the original timeline for audit. A queued job the operator
// no longer wants is cleared with the cancel entry.
func (s *Server) ResumeProvisions() {
	if provisionJobs.persist == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	jobs, err := provisionJobs.persist.ListProvisionJobs(ctx)
	if err != nil {
		slog.Error("loading persisted provision jobs failed", "error", err)
		return
	}
	resumed := 0
	for _, job := range jobs {
		record := ProvisionRecord{
			Name:        job.Name,
			PartnerID:   job.PartnerID,
			Path:        job.Path,
			State:       job.State,
			Stage:       job.Stage,
			CreationKey: job.CreationKey,
			InstanceID:  job.InstanceID,
			Error:       job.LastError,
			CreatedAt:   job.CreatedAt,
			UpdatedAt:   job.UpdatedAt,
		}
		if record.Path == "" {
			// Rows written before the path column existed were all SSH jobs.
			record.Path = "ssh"
		}
		provisionJobs.seed(record)
		if terminalProvisionState(record.State) {
			continue
		}
		adapter, ok := s.provisionAdapter(record.PartnerID)
		if !ok {
			provisionJobs.fail(record.Name, fmt.Errorf("重启后无法恢复创建任务：合作商 %s 未配置", record.PartnerID))
			continue
		}
		switch record.Path {
		case "self":
			selfProvisioner, ok := adapter.(partner.SelfProvisioningAdapter)
			if !ok {
				provisionJobs.fail(record.Name, fmt.Errorf("重启后无法恢复创建任务：合作商 %s 不再支持自带引导的创建", record.PartnerID))
				continue
			}
			// ProvisionNode is idempotent for the partners that use it (the
			// container the first call placed is adopted, not duplicated).
			go s.runSelfProvision(context.Background(), selfProvisioner, record.PartnerID, record.Name)
			resumed++
		case "ssh":
			provisioner, ok := adapter.(partner.ProvisioningAdapter)
			if !ok {
				provisionJobs.fail(record.Name, fmt.Errorf("重启后无法恢复创建任务：合作商 %s 不再支持按需创建", record.PartnerID))
				continue
			}
			go s.resumeProvision(provisioner, record.PartnerID, record.Name)
			resumed++
		default:
			provisionJobs.fail(record.Name, fmt.Errorf("未知的创建管道 %q", record.Path))
		}
	}
	if resumed > 0 {
		slog.Info("resumed provision jobs from the previous run", "resumed", resumed)
	}
}

// resumeProvision re-arms one persisted SSH-pipeline job. See
// ResumeProvisions for the stage table it implements.
func (s *Server) resumeProvision(provisioner partner.ProvisioningAdapter, partnerID, name string) {
	record, ok := provisionJobs.snapshot(name)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.provisionTimeout())
	defer cancel()
	provisionJobs.track(name, cancel)
	defer provisionJobs.done(name)

	logger := slog.Default().With("provision", name, "partner", partnerID, "resumed", true)
	abandon := provisionAbandon(ctx, provisioner, name, logger)
	switch {
	case record.InstanceID != "":
		provisionJobs.update(name, "WAITING_SSH", "控制面重启后继续等待容器连接信息")
		s.followProvision(ctx, provisioner, partnerID, name, record.CreationKey, record.InstanceID, logger, abandon)
	case record.CreationKey != "":
		provisionJobs.update(name, "CREATING", "控制面重启后继续等待平台受理")
		s.followProvision(ctx, provisioner, partnerID, name, record.CreationKey, "", logger, abandon)
	default:
		// The create call may never have landed or its key was lost in the
		// crash window. Re-submit under the same name; a name-in-use refusal
		// means the first request probably exists, and renaming here would
		// allocate a second container on top of it.
		provisionJobs.update(name, "CREATING", "控制面重启后重新提交创建请求")
		key, err := provisioner.CreateInstance(ctx, name)
		if err != nil {
			if errors.Is(err, partner.ErrNameInUse) {
				provisionJobs.fail(name, fmt.Errorf(
					"重启后丢失创建凭据且平台报实例名已占用：请到平台控制台核对 %s 是否已创建，确认后取消本任务", name))
			} else {
				provisionJobs.fail(name, err)
			}
			return
		}
		provisionJobs.setCreationKey(name, key)
		provisionJobs.update(name, "CREATING", "等待平台受理")
		s.followProvision(ctx, provisioner, partnerID, name, key, "", logger, abandon)
	}
}

var (
	errProvisionNotFound = errors.New("provision job not found")
	// The three creation failures the console reports with a specific status.
	errProvisionUnsupportedPartner = errors.New("未配置或不支持按需创建的合作商")
	errProvisionNoOnboarding       = errors.New("控制节点尚未配置自动安装制品")
	errProvisionNameInUse          = errors.New("实例名已在创建中")
)
