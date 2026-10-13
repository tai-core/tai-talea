package alayanew

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/partner"
)

// Config is the platform-specific wiring for one Alaya NeW partner.
type Config struct {
	AccessKey string
	SecretKey string
	// AIDCId selects the intelligent computing center; 0 means all.
	AIDCId int
	// ModelSupport is advertised on every instance so the planner can match
	// the configured model. Usually a single entry: the controller's model_id.
	ModelSupport []string
	// Creation defaults (Task: provision instances through the console API).
	ProductCode      string            `json:"product_code"`
	Image            string            `json:"image"`
	NASStorageID     string            `json:"nas_storage_id"`
	NASFileDirectory string            `json:"nas_file_directory"`
	NASMountPath     string            `json:"nas_mount_path"`
	NASReadOnly      bool              `json:"nas_read_only"`
	Env              map[string]string `json:"env"`
	ComputeResource  string            `json:"compute_resource"`
	// Priority is the scheduling priority asked for at creation. The platform
	// queues a request when the pool has no free node and serves the queue by
	// priority (observed default 3, lower wins). Nil leaves the platform's own
	// default in place.
	Priority *int `json:"priority"`
	// BootstrapPort and ServicePort are the container ports published by CCI.
	BootstrapPort int
	ServicePort   int
	// AdvertiseHosts maps an instance name to the address peers inside the
	// intelligent computing center should use. CCI port mappings expose the
	// public ingress IP, but cross-worker traffic (Router health checks, PD
	// disaggregation) runs inside the same network, where the public IP is
	// not reachable for the ports the containers do not publish — the PD
	// transfer handshake in particular has no published port at all.
	//
	// It is the static half of the mapping: operators use it for instances
	// that were never provisioned through Talea. Containers Talea created
	// itself are covered by AdvertiseResolver instead, because their address
	// only becomes known once the installation measured it from the inside.
	AdvertiseHosts map[string]string `json:"advertise_hosts"`
	// AdvertiseResolver supplies the address the control plane verified for an
	// instance it installed, keyed by instance name. It is consulted only when
	// AdvertiseHosts has no entry, and an empty result falls back to the
	// address the platform publishes.
	AdvertiseResolver func(name string) string `json:"-"`
	// Timeout bounds one signed request.
	Timeout time.Duration
	// WaitTimeout bounds waiting for a created instance that already exists to
	// reach RUNNING. It deliberately does not bound waiting for the platform to
	// place a still-queued request: that phase has no container to give up.
	WaitTimeout time.Duration
	// PollInterval is how long the adapter waits between two status reads while
	// waiting for a container. The platform answers in well under a second, so
	// this is politeness rather than necessity.
	PollInterval time.Duration
}

// Adapter implements partner.PartnerAdapter for Alaya NeW CCI. Semantics:
//
//   - An instance with status RUNNING holds a lease; it is also offered as
//     available capacity, which satisfies the snapshot completeness rule
//     (available set must be a subset of the lease view).
//   - Endpoints come from the platform port mapping of the container ports.
type Adapter struct {
	partnerID string
	client    *Client
	cfg       Config
}

// New builds an adapter.
func New(partnerID string, cfg Config) (*Adapter, error) {
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("alayanew adapter requires access_key and secret_key")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.WaitTimeout <= 0 {
		cfg.WaitTimeout = 30 * time.Minute
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 15 * time.Second
	}
	if cfg.BootstrapPort == 0 {
		cfg.BootstrapPort = 9001
	}
	if cfg.ServicePort == 0 {
		cfg.ServicePort = 9002
	}
	return &Adapter{
		partnerID: partnerID,
		client:    NewClient(cfg.AccessKey, cfg.SecretKey, cfg.Timeout),
		cfg:       cfg,
	}, nil
}

// PartnerID implements partner.PartnerAdapter.
func (a *Adapter) PartnerID() string { return a.partnerID }

// ListAvailableInstances implements partner.PartnerAdapter. On this platform
// every running container is idle capacity we pay for.
func (a *Adapter) ListAvailableInstances(ctx context.Context) ([]partner.CapacityInstance, error) {
	return a.listRunning(ctx)
}

