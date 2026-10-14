// Package kubernetes implements a PartnerAdapter for one Kubernetes namespace.
//
// It exists because a container platform may offer no login at all. On the
// target cluster the tenant may create and delete Pods and read their logs, and
// nothing else: no exec, no port-forward, no Service, no ConfigMap, no
// PersistentVolumeClaim, and no outbound network. SSH provisioning is therefore
// impossible, and the only way a worker can be prepared is for the image to
// carry the bootstrap and start it on its own.
//
// That is the whole difference from the alayanew adapter: capacity creation and
// node preparation collapse into one step, and the endpoints are read off the
// Pod rather than derived from a port mapping. The control plane must run
// inside the same cluster, because Pod IPs are not routable from outside it.
package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/partner"
)

// Environment variables the container entrypoint reads. They are part of the
// deploy/container contract, not an invention of this adapter.
const (
	EnvBootstrapToken = "TAI_TALEA_BOOTSTRAP_TOKEN"
	EnvBootstrapPort  = "TAI_TALEA_BOOTSTRAP_PORT"
	EnvServicePort    = "TAI_TALEA_SERVICE_PORT"
	EnvProfile        = "TAI_TALEA_PROFILE"
)

// VolumeMountConfig mounts one node path into the worker container. Model
// weights arrive this way: the namespace has no PersistentVolumeClaim
// permission, so a host path is the only route that exists.
type VolumeMountConfig struct {
	Name      string
	HostPath  string
	MountPath string
	ReadOnly  bool
}

// NodeProfile is one machine a worker may be placed on.
//
// A GPU cluster is rarely homogeneous. The two H100 nodes this adapter was
// first pointed at hold different generations of HCA, and one of the cards on
// the first node has an active link to the peer that carries no traffic. That
// is a property of the machine, so it is expressed here rather than in the
// control plane's global environment: NCCL_IB_HCA selects the cards before the
// first collective runs, and a worker that started with the wrong list fails
// in a way that looks like a model problem.
type NodeProfile struct {
	// Host is the node's kubernetes.io/hostname value.
	Host string
	// Env is merged over the partner's env for Pods placed on this node.
	Env map[string]string
	// Selector overrides the partner's node selector when set. Empty pins the
	// Pod by kubernetes.io/hostname: Host.
	Selector map[string]string
	// ExtraSGLangArgs are the sglang flags this machine's hardware requires,
	// such as --disaggregation-ib-device. They travel with the instance rather
	// than with its role, because the machine is chosen by the creation
	// rotation and the role by the planner, and neither knows the other's
	// decision.
	ExtraSGLangArgs []string
}

// Config is the wiring for one Kubernetes namespace.
type Config struct {
	// Kubeconfig is the path to a kubeconfig file. Empty means in-cluster:
	// the projected service account token under /var/run/secrets.
	Kubeconfig string
	// Context selects the kubeconfig context; empty uses current-context.
	Context string
	// Namespace is the tenant namespace. It overrides the kubeconfig value.
	Namespace string

	Image           string
	ImagePullPolicy string
	// Entrypoint is the container command that starts the bootstrap. The
	// default matches deploy/container/entrypoint.sh.
	Entrypoint []string

	// BootstrapToken is the shared secret injected into the container. The
	// bootstrap refuses to serve without it.
	BootstrapToken string
	BootstrapPort  int
	ServicePort    int
	ProfilePath    string

	// GPUResource is the extended resource name, nvidia.com/gpu by default.
	GPUResource string
	GPUCount    int
	// NodeSelector pins the workers to a GPU node pool.
	NodeSelector map[string]string
	// Nodes are the machines workers may be placed on, in rotation order. It
	// is empty for a namespace that has one suitable node, in which case
	// NodeSelector decides. When it is set, each created Pod takes the next
	// entry, so a request for two workers lands one on each machine instead of
	// stacking both on the first.
	Nodes []NodeProfile

	// Volumes are node paths mounted into the container.
	Volumes []VolumeMountConfig
	// ShmSize replaces /dev/shm with a memory-backed volume. Empty keeps the
	// container default, which is 64 MB and too small for tensor parallelism.
	ShmSize string

	// HostNetwork / HostIPC / Privileged are what an H100 worker needs to
	// reach the fabric. The RDMA devices under /dev/infiniband are only
	// visible in the host network namespace, SGLang's ranks exchange CUDA IPC
	// handles that a private IPC namespace hides, and the GPU device nodes sit
	// behind a device cgroup that only a privileged container opens. All three
	// default to false: a cluster that provisions CPU workers, or that runs
	// its own device plugin, is not asked to weaken its isolation.
	HostNetwork bool
	HostIPC     bool
	Privileged  bool

	// LivenessProbe asks the kubelet to restart a worker that is alive but has
	// stopped answering. It is off by default because the probe it installs
	// requires a worker image whose bootstrap serves /bootstrap/liveness: an
	// image that predates that endpoint answers 404, and the container is then
	// restarted in a loop without ever serving. Turn it on for an image that
	// has the endpoint; the timing is fixed in manifest.go and cannot kill a
	// healthy worker.
	LivenessProbe bool

	ServiceAccount string
	// RunAsUser / RunAsGroup / FSGroup are optional. Leaving RunAsGroup unset
	// matters: overriding the image's group drops the supplementary video
	// group that grants access to the GPU device nodes.
	RunAsUser  *int64
	RunAsGroup *int64
	FSGroup    *int64

	Env          map[string]string
	ModelSupport []string

	// Timeout bounds one API call; WaitTimeout bounds waiting for a created
	// Pod to reach Running. Image pulls and CUDA initialisation are slow.
	Timeout     time.Duration
	WaitTimeout time.Duration
}

