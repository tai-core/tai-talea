package alayanew

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// BootstrapPort and ServicePort are the container ports published by CCI.
	BootstrapPort int
	ServicePort   int
	// Timeout bounds one signed request.
	Timeout time.Duration
	// WaitTimeout bounds waiting for a created instance to reach RUNNING.
	WaitTimeout time.Duration
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

// ReleaseInstance implements partner.PartnerAdapter.
func (a *Adapter) ReleaseInstance(ctx context.Context, instanceID string) error {
	if err := a.client.releaseInstance(ctx, instanceID); err != nil {
		return fmt.Errorf("release %s: %w", instanceID, err)
	}
	return nil
}

// AwaitSSHAccess implements partner.ProvisioningAdapter: poll the instance
// until RUNNING, then fetch the SSH connection details.
func (a *Adapter) AwaitSSHAccess(ctx context.Context, instanceID string) (partner.SSHAccess, error) {
	deadline := time.Now().Add(a.cfg.WaitTimeout)
	for {
		status, err := a.client.instanceStatus(ctx, instanceID)
		if err != nil {
			return partner.SSHAccess{}, err
		}
		if status == "RUNNING" {
			break
		}
		if status == "FAILED" || status == "ERROR" {
			return partner.SSHAccess{}, fmt.Errorf("instance %s reached %s", instanceID, status)
		}
		if time.Now().After(deadline) {
			return partner.SSHAccess{}, fmt.Errorf("instance %s not RUNNING within %s (last: %s)", instanceID, a.cfg.WaitTimeout, status)
		}
		select {
		case <-ctx.Done():
			return partner.SSHAccess{}, ctx.Err()
		case <-time.After(15 * time.Second):
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
		case <-time.After(10 * time.Second):
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
	instance := partner.CapacityInstance{
		ID:       name,
		LeaseID:  "cci-" + name,
		Endpoint: bootstrap,
		Spec: domain.InstanceSpec{
			GPU:          record.Resource.GPUName,
			GPUCount:     int(record.Resource.GPUCount),
			ModelSupport: a.cfg.ModelSupport,
		},
	}
	if service, ok := mapped[a.cfg.ServicePort]; ok {
		instance.ServiceEndpoint = service
	}
	return instance, nil
}