// ListActiveLeases implements partner.PartnerAdapter.
func (a *Adapter) ListActiveLeases(ctx context.Context) ([]partner.CapacityInstance, error) {
	return a.listRunning(ctx)
}

// leaseIDFor is the lease identity the pull snapshot publishes for one
// container. Sharing it with ProvisionIdentity keeps on-demand creation and
// the poll describing the same node.
func leaseIDFor(name string) string { return "cci-" + name }

// ProvisionIdentity implements partner.InstanceIdentity. The snapshot keys
// containers by the operator-chosen name (the platform UUID would read as a
// brand new container on every poll and revoke the managed node instead), so
// the enrolment must use the name too, together with the matching lease.
func (a *Adapter) ProvisionIdentity(name string) (string, string) {
	return name, leaseIDFor(name)
}

// ReleaseInstance implements partner.PartnerAdapter.
func (a *Adapter) ReleaseInstance(ctx context.Context, instanceID string) error {
	uuid, err := a.resolveUUID(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("release %s: %w", instanceID, err)
	}
	if err := a.client.releaseInstance(ctx, uuid); err != nil {
		return fmt.Errorf("release %s: %w", instanceID, err)
	}
	return nil
}

// resolveUUID maps the control-plane identity (the operator-chosen instance
// name, or an already-UUID instance from the provision flow) to the platform
// instance ID that mutation endpoints require.
//
// A miss is reported as partner.ErrInstanceNotFound rather than as a plain
// error: the list call succeeded and simply does not mention the container, so
// the caller learns a fact about the world instead of a transient failure.
func (a *Adapter) resolveUUID(ctx context.Context, nameOrID string) (string, error) {
	records, err := a.client.listInstances(ctx, a.cfg.AIDCId, "")
	if err != nil {
		return "", err
	}
	for _, record := range records {
		if record.Name == nameOrID || record.ID == nameOrID {
			return record.ID, nil
		}
	}
	return "", fmt.Errorf("instance %s: %w", nameOrID, partner.ErrInstanceNotFound)
}

// awaitingCapacity reports whether an instance status means "the platform
// accepted the request but has not placed the container anywhere yet". No
// container exists in these states, so no capacity is billed and the only asset
// the request holds is its place in the scheduling queue.
//
// An unrecognised status is deliberately NOT treated as queued: a status we do
// not know may well describe a container that already exists and is being
// billed, and leaking that costs money, while holding a queue position costs
// nothing. Unknown states therefore keep the strict behaviour of a timeout that
// releases.
func awaitingCapacity(status string) bool {
	switch status {
	case "QUEUED", "PENDING", "SCHEDULING":
		return true
	default:
		return false
	}
}