// Adapter implements partner.PartnerAdapter and partner.SelfProvisioningAdapter.
type Adapter struct {
	partnerID string
	cfg       Config
	client    *Client
	// nodeCursor is the rotation position over Config.Nodes. It advances once
	// per created Pod. atomic, because provisioning requests from different
	// partners arrive on separate goroutines.
	nodeCursor atomic.Uint64
}

// nextNode returns the node the next worker should be placed on, or nil when
// the adapter is configured for a single node.
func (a *Adapter) nextNode() *NodeProfile {
	if len(a.cfg.Nodes) == 0 {
		return nil
	}
	// Add returns the post-increment value, so the first Pod takes index 0.
	index := a.nodeCursor.Add(1) - 1
	return &a.cfg.Nodes[index%uint64(len(a.cfg.Nodes))]
}

// nodeProfileFor returns the profile of the machine a Pod is scheduled on, or
// nil when no profile matches.
//
// The answer comes from the Pod's spec.nodeName rather than from the rotation
// cursor, because only the scheduler knows where the Pod actually went: a
// control-plane restart resets the cursor, and an adopted Pod - the path taken
// when a creation is retried under a name that already exists - never advanced
// it at all. Guessing from the cursor would hand such a Pod the other
// machine's hardware flags, which is the failure this whole field exists to
// prevent.
func (a *Adapter) nodeProfileFor(host string) *NodeProfile {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil
	}
	for index := range a.cfg.Nodes {
		if a.cfg.Nodes[index].Host == host {
			return &a.cfg.Nodes[index]
		}
	}
	return nil
}

// hardwareArgsFor is the per-machine sglang override for a Pod, empty when the
// namespace is single-node or the node is not in the rotation list.
func (a *Adapter) hardwareArgsFor(pod Pod) []string {
	profile := a.nodeProfileFor(pod.Spec.NodeName)
	if profile == nil {
		return nil
	}
	return profile.ExtraSGLangArgs
}

