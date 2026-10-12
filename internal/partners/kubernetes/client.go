package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxResponseBytes bounds what the API server can send back in one call.
const maxResponseBytes = 4 << 20

// APIError is a non-2xx status from the Kubernetes API server.
type APIError struct {
	StatusCode int
	Reason     string
	Message    string
}

func (e *APIError) Error() string {
	detail := e.Message
	if detail == "" {
		detail = e.Reason
	}
	if detail == "" {
		detail = http.StatusText(e.StatusCode)
	}
	return fmt.Sprintf("kubernetes api returned HTTP %d: %s", e.StatusCode, detail)
}

// IsNotFound reports whether the error is a missing object.
func IsNotFound(err error) bool { return hasStatus(err, http.StatusNotFound) }

// IsAlreadyExists reports whether the error is a duplicate create.
func IsAlreadyExists(err error) bool { return hasStatus(err, http.StatusConflict) }

func hasStatus(err error, code int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == code
}

// Client is a minimal Kubernetes REST client. It speaks only the Pod verbs the
// adapter needs; everything else belongs to kubectl.
type Client struct {
	server    string
	namespace string
	http      *http.Client
	token     string
	username  string
	password  string
}

// NewClient builds a client for one namespace using a resolved credential.
func NewClient(creds credentials, timeout time.Duration) (*Client, error) {
	tlsConfig, err := creds.tlsConfig()
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		TLSClientConfig:       tlsConfig,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		// The API server is reached directly. Honouring a proxy environment
		// variable here would silently route cluster traffic through a proxy
		// that cannot see it.
		Proxy: nil,
	}
	return &Client{
		server:    strings.TrimRight(creds.server, "/"),
		namespace: creds.namespace,
		http:      &http.Client{Timeout: timeout, Transport: transport},
		token:     creds.token,
		username:  creds.username,
		password:  creds.password,
	}, nil
}

// Namespace reports the namespace the client operates in.
func (c *Client) Namespace() string { return c.namespace }

// CreatePod submits one Pod and returns the stored object.
func (c *Client) CreatePod(ctx context.Context, pod Pod) (Pod, error) {
	var created Pod
	err := c.do(ctx, http.MethodPost, c.podsPath(), "", pod, &created)
	return created, err
}

// GetPod reads one Pod by name.
func (c *Client) GetPod(ctx context.Context, name string) (Pod, error) {
	var pod Pod
	err := c.do(ctx, http.MethodGet, c.podsPath()+"/"+url.PathEscape(name), "", nil, &pod)
	return pod, err
}