// AwaitSSHAccess implements partner.ProvisioningAdapter: poll the instance
// until RUNNING, then fetch the SSH connection details.
//
// The wait budget covers exactly one situation: a container that exists and is
// taking too long to come up. It deliberately does not cover "the platform has
// not placed us yet". A queued request has no container, so nothing is being
// billed and the only thing a local timeout could reclaim is the queue
// position - the very thing the request was made for. Releasing there would
// quietly move us to the back of the line, which is how a waiting provision
// turns into a failed one. So the budget starts only once the instance leaves
// the queued states, and a wait that ends while still queued reports
// ErrAwaitingCapacity so the caller keeps the request.
func (a *Adapter) AwaitSSHAccess(ctx context.Context, instanceID string) (partner.SSHAccess, error) {
	var deadline time.Time
	var lastStatus string
	// seenContainer records whether the platform ever described a container
	// that exists. Until it does, we have no evidence that anything can be
	// reclaimed, so failures report ErrAwaitingCapacity and the caller leaves
	// the request alone.
	seenContainer := false
	for {
		status, err := a.client.instanceStatus(ctx, instanceID)
		if err != nil {
			if !seenContainer {
				return partner.SSHAccess{}, fmt.Errorf("instance %s status unreadable: %w: %w", instanceID, partner.ErrAwaitingCapacity, err)
			}
			return partner.SSHAccess{}, err
		}
		lastStatus = status
		if status == "RUNNING" {
			break
		}
		if status == "FAILED" || status == "ERROR" {
			return partner.SSHAccess{}, fmt.Errorf("instance %s reached %s", instanceID, status)
		}
		switch {
		case awaitingCapacity(status):
			// Keep the budget stopped for as long as the container is absent.
			deadline = time.Time{}
		case deadline.IsZero():
			seenContainer = true
			deadline = time.Now().Add(a.cfg.WaitTimeout)
		case time.Now().After(deadline):
			return partner.SSHAccess{}, fmt.Errorf("instance %s not RUNNING within %s (last: %s)", instanceID, a.cfg.WaitTimeout, status)
		}
		select {
		case <-ctx.Done():
			if awaitingCapacity(lastStatus) {
				return partner.SSHAccess{}, fmt.Errorf("instance %s is still %s: %w", instanceID, lastStatus, partner.ErrAwaitingCapacity)
			}
			return partner.SSHAccess{}, ctx.Err()
		case <-time.After(a.cfg.PollInterval):
		}
	}
	info, err := a.client.getSSH(ctx, instanceID)
	if err != nil {
		return partner.SSHAccess{}, err
	}
	access, err := parseSSHURL(info.URL)
	if err != nil {
		return partner.SSHAccess{}, err
	}
	access.Password = info.Password
	return access, nil
}

// parseSSHURL splits "ssh user@host -p port" as returned by get-ssh.
func parseSSHURL(url string) (partner.SSHAccess, error) {
	fields := strings.Fields(url)
	if len(fields) != 4 || fields[0] != "ssh" || fields[2] != "-p" {
		return partner.SSHAccess{}, fmt.Errorf("unrecognized ssh url %q", url)
	}
	userHost := strings.SplitN(fields[1], "@", 2)
	if len(userHost) != 2 || userHost[0] == "" || userHost[1] == "" {
		return partner.SSHAccess{}, fmt.Errorf("unrecognized user@host in %q", url)
	}
	port, err := strconv.Atoi(fields[3])
	if err != nil || port <= 0 || port > 65535 {
		return partner.SSHAccess{}, fmt.Errorf("invalid port in %q", url)
	}
	return partner.SSHAccess{User: userHost[0], Host: userHost[1], Port: port}, nil
}

// CreateInstance provisions a new CCI container and waits until it is RUNNING.
// The optional provisioning capability is discovered via a type assertion on
// the adapter; the read-mostly PartnerAdapter contract stays untouched.
func (a *Adapter) CreateInstance(ctx context.Context, name string) (string, error) {
	if a.cfg.ProductCode == "" || a.cfg.Image == "" {
		return "", errors.New("alayanew creation requires product_code and image")
	}
	cciParams := map[string]any{
		"name":        name,
		"image":       a.cfg.Image,
		"description": "managed by tai-talea",
	}
	if len(a.cfg.Env) > 0 {
		cciParams["env"] = a.cfg.Env
	}
	if a.cfg.ComputeResource != "" {
		cciParams["computeResourceType"] = a.cfg.ComputeResource
	}
	if a.cfg.Priority != nil {
		cciParams["priority"] = *a.cfg.Priority
	}
	if a.cfg.NASStorageID != "" {
		cciParams["storageConfigEnable"] = true
		cciParams["storageConfigs"] = []map[string]any{{
			"storageId":     a.cfg.NASStorageID,
			"storageType":   "nas-capacity",
			"fileDirectory": a.cfg.NASFileDirectory,
			"mountPath":     a.cfg.NASMountPath,
			"onlyRead":      a.cfg.NASReadOnly,
		}}
	}
	key, err := a.client.createInstance(ctx, a.cfg.ProductCode, cciParams)
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.Status == statusNameInUse {
			// A taken name is not a failure of the request but of the name:
			// the platform never hands the name of a released container back.
			// Nothing has been allocated, so the caller retries under a fresh
			// name at no cost.
			return "", fmt.Errorf("create instance %s: %w: %v", name, partner.ErrNameInUse, err)
		}
		return "", fmt.Errorf("create instance %s: %w", name, err)
	}
	return key, nil
}