// New builds an adapter for one namespace.
func New(partnerID string, cfg Config) (*Adapter, error) {
	if !validLabelValue(partnerID) {
		return nil, errors.New("kubernetes adapter requires a label-safe partner id")
	}
	if strings.TrimSpace(cfg.Image) == "" {
		return nil, errors.New("kubernetes adapter requires an image")
	}
	if strings.TrimSpace(cfg.BootstrapToken) == "" {
		return nil, errors.New("kubernetes adapter requires the container bootstrap token")
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
	if cfg.GPUResource == "" {
		cfg.GPUResource = "nvidia.com/gpu"
	}
	if len(cfg.Entrypoint) == 0 {
		cfg.Entrypoint = []string{"/opt/tai-talea/entrypoint.sh", "serve"}
	}
	// A node profile without a host cannot be pinned, and a duplicate host
	// would rotate two workers onto one machine while a second sits idle.
	// Both mistakes are configurations that start cleanly and then surprise
	// the operator much later, so they are refused here.
	seenNodes := make(map[string]bool, len(cfg.Nodes))
	for index := range cfg.Nodes {
		host := strings.TrimSpace(cfg.Nodes[index].Host)
		if host == "" {
			return nil, errors.New("kubernetes adapter requires a host on every node profile")
		}
		if seenNodes[host] {
			return nil, fmt.Errorf("kubernetes adapter lists node %q twice", host)
		}
		seenNodes[host] = true
	}

	creds, err := loadCredentials(cfg.Kubeconfig, cfg.Context)
	if err != nil {
		return nil, err
	}
	if ns := strings.TrimSpace(cfg.Namespace); ns != "" {
		creds.namespace = ns
	}
	if creds.namespace == "" {
		return nil, errors.New("kubernetes adapter requires a namespace (set kubernetes.namespace or a kubeconfig context)")
	}
	cfg.Namespace = creds.namespace

	client, err := NewClient(creds, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	return &Adapter{partnerID: partnerID, cfg: cfg, client: client}, nil
}

// PartnerID implements partner.PartnerAdapter.
func (a *Adapter) PartnerID() string { return a.partnerID }

// ListAvailableInstances implements partner.PartnerAdapter. Every running
// worker in the namespace is capacity this control plane pays for and holds.
func (a *Adapter) ListAvailableInstances(ctx context.Context) ([]partner.CapacityInstance, error) {
	return a.listRunning(ctx)
}

// ListActiveLeases implements partner.PartnerAdapter.
func (a *Adapter) ListActiveLeases(ctx context.Context) ([]partner.CapacityInstance, error) {
	return a.listRunning(ctx)
}

// ReleaseInstance implements partner.PartnerAdapter by deleting the Pod, which
// is the only reclaim primitive the namespace offers.
func (a *Adapter) ReleaseInstance(ctx context.Context, instanceID string) error {
	if err := a.client.DeletePod(ctx, instanceID); err != nil {
		return fmt.Errorf("release %s: %w", instanceID, err)
	}
	return nil
}

// ProvisionNode implements partner.SelfProvisioningAdapter: it creates one Pod
// and waits for the bootstrap inside it to become reachable. There is no SSH
// step, because the namespace does not grant a login.
func (a *Adapter) ProvisionNode(ctx context.Context, name string) (partner.ProvisionedNode, error) {
	if !validName(name) {
		return partner.ProvisionedNode{}, fmt.Errorf("%q is not a valid pod name", name)
	}
	created, err := a.client.CreatePod(ctx, a.buildPod(name))
	if err != nil {
		if !IsAlreadyExists(err) {
			return partner.ProvisionedNode{}, fmt.Errorf("create pod %s: %w", name, err)
		}
		// A repeated request for the same name adopts the Pod already there,
		// but only when it is ours: two partners may share one namespace.
		existing, getErr := a.client.GetPod(ctx, name)
		if getErr != nil {
			return partner.ProvisionedNode{}, fmt.Errorf("create pod %s: %w", name, err)
		}
		if existing.Metadata.Labels[LabelPartner] != a.partnerID {
			return partner.ProvisionedNode{}, fmt.Errorf("pod %s already exists and belongs to another partner", name)
		}
		created = existing
		slog.Info("kubernetes: adopting existing pod", "partner", a.partnerID, "pod", name)
	}
	if created.Metadata.Name == "" {
		created.Metadata.Name = name
	}

	running, err := a.waitRunning(ctx, name)
	if err != nil {
		return partner.ProvisionedNode{}, err
	}
	return a.toProvisioned(running), nil
}

// waitRunning blocks until the Pod has an IP and is running.
func (a *Adapter) waitRunning(ctx context.Context, name string) (Pod, error) {
	deadline := time.Now().Add(a.cfg.WaitTimeout)
	for {
		pod, err := a.client.GetPod(ctx, name)
		if err != nil {
			if IsNotFound(err) {
				return Pod{}, fmt.Errorf("pod %s disappeared while waiting for it to start", name)
			}
			return Pod{}, err
		}
		if pod.Status.Phase == PodRunning && pod.Status.PodIP != "" {
			return pod, nil
		}
		if pod.Status.Phase == PodFailed || pod.Status.Phase == PodSucceeded {
			return Pod{}, fmt.Errorf("pod %s reached %s: %s", name, pod.Status.Phase, podDetail(pod))
		}
		if time.Now().After(deadline) {
			return Pod{}, fmt.Errorf("pod %s not running within %s (%s)", name, a.cfg.WaitTimeout, podDetail(pod))
		}
		select {
		case <-ctx.Done():
			return Pod{}, ctx.Err()
		case <-time.After(waitPollInterval):
		}
	}
}

// waitPollInterval is how often a created Pod is re-read while it starts. It is
// a variable so tests do not have to wait whole seconds.
var waitPollInterval = 5 * time.Second

// listRunning converts the namespace's running Pods into capacity instances.
func (a *Adapter) listRunning(ctx context.Context) ([]partner.CapacityInstance, error) {
	pods, err := a.client.ListPods(ctx, a.selector())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", partner.ErrPartnerUnavailable, err)
	}
	instances := make([]partner.CapacityInstance, 0, len(pods))
	for _, pod := range pods {
		instance, err := a.toCapacityInstance(pod)
		if err != nil {
			// One Pod without an address must not poison the snapshot: skip it
			// and let the next poll reconcile.
			slog.Warn("kubernetes: skip pod without a usable address", "partner", a.partnerID, "pod", pod.Metadata.Name, "error", err)
			continue
		}
		instances = append(instances, instance)
	}
	return instances, nil
}

func (a *Adapter) toCapacityInstance(pod Pod) (partner.CapacityInstance, error) {
	if pod.Metadata.DeletionTimestamp != nil {
		return partner.CapacityInstance{}, errors.New("pod is terminating")
	}
	if pod.Status.Phase != PodRunning || pod.Status.PodIP == "" {
		return partner.CapacityInstance{}, fmt.Errorf("pod is %s without an address", pod.Status.Phase)
	}
	// The name is written into the labels at creation and parsed back here, so
	// a Pod created by hand under our labels cannot claim a foreign identity.
	name := pod.Metadata.Name
	return partner.CapacityInstance{
		ID:              name,
		Endpoint:        a.endpoint(pod.Status.PodIP, a.cfg.BootstrapPort),
		ServiceEndpoint: a.endpoint(pod.Status.PodIP, a.cfg.ServicePort),
		// The UID, not the name: a recreated Pod is a different container and
		// the control plane must be able to tell the two apart.
		LeaseID: "k8s-" + pod.Metadata.UID,
		Spec: domain.InstanceSpec{
			GPU:                a.cfg.GPUResource,
			GPUCount:           a.cfg.GPUCount,
			ModelSupport:       a.cfg.ModelSupport,
			SGLangArgsOverride: a.hardwareArgsFor(pod),
		},
	}, nil
}

func (a *Adapter) toProvisioned(pod Pod) partner.ProvisionedNode {
	return partner.ProvisionedNode{
		ID:              pod.Metadata.Name,
		Endpoint:        a.endpoint(pod.Status.PodIP, a.cfg.BootstrapPort),
		ServiceEndpoint: a.endpoint(pod.Status.PodIP, a.cfg.ServicePort),
		LeaseID:         "k8s-" + pod.Metadata.UID,
		Spec: domain.InstanceSpec{
			GPU:                a.cfg.GPUResource,
			GPUCount:           a.cfg.GPUCount,
			ModelSupport:       a.cfg.ModelSupport,
			SGLangArgsOverride: a.hardwareArgsFor(pod),
		},
	}
}

// endpoint renders an in-cluster address. Workers reach each other over the Pod
// network, and the control plane runs inside the same cluster for exactly that
// reason: those addresses are not routable from outside.
func (a *Adapter) endpoint(ip string, port int) string {
	return "http://" + net.JoinHostPort(ip, strconv.Itoa(port))
}

// podDetail summarises why a Pod has not started, so the operator sees the
// cause in the provisioning record instead of an opaque timeout.
func podDetail(pod Pod) string {
	if pod.Status.Message != "" {
		return pod.Status.Message
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Status == "False" && condition.Message != "" {
			return condition.Type + ": " + condition.Message
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Waiting != nil && status.State.Waiting.Message != "" {
			return status.State.Waiting.Reason + ": " + status.State.Waiting.Message
		}
		if status.State.Terminated != nil {
			return fmt.Sprintf("%s: exit code %d %s", status.State.Terminated.Reason, status.State.Terminated.ExitCode, status.State.Terminated.Message)
		}
	}
	if pod.Status.Reason != "" {
		return pod.Status.Reason
	}
	return "no detail reported"
}

var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
var labelValue = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)

// validName reports whether the value is a Kubernetes object name. The Pod
// name becomes half of an instance identity, so it is validated before use.
func validName(value string) bool {
	return len(value) > 0 && len(value) <= 63 && dns1123.MatchString(value)
}

// validLabelValue reports whether the value can be used as a label value. The
// partner id travels as a label, and label values accept a wider character set
// than object names do.
func validLabelValue(value string) bool {
	return len(value) > 0 && len(value) <= 63 && labelValue.MatchString(value)
}