// ListPods reads the Pods matching a label selector.
func (c *Client) ListPods(ctx context.Context, labelSelector string) ([]Pod, error) {
	query := url.Values{}
	if labelSelector != "" {
		query.Set("labelSelector", labelSelector)
	}
	var list struct {
		Items []Pod `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, c.podsPath(), query.Encode(), nil, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// DeletePod removes one Pod. A missing Pod is not an error: the adapter is
// read-mostly and release has to stay idempotent.
func (c *Client) DeletePod(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, c.podsPath()+"/"+url.PathEscape(name), "", nil, nil)
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

func (c *Client) podsPath() string {
	return "/api/v1/namespaces/" + url.PathEscape(c.namespace) + "/pods"
}

func (c *Client) do(ctx context.Context, method, path, query string, body any, out any) error {
	if c.namespace == "" {
		return errors.New("kubernetes adapter requires a namespace")
	}
	target := c.server + path
	if query != "" {
		target += "?" + query
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode kubernetes request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	} else if c.username != "" {
		request.SetBasicAuth(c.username, c.password)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(payload) > maxResponseBytes {
		return fmt.Errorf("%s %s: kubernetes response is too large", method, path)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var status struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(payload, &status)
		return &APIError{StatusCode: response.StatusCode, Reason: status.Reason, Message: status.Message}
	}
	if out == nil || len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("%s %s: decode kubernetes response: %w", method, path, err)
	}
	return nil
}

// ---------------------------------------------------------------- wire types

// Pod is the subset of the core/v1 Pod schema the adapter reads and writes.
type Pod struct {
	APIVersion string     `json:"apiVersion,omitempty"`
	Kind       string     `json:"kind,omitempty"`
	Metadata   ObjectMeta `json:"metadata,omitempty"`
	Spec       PodSpec    `json:"spec,omitempty"`
	Status     PodStatus  `json:"status,omitempty"`
}

// ObjectMeta carries the identity and ownership labels of a Pod.
type ObjectMeta struct {
	Name        string            `json:"name,omitempty"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	UID         string            `json:"uid,omitempty"`
	// DeletionTimestamp is set once a delete has been accepted but the Pod is
	// still terminating. Such a Pod must not be offered as capacity.
	DeletionTimestamp *string `json:"deletionTimestamp,omitempty"`
}

// PodSpec is the Pod scheduling and container shape.
type PodSpec struct {
	Containers                    []Container         `json:"containers"`
	NodeSelector                  map[string]string   `json:"nodeSelector,omitempty"`
	RestartPolicy                 string              `json:"restartPolicy,omitempty"`
	ServiceAccountName            string              `json:"serviceAccountName,omitempty"`
	TerminationGracePeriodSeconds *int64              `json:"terminationGracePeriodSeconds,omitempty"`
	SecurityContext               *PodSecurityContext `json:"securityContext,omitempty"`
	Volumes                       []Volume            `json:"volumes,omitempty"`
	// NodeName is read back from a scheduled Pod, never written on creation:
	// it is how the adapter learns which machine a worker actually landed on,
	// so the per-machine sglang flags can follow the Pod instead of the role.
	// Reading the scheduler's answer beats re-deriving it from the rotation
	// cursor, which a restart resets and which an adopted Pod never advanced.
	NodeName string `json:"nodeName,omitempty"`
	// HostNetwork and HostIPC put the container into the node's network and
	// IPC namespaces. An RDMA worker needs both: the InfiniBand character
	// devices are addressed by the host's device names, and SGLang's tensor
	// parallel ranks exchange CUDA IPC handles that a private IPC namespace
	// hides. Both default to false so a cluster that does not ask for them
	// keeps the least-privilege shape.
	HostNetwork bool `json:"hostNetwork,omitempty"`
	HostIPC     bool `json:"hostIPC,omitempty"`
	// DNSPolicy has to travel with HostNetwork. ClusterFirst, the default, is
	// rejected for a host-network Pod: the kubelet refuses to program it and
	// silently falls back to the node resolver, which cannot see cluster
	// names.
	DNSPolicy string `json:"dnsPolicy,omitempty"`
}

// Container is one container of the Pod.
type Container struct {
	Name            string               `json:"name"`
	Image           string               `json:"image"`
	ImagePullPolicy string               `json:"imagePullPolicy,omitempty"`
	Command         []string             `json:"command,omitempty"`
	Args            []string             `json:"args,omitempty"`
	Env             []EnvVar             `json:"env,omitempty"`
	Ports           []ContainerPort      `json:"ports,omitempty"`
	Resources       ResourceRequirements `json:"resources,omitempty"`
	VolumeMounts    []VolumeMount        `json:"volumeMounts,omitempty"`
	SecurityContext *SecurityContext     `json:"securityContext,omitempty"`
	ReadinessProbe  *Probe               `json:"readinessProbe,omitempty"`
}

// EnvVar is one environment variable.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ContainerPort declares a port the container listens on.
type ContainerPort struct {
	Name          string `json:"name,omitempty"`
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol,omitempty"`
}

// ResourceRequirements carries the GPU request. Kubernetes models extended
// resources as quantities, which travel as strings; a GPU may only be claimed
// through limits, never through requests.
type ResourceRequirements struct {
	Limits   map[string]string `json:"limits,omitempty"`
	Requests map[string]string `json:"requests,omitempty"`
}

// VolumeMount binds a volume into the container.
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// Volume is one Pod volume.
type Volume struct {
	Name     string          `json:"name"`
	HostPath *HostPathVolume `json:"hostPath,omitempty"`
	EmptyDir *EmptyDirVolume `json:"emptyDir,omitempty"`
}

// HostPathVolume mounts a node path. It is how model weights reach the
// container when the platform offers no PersistentVolumeClaim.
type HostPathVolume struct {
	Path string `json:"path"`
	Type string `json:"type,omitempty"`
}

// EmptyDirVolume is a scratch directory. With Medium "Memory" it becomes a
// tmpfs, which is what /dev/shm needs: the default 64 MB breaks every
// multi-process SGLang rank.
type EmptyDirVolume struct {
	Medium    string `json:"medium,omitempty"`
	SizeLimit string `json:"sizeLimit,omitempty"`
}

// PodSecurityContext is the Pod level security context.
type PodSecurityContext struct {
	RunAsUser  *int64 `json:"runAsUser,omitempty"`
	RunAsGroup *int64 `json:"runAsGroup,omitempty"`
	FSGroup    *int64 `json:"fsGroup,omitempty"`
}

// SecurityContext is the container level security context.
type SecurityContext struct {
	RunAsUser                *int64 `json:"runAsUser,omitempty"`
	RunAsGroup               *int64 `json:"runAsGroup,omitempty"`
	RunAsNonRoot             *bool  `json:"runAsNonRoot,omitempty"`
	AllowPrivilegeEscalation *bool  `json:"allowPrivilegeEscalation,omitempty"`
	// Privileged lifts the device cgroup restrictions. It is not a substitute
	// for hostNetwork and hostIPC: those change which namespaces the container
	// shares, this one only decides whether the devices behind them may be
	// opened.
	Privileged   *bool         `json:"privileged,omitempty"`
	Capabilities *Capabilities `json:"capabilities,omitempty"`
}

// Capabilities is the container capability set.
type Capabilities struct {
	Drop []string `json:"drop,omitempty"`
}

// Probe is a readiness probe.
type Probe struct {
	HTTPGet             *HTTPGetAction `json:"httpGet,omitempty"`
	InitialDelaySeconds int            `json:"initialDelaySeconds,omitempty"`
	PeriodSeconds       int            `json:"periodSeconds,omitempty"`
	TimeoutSeconds      int            `json:"timeoutSeconds,omitempty"`
	FailureThreshold    int            `json:"failureThreshold,omitempty"`
}

// HTTPGetAction probes an HTTP endpoint.
type HTTPGetAction struct {
	Path        string       `json:"path,omitempty"`
	Port        int          `json:"port"`
	HTTPHeaders []HTTPHeader `json:"httpHeaders,omitempty"`
}

// HTTPHeader is one probe request header.
type HTTPHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// PodStatus is the observed Pod state.
type PodStatus struct {
	Phase             string            `json:"phase,omitempty"`
	Reason            string            `json:"reason,omitempty"`
	Message           string            `json:"message,omitempty"`
	PodIP             string            `json:"podIP,omitempty"`
	HostIP            string            `json:"hostIP,omitempty"`
	Conditions        []PodCondition    `json:"conditions,omitempty"`
	ContainerStatuses []ContainerStatus `json:"containerStatuses,omitempty"`
}

// PodCondition is one condition of the Pod condition set.
type PodCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// ContainerState is the observed state of one container.
type ContainerState struct {
	Waiting    *ContainerStateWaiting    `json:"waiting,omitempty"`
	Running    *ContainerStateRunning    `json:"running,omitempty"`
	Terminated *ContainerStateTerminated `json:"terminated,omitempty"`
}

// ContainerStateWaiting means the container has not started yet.
type ContainerStateWaiting struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// ContainerStateRunning means the container is running.
type ContainerStateRunning struct {
	StartedAt string `json:"startedAt,omitempty"`
}

// ContainerStateTerminated means the container exited.
type ContainerStateTerminated struct {
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
	ExitCode int    `json:"exitCode"`
}

// ContainerStatus is the observed container state.
type ContainerStatus struct {
	Name         string         `json:"name"`
	Ready        bool           `json:"ready"`
	RestartCount int            `json:"restartCount"`
	State        ContainerState `json:"state"`
}

// Pod phases reported by the API server.
const (
	PodPending   = "Pending"
	PodRunning   = "Running"
	PodSucceeded = "Succeeded"
	PodFailed    = "Failed"
	PodUnknown   = "Unknown"
)