// WaitCreation polls one subscription key until it resolves. It returns the
// created instance id, or an error when the platform reports FAILED.
func (a *Adapter) WaitCreation(ctx context.Context, key string) (string, error) {
	deadline := time.Now().Add(a.cfg.WaitTimeout)
	for {
		result, err := a.client.subscriptionResultOnce(ctx, key)
		if err != nil {
			return "", err
		}
		switch result.Result {
		case "SUCCESS":
			if result.InstanceID == "" {
				return "", errors.New("alayanew creation succeeded without instanceId")
			}
			return result.InstanceID, nil
		case "FAILED":
			return "", fmt.Errorf("alayanew creation failed: %s", result.Message)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("alayanew creation %s not resolved within %s", key, a.cfg.WaitTimeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(a.cfg.PollInterval):
		}
	}
}

// listRunning converts RUNNING instances into capacity instances.
func (a *Adapter) listRunning(ctx context.Context) ([]partner.CapacityInstance, error) {
	records, err := a.client.listInstances(ctx, a.cfg.AIDCId, "RUNNING")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", partner.ErrPartnerUnavailable, err)
	}
	instances := make([]partner.CapacityInstance, 0, len(records))
	for _, record := range records {
		instance, err := a.toCapacityInstance(ctx, record)
		if err != nil {
			// One unreachable instance must not poison the whole snapshot:
			// skip it and let the next poll reconcile.
			slog.Warn("alayanew: skip instance without usable port mapping", "instance", record.ID, "error", err)
			continue
		}
		instances = append(instances, instance)
	}
	return instances, nil
}

// advertiseHost picks the address peers should use for one container. The
// static operator mapping wins; otherwise the control plane is asked for the
// address it recorded when it installed the node, and an unknown container
// keeps the address the platform published.
func (a *Adapter) advertiseHost(name string) string {
	if host, ok := a.cfg.AdvertiseHosts[name]; ok && host != "" {
		return host
	}
	if a.cfg.AdvertiseResolver == nil {
		return ""
	}
	return a.cfg.AdvertiseResolver(name)
}

func (a *Adapter) toCapacityInstance(ctx context.Context, record InstanceRecord) (partner.CapacityInstance, error) {
	mapped, err := a.client.portMappings(ctx, record.ID)
	if err != nil {
		return partner.CapacityInstance{}, err
	}
	bootstrap, ok := mapped[a.cfg.BootstrapPort]
	if !ok {
		return partner.CapacityInstance{}, fmt.Errorf("bootstrap port %d not published", a.cfg.BootstrapPort)
	}
	// The operator-chosen instance name is the identity the control plane
	// enrolled through onboarding. The platform UUID would look like a brand
	// new container on every poll and revoke the managed node instead.
	name := record.Name
	if name == "" {
		name = record.ID
	}
	// The platform publishes the public ingress host, but Router health
	// checks and PD disaggregation originate from inside the same network,
	// where the public IP is a hairpin that never completes. Swap in the
	// configured internal address and the container port (the published
	// ingress port maps 1:1 to the container port, so inside the network
	// peers reach the container port directly).
	rewrite := func(endpoint string, containerPort int) string {
		host := a.advertiseHost(name)
		if host == "" {
			return endpoint
		}
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" {
			return endpoint
		}
		u.Host = net.JoinHostPort(host, strconv.Itoa(containerPort))
		return u.String()
	}
	instance := partner.CapacityInstance{
		ID:       name,
		LeaseID:  leaseIDFor(name),
		Endpoint: rewrite(bootstrap, a.cfg.BootstrapPort),
		Spec: domain.InstanceSpec{
			GPU:          record.Resource.GPUName,
			GPUCount:     int(record.Resource.GPUCount),
			ModelSupport: a.cfg.ModelSupport,
		},
	}
	if service, ok := mapped[a.cfg.ServicePort]; ok {
		instance.ServiceEndpoint = rewrite(service, a.cfg.ServicePort)
	}
	return instance, nil
}
