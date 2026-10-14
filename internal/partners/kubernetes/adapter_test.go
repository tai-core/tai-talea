package kubernetes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/partner"
)

// fakeAPIServer stands in for the Kubernetes API server: it stores Pods in
// memory and answers the four verbs the adapter uses.
type fakeAPIServer struct {
	mu sync.Mutex
	// pendingGets is how many GETs a Pod stays Pending before it runs.
	pendingGets int
	pods        map[string]Pod
	gets        map[string]int
	deleted     []string
	server      *httptest.Server
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	f := &fakeAPIServer{pods: map[string]Pod{}, gets: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/namespaces/{ns}/pods", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var pod Pod
		if err := json.NewDecoder(r.Body).Decode(&pod); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, exists := f.pods[pod.Metadata.Name]; exists {
			writeKubeStatus(w, http.StatusConflict, "AlreadyExists", "pods \""+pod.Metadata.Name+"\" already exists")
			return
		}
		pod.Metadata.UID = "uid-" + pod.Metadata.Name
		pod.Status = PodStatus{Phase: PodPending}
		f.pods[pod.Metadata.Name] = pod
		writeJSON(w, http.StatusCreated, pod)
	})
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/pods", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		selector := r.URL.Query().Get("labelSelector")
		items := make([]Pod, 0, len(f.pods))
		for _, pod := range f.pods {
			if selector != "" && !matchesSelector(pod.Metadata.Labels, selector) {
				continue
			}
			items = append(items, pod)
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	})
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/pods/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		name := r.PathValue("name")
		pod, ok := f.pods[name]
		if !ok {
			writeKubeStatus(w, http.StatusNotFound, "NotFound", "pods \""+name+"\" not found")
			return
		}
		f.gets[name]++
		if f.gets[name] > f.pendingGets {
			pod.Status.Phase = PodRunning
			pod.Status.PodIP = "10.222.1.10"
		}
		f.pods[name] = pod
		writeJSON(w, http.StatusOK, pod)
	})
	mux.HandleFunc("DELETE /api/v1/namespaces/{ns}/pods/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		name := r.PathValue("name")
		if _, ok := f.pods[name]; !ok {
			writeKubeStatus(w, http.StatusNotFound, "NotFound", "pods \""+name+"\" not found")
			return
		}
		delete(f.pods, name)
		f.deleted = append(f.deleted, name)
		writeJSON(w, http.StatusOK, map[string]string{"status": "Success"})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// put inserts a Pod directly, as if it had been created out of band.
func (f *fakeAPIServer) put(pod Pod) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pod.Metadata.UID == "" {
		pod.Metadata.UID = "uid-" + pod.Metadata.Name
	}
	f.pods[pod.Metadata.Name] = pod
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeKubeStatus(w http.ResponseWriter, status int, reason, message string) {
	writeJSON(w, status, map[string]any{"kind": "Status", "reason": reason, "message": message, "code": status})
}

func matchesSelector(labels map[string]string, selector string) bool {
	for _, term := range strings.Split(selector, ",") {
		key, value, ok := strings.Cut(term, "=")
		if !ok || labels[key] != value {
			return false
		}
	}
	return true
}

const testToken = "0123456789abcdef0123456789abcdef"

func testAdapter(t *testing.T, f *fakeAPIServer, mutate func(*Config)) *Adapter {
	t.Helper()
	cfg := Config{
		Namespace:      "nightshift",
		Image:          "registry.local/talea-worker:test",
		Entrypoint:     []string{"/opt/tai-talea/entrypoint.sh", "serve"},
		BootstrapToken: testToken,
		BootstrapPort:  9001,
		ServicePort:    9002,
		GPUResource:    "nvidia.com/gpu",
		GPUCount:       8,
		ShmSize:        "512Gi",
		NodeSelector:   map[string]string{"gpu": "h100"},
		Volumes:        []VolumeMountConfig{{Name: "models", HostPath: "/apps", MountPath: "/apps", ReadOnly: true}},
		Timeout:        5 * time.Second,
		WaitTimeout:    5 * time.Second,
		ModelSupport:   []string{"zai-org/GLM-5.3-Flash"},
		ServiceAccount: "talea-worker",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	client, err := NewClient(credentials{server: f.server.URL, namespace: cfg.Namespace, token: "sa-token"}, cfg.Timeout)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	return &Adapter{partnerID: "cmcc-k8s", cfg: cfg, client: client}
}

func TestBuildPodKeepsGPUOutOfRequests(t *testing.T) {
	f := newFakeAPIServer(t)
	adapter := testAdapter(t, f, nil)

	pod := adapter.buildPod("glm53-prefill")
	container := pod.Spec.Containers[0]
	if got := container.Resources.Limits["nvidia.com/gpu"]; got != "8" {
		t.Fatalf("gpu limit = %q, want 8", got)
	}
	// A GPU requested without a limit is silently scheduled on no GPU at all.
	if len(container.Resources.Requests) != 0 {
		t.Fatalf("gpu must not appear in requests, got %v", container.Resources.Requests)
	}
	if pod.Spec.NodeSelector["gpu"] != "h100" {
		t.Fatalf("node selector lost: %v", pod.Spec.NodeSelector)
	}
}

func TestBuildPodReplacesDevShmAndMountsWeights(t *testing.T) {
	f := newFakeAPIServer(t)
	adapter := testAdapter(t, f, nil)
	pod := adapter.buildPod("glm53-prefill")

	var shm *Volume
	mounts := map[string]string{}
	for _, mount := range pod.Spec.Containers[0].VolumeMounts {
		mounts[mount.Name] = mount.MountPath
	}
	for index := range pod.Spec.Volumes {
		if pod.Spec.Volumes[index].Name == shmVolumeName {
			shm = &pod.Spec.Volumes[index]
		}
	}
	if shm == nil || shm.EmptyDir == nil {
		t.Fatalf("no /dev/shm volume in %v", pod.Spec.Volumes)
	}
	if shm.EmptyDir.Medium != "Memory" {
		t.Fatalf("shm medium = %q, want Memory", shm.EmptyDir.Medium)
	}
	if mounts[shmVolumeName] != "/dev/shm" {
		t.Fatalf("shm mount = %q", mounts[shmVolumeName])
	}
	// Weights are read from a node path; there is no PVC permission to use.
	var models *Volume
	for index := range pod.Spec.Volumes {
		if pod.Spec.Volumes[index].Name == "models" {
			models = &pod.Spec.Volumes[index]
		}
	}
	if models == nil || models.HostPath == nil || models.HostPath.Path != "/apps" {
		t.Fatalf("weight volume missing: %v", pod.Spec.Volumes)
	}
	if mounts["models"] != "/apps" {
		t.Fatalf("weight mount = %q", mounts["models"])
	}
}

func TestBuildPodInjectsTokenAndLeavesGroupAlone(t *testing.T) {
	f := newFakeAPIServer(t)
	adapter := testAdapter(t, f, nil)
	pod := adapter.buildPod("glm53-decode")

	env := map[string]string{}
	for _, variable := range pod.Spec.Containers[0].Env {
		env[variable.Name] = variable.Value
	}
	if env[EnvBootstrapToken] != testToken {
		t.Fatalf("bootstrap token not injected: %v", env)
	}
	if env[EnvServicePort] != "9002" {
		t.Fatalf("service port not injected: %v", env)
	}
	// Overriding the group drops the supplementary video group the image
	// relies on to reach the GPU device nodes.
	if pod.Spec.Containers[0].SecurityContext.RunAsGroup != nil {
		t.Fatal("runAsGroup must stay unset unless configured")
	}
	if pod.Spec.SecurityContext != nil {
		t.Fatal("pod security context must stay absent when nothing is configured")
	}
	if pod.Metadata.Labels[LabelPartner] != "cmcc-k8s" || pod.Metadata.Labels[LabelManagedBy] != managedByValue {
		t.Fatalf("ownership labels missing: %v", pod.Metadata.Labels)
	}
}

func TestBuildPodGrantsHostAccessOnlyWhenConfigured(t *testing.T) {
	f := newFakeAPIServer(t)

	// The default shape is the least-privilege one. A cluster that provisions
	// CPU workers, or that runs its own device plugin, must not be handed a
	// privileged host-network Pod just because the adapter learned the flags.
	plain := testAdapter(t, f, nil).buildPod("glm53-decode")
	if plain.Spec.HostNetwork || plain.Spec.HostIPC {
		t.Fatalf("host namespaces must stay off by default: %+v", plain.Spec)
	}
	if plain.Spec.DNSPolicy != "" {
		t.Fatalf("dns policy must stay unset without host network, got %q", plain.Spec.DNSPolicy)
	}
	if plain.Spec.Containers[0].SecurityContext.Privileged != nil {
		t.Fatal("privileged must stay off by default")
	}

	host := testAdapter(t, f, func(cfg *Config) {
		cfg.HostNetwork = true
		cfg.HostIPC = true
		cfg.Privileged = true
	}).buildPod("glm53-decode")
	if !host.Spec.HostNetwork || !host.Spec.HostIPC {
		t.Fatalf("host namespaces not requested: %+v", host.Spec)
	}
	// ClusterFirst, the default, is not valid for a host-network Pod: the
	// kubelet rejects it and falls back to the node resolver, which sees no
	// cluster names at all.
	if host.Spec.DNSPolicy != dnsClusterFirstWithHostNet {
		t.Fatalf("dns policy = %q, want %q", host.Spec.DNSPolicy, dnsClusterFirstWithHostNet)
	}
	security := host.Spec.Containers[0].SecurityContext
	if security.Privileged == nil || !*security.Privileged {
		t.Fatal("privileged not set on the container")
	}
	// Privileged is not an excuse to start overriding the group: doing so
	// drops the supplementary video group that reaches the GPU device nodes.
	if security.RunAsGroup != nil {
		t.Fatal("runAsGroup must stay unset even when privileged")
	}
}

// The liveness probe is the only mechanism that restarts a worker which is
// alive but has stopped answering, and it is also the only one that can kill a
// worker that is merely slow. Both halves of that trade are asserted here.
func TestBuildPodInstallsALivenessProbeOnlyWhenAsked(t *testing.T) {
	f := newFakeAPIServer(t)

	// Off by default. A worker image whose bootstrap predates the endpoint
	// answers 404, and a probe against it would restart the container forever
	// without it ever serving - a worse outcome than the wedge it prevents.
	plain := testAdapter(t, f, nil).buildPod("glm53-decode")
	if plain.Spec.Containers[0].LivenessProbe != nil {
		t.Fatal("liveness probe must stay off unless the operator asks for it")
	}

	pod := testAdapter(t, f, func(cfg *Config) { cfg.LivenessProbe = true }).buildPod("glm53-decode")
	probe := pod.Spec.Containers[0].LivenessProbe
	if probe == nil {
		t.Fatal("liveness probe was requested but not rendered")
	}
	if probe.HTTPGet == nil {
		t.Fatal("liveness probe has no httpGet action")
	}
	if probe.HTTPGet.Path != livenessPath {
		t.Fatalf("liveness path = %q, want %q", probe.HTTPGet.Path, livenessPath)
	}
	if probe.HTTPGet.Port != 9001 {
		t.Fatalf("liveness port = %d, want the bootstrap port", probe.HTTPGet.Port)
	}
	// The endpoint is behind the same shared secret as the rest of the
	// surface; a probe without the header would be answered with a 401 and the
	// kubelet would read that as the container being dead.
	if len(probe.HTTPGet.HTTPHeaders) != 1 || probe.HTTPGet.HTTPHeaders[0].Name != "X-Bootstrap-Token" ||
		probe.HTTPGet.HTTPHeaders[0].Value != testToken {
		t.Fatalf("liveness probe does not carry the bootstrap token: %+v", probe.HTTPGet.HTTPHeaders)
	}
	if probe.InitialDelaySeconds != livenessInitialDelaySeconds ||
		probe.PeriodSeconds != livenessPeriodSeconds ||
		probe.TimeoutSeconds != livenessTimeoutSeconds ||
		probe.FailureThreshold != livenessFailureThreshold {
		t.Fatalf("liveness timing drifted from the documented constants: %+v", probe)
	}
}

// The readiness and liveness probes must not converge on one endpoint. A
// readiness probe is *supposed* to fail while a worker loads its weights; the
// same failure reported as liveness restarts a container that is doing exactly
// what it should.
func TestLivenessAndReadinessProbeDifferentQuestions(t *testing.T) {
	f := newFakeAPIServer(t)
	container := testAdapter(t, f, func(cfg *Config) { cfg.LivenessProbe = true }).
		buildPod("glm53-decode").Spec.Containers[0]

	if container.ReadinessProbe == nil || container.LivenessProbe == nil {
		t.Fatal("both probes should be present")
	}
	if container.ReadinessProbe.HTTPGet.Path == container.LivenessProbe.HTTPGet.Path {
		t.Fatalf("both probes point at %q; liveness must not be the model proxy",
			container.ReadinessProbe.HTTPGet.Path)
	}
	if container.ReadinessProbe.HTTPGet.Path != "/bootstrap/health" {
		t.Fatalf("readiness path = %q, want the health surface", container.ReadinessProbe.HTTPGet.Path)
	}
}

// A kubelet that does not receive a rendered probe does not probe at all, and
// every deployed cluster would then be running the old, unwatched spec while
// the source reads as if it were covered. The tag is the whole delivery
// mechanism, so it is asserted directly.
func TestLivenessProbeSurvivesTheAPISerialisation(t *testing.T) {
	f := newFakeAPIServer(t)
	container := testAdapter(t, f, func(cfg *Config) { cfg.LivenessProbe = true }).
		buildPod("glm53-decode").Spec.Containers[0]

	encoded, err := json.Marshal(container)
	if err != nil {
		t.Fatalf("marshal container: %v", err)
	}
	if !strings.Contains(string(encoded), `"livenessProbe"`) {
		t.Fatalf("liveness probe is missing from the Pod JSON: %s", encoded)
	}
	// The absent probe must not appear at all, so an old image is never asked
	// to answer a path it does not serve.
	plain := testAdapter(t, f, nil).buildPod("glm53-decode").Spec.Containers[0]
	encoded, err = json.Marshal(plain)
	if err != nil {
		t.Fatalf("marshal container: %v", err)
	}
	if strings.Contains(string(encoded), "livenessProbe") {
		t.Fatalf("an unrequested liveness probe appeared in the Pod JSON: %s", encoded)
	}
}

// The probe has to be patient with a healthy worker and impatient with a
// wedged one, and both bounds live in constants that are easy to relax by
// accident. A healthy worker is never silent for a second; the one this was
// written for was silent for ten minutes.
func TestLivenessTimingKeepsItsMargin(t *testing.T) {
	if livenessInitialDelaySeconds < 30 {
		t.Fatalf("initial delay %ds is too short for the entrypoint and the bootstrap",
			livenessInitialDelaySeconds)
	}
	window := time.Duration(livenessPeriodSeconds) * time.Duration(livenessFailureThreshold) * time.Second
	if window < time.Minute {
		t.Fatalf("a %s silence window is too easy to trip", window)
	}
	if window > 3*time.Minute {
		t.Fatalf("a %s silence window recovers too slowly from a real wedge", window)
	}
}

func TestProvisionNodeWaitsForRunningAndReportsEndpoints(t *testing.T) {
	f := newFakeAPIServer(t)
	f.pendingGets = 1 // Pending once, then Running
	shrinkPollInterval(t)
	adapter := testAdapter(t, f, nil)

	node, err := adapter.ProvisionNode(context.Background(), "glm53-prefill")
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if node.ID != "glm53-prefill" {
		t.Fatalf("id = %q", node.ID)
	}
	if node.Endpoint != "http://10.222.1.10:9001" {
		t.Fatalf("endpoint = %q", node.Endpoint)
	}
	if node.ServiceEndpoint != "http://10.222.1.10:9002" {
		t.Fatalf("service endpoint = %q", node.ServiceEndpoint)
	}
	if node.LeaseID != "k8s-uid-glm53-prefill" {
		t.Fatalf("lease = %q", node.LeaseID)
	}
	if node.Spec.GPUCount != 8 {
		t.Fatalf("spec = %+v", node.Spec)
	}
}

func TestProvisionNodeAdoptsItsOwnPodAndRefusesForeignOnes(t *testing.T) {
	f := newFakeAPIServer(t)
	f.put(Pod{
		Metadata: ObjectMeta{Name: "existing", Labels: map[string]string{LabelPartner: "cmcc-k8s"}},
		Status:   PodStatus{Phase: PodRunning, PodIP: "10.222.1.11"},
	})
	adapter := testAdapter(t, f, nil)
	node, err := adapter.ProvisionNode(context.Background(), "existing")
	if err != nil {
		t.Fatalf("adopting own pod: %v", err)
	}
	if node.ID != "existing" {
		t.Fatalf("id = %q", node.ID)
	}

	f.put(Pod{
		Metadata: ObjectMeta{Name: "foreign", Labels: map[string]string{LabelPartner: "someone-else"}},
		Status:   PodStatus{Phase: PodRunning, PodIP: "10.222.1.12"},
	})
	if _, err := adapter.ProvisionNode(context.Background(), "foreign"); err == nil {
		t.Fatal("adopting another partner's pod must fail")
	}
}

func TestProvisionNodeRejectsInvalidName(t *testing.T) {
	f := newFakeAPIServer(t)
	adapter := testAdapter(t, f, nil)
	for _, name := range []string{"", "Upper_Case", "-leading", "trailing-", strings.Repeat("a", 64)} {
		if _, err := adapter.ProvisionNode(context.Background(), name); err == nil {
			t.Fatalf("name %q must be rejected", name)
		}
	}
}

func TestProvisionNodeReportsWhyThePodNeverStarted(t *testing.T) {
	f := newFakeAPIServer(t)
	f.pendingGets = 1 << 30 // never becomes running
	shrinkPollInterval(t)
	f.put(Pod{
		Metadata: ObjectMeta{Name: "stuck", Labels: map[string]string{LabelPartner: "cmcc-k8s"}},
		Status: PodStatus{
			Phase: PodPending,
			ContainerStatuses: []ContainerStatus{{
				Name:  containerName,
				State: ContainerState{Waiting: &ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "failed to pull image"}},
			}},
		},
	})
	adapter := testAdapter(t, f, func(cfg *Config) { cfg.WaitTimeout = 200 * time.Millisecond })

	_, err := adapter.ProvisionNode(context.Background(), "stuck")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Fatalf("error must name the cause, got %q", err)
	}
}

// shrinkPollInterval makes the running-wait loop spin fast, and restores the
// production interval when the test ends.
func shrinkPollInterval(t *testing.T) {
	t.Helper()
	previous := waitPollInterval
	waitPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { waitPollInterval = previous })
}

func TestListAvailableInstancesFiltersByOwnershipAndAddress(t *testing.T) {
	f := newFakeAPIServer(t)
	f.put(Pod{
		Metadata: ObjectMeta{Name: "ours", Labels: map[string]string{LabelManagedBy: managedByValue, LabelPartner: "cmcc-k8s"}},
		Status:   PodStatus{Phase: PodRunning, PodIP: "10.222.1.20"},
	})
	// A Pod of another partner in the shared namespace must never be adopted.
	f.put(Pod{
		Metadata: ObjectMeta{Name: "theirs", Labels: map[string]string{LabelManagedBy: managedByValue, LabelPartner: "other"}},
		Status:   PodStatus{Phase: PodRunning, PodIP: "10.222.1.21"},
	})
	// Perturbed by hand under our labels: no address yet, not capacity.
	f.put(Pod{
		Metadata: ObjectMeta{Name: "pending", Labels: map[string]string{LabelManagedBy: managedByValue, LabelPartner: "cmcc-k8s"}},
		Status:   PodStatus{Phase: PodPending},
	})
	// Terminating: still present, already gone for capacity purposes.
	terminating := "2026-10-08T12:00:00Z"
	f.put(Pod{
		Metadata: ObjectMeta{Name: "terminating", Labels: map[string]string{LabelManagedBy: managedByValue, LabelPartner: "cmcc-k8s"}, DeletionTimestamp: &terminating},
		Status:   PodStatus{Phase: PodRunning, PodIP: "10.222.1.22"},
	})

	adapter := testAdapter(t, f, nil)
	instances, err := adapter.ListAvailableInstances(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(instances) != 1 || instances[0].ID != "ours" {
		t.Fatalf("got %+v", instances)
	}
	if instances[0].Endpoint != "http://10.222.1.20:9001" || instances[0].ServiceEndpoint != "http://10.222.1.20:9002" {
		t.Fatalf("endpoints = %+v", instances[0])
	}
}

func TestReleaseInstanceIsIdempotent(t *testing.T) {
	f := newFakeAPIServer(t)
	f.put(Pod{Metadata: ObjectMeta{Name: "gone-later"}})
	adapter := testAdapter(t, f, nil)

	if err := adapter.ReleaseInstance(context.Background(), "gone-later"); err != nil {
		t.Fatalf("first release: %v", err)
	}
	if err := adapter.ReleaseInstance(context.Background(), "gone-later"); err != nil {
		t.Fatalf("second release must be a no-op, got %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.deleted) != 1 {
		t.Fatalf("deleted = %v", f.deleted)
	}
}

func TestAdapterSatisfiesBothContractShapes(t *testing.T) {
	f := newFakeAPIServer(t)
	adapter := testAdapter(t, f, nil)
	var _ partner.PartnerAdapter = adapter
	var _ partner.SelfProvisioningAdapter = adapter
	if adapter.PartnerID() != "cmcc-k8s" {
		t.Fatalf("partner id = %q", adapter.PartnerID())
	}
}

func TestToInstanceNormalizesEndpoints(t *testing.T) {
	node := partner.ProvisionedNode{ID: "p", Endpoint: "http://10.0.0.1:9001/", ServiceEndpoint: "http://10.0.0.1:9002/", LeaseID: "k8s-x"}
	instance := node.ToInstance("cmcc-k8s")
	if instance.Endpoint != "http://10.0.0.1:9001" || instance.ServiceEndpoint != "http://10.0.0.1:9002" {
		t.Fatalf("endpoints not normalized: %+v", instance)
	}
	if instance.ServiceURL() != "http://10.0.0.1:9002" {
		t.Fatalf("service url = %q", instance.ServiceURL())
	}
}

// minimalKubeconfig writes a kubeconfig that resolves without touching the
// network: New only parses it. TLS verification is skipped so no real
// certificate has to be generated for a client that never dials.
func minimalKubeconfig(t *testing.T) string {
	t.Helper()
	return writeKubeconfig(t, `
apiVersion: v1
kind: Config
current-context: nightshift
clusters:
  - name: tenant-cluster
    cluster:
      server: https://192.168.203.2:6443
      insecure-skip-tls-verify: true
contexts:
  - name: nightshift
    context:
      cluster: tenant-cluster
      user: nightshift
      namespace: nightshift
users:
  - name: nightshift
    user:
      token: sa-token-value
`)
}

// envOf flattens the worker container's environment.
func envOf(pod Pod) map[string]string {
	env := map[string]string{}
	for _, variable := range pod.Spec.Containers[0].Env {
		env[variable.Name] = variable.Value
	}
	return env
}

func TestNodeRotationSpreadsWorkersAndMergesEnv(t *testing.T) {
	f := newFakeAPIServer(t)

	// A partner without nodes behaves exactly as before: one selector, one
	// environment, and no node label claiming a machine it never chose.
	plain := testAdapter(t, f, nil).buildPod("glm53-a")
	if _, labelled := plain.Metadata.Labels[LabelNode]; labelled {
		t.Fatalf("a partner without nodes must not name one: %v", plain.Metadata.Labels)
	}
	if plain.Spec.NodeSelector["gpu"] != "h100" {
		t.Fatalf("selector changed: %v", plain.Spec.NodeSelector)
	}

	adapter := testAdapter(t, f, func(cfg *Config) {
		cfg.Env = map[string]string{"NCCL_IB_TC": "106", "TAI_TALEA_START_WAIT_SECONDS": "120"}
		cfg.Nodes = []NodeProfile{
			{Host: "h100-04", Env: map[string]string{"NCCL_IB_TC": "136"}},
			{Host: "h100-01", Env: map[string]string{"NCCL_IB_HCA": "mlx5_0,mlx5_1"}},
		}
	})

	first := adapter.buildPod("glm53-a")
	second := adapter.buildPod("glm53-b")
	third := adapter.buildPod("glm53-c")

	// Two workers must land on two machines. Rotating is the whole point: both
	// would otherwise claim all eight cards of the first node.
	if got := first.Metadata.Labels[LabelNode]; got != "h100-04" {
		t.Fatalf("first pod node = %q, want h100-04", got)
	}
	if got := second.Metadata.Labels[LabelNode]; got != "h100-01" {
		t.Fatalf("second pod node = %q, want h100-01", got)
	}
	if got := third.Metadata.Labels[LabelNode]; got != "h100-04" {
		t.Fatalf("rotation did not wrap around: %q", got)
	}
	if first.Spec.NodeSelector[hostnameLabel] != "h100-04" || second.Spec.NodeSelector[hostnameLabel] != "h100-01" {
		t.Fatalf("nodes not pinned by hostname: %v / %v", first.Spec.NodeSelector, second.Spec.NodeSelector)
	}

	// The machine's own environment is the most specific layer, so it wins...
	if env := envOf(first); env["NCCL_IB_TC"] != "136" {
		t.Fatalf("node env must override the partner env: %v", env)
	}
	// ...while everything the partner sets still reaches the container.
	if env := envOf(first); env["TAI_TALEA_START_WAIT_SECONDS"] != "120" {
		t.Fatalf("partner env lost on a node-aware pod: %v", env)
	}
	// A value only the second node needs must not leak onto the first.
	if env := envOf(second); env["NCCL_IB_HCA"] != "mlx5_0,mlx5_1" {
		t.Fatalf("second node env missing: %v", env)
	}
	if env := envOf(first); env["NCCL_IB_HCA"] != "" {
		t.Fatalf("node env leaked across machines: %v", env)
	}
}

func TestNodeSelectorOverridesThePartnerSelector(t *testing.T) {
	f := newFakeAPIServer(t)
	adapter := testAdapter(t, f, func(cfg *Config) {
		cfg.Nodes = []NodeProfile{{
			Host:     "h100-04",
			Selector: map[string]string{"nvidia.com/gpu.product": "H100-SXM"},
		}}
	})
	pod := adapter.buildPod("glm53-a")
	if pod.Spec.NodeSelector["nvidia.com/gpu.product"] != "H100-SXM" {
		t.Fatalf("explicit node selector ignored: %v", pod.Spec.NodeSelector)
	}
	if _, pinned := pod.Spec.NodeSelector[hostnameLabel]; pinned {
		t.Fatalf("hostname must not be added when a selector is configured: %v", pod.Spec.NodeSelector)
	}
}

// TestNodeRotationIsSafeUnderConcurrency covers the reason the cursor is
// atomic: provisioning requests arrive on separate goroutines, and a lost
// update would place two workers on one machine.
func TestNodeRotationIsSafeUnderConcurrency(t *testing.T) {
	f := newFakeAPIServer(t)
	adapter := testAdapter(t, f, func(cfg *Config) {
		cfg.Nodes = []NodeProfile{{Host: "h100-04"}, {Host: "h100-01"}}
	})

	const workers = 40
	var wg sync.WaitGroup
	counts := make([]int, len(adapter.cfg.Nodes))
	var mu sync.Mutex
	for index := 0; index < workers; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			host := adapter.buildPod("glm53").Metadata.Labels[LabelNode]
			mu.Lock()
			defer mu.Unlock()
			for at, node := range adapter.cfg.Nodes {
				if node.Host == host {
					counts[at]++
				}
			}
		}()
	}
	wg.Wait()
	for at, count := range counts {
		if count != workers/len(counts) {
			t.Fatalf("node %s got %d of %d workers, want an even split: %v",
				adapter.cfg.Nodes[at].Host, count, workers, counts)
		}
	}
}

func TestNewRejectsBrokenNodeProfiles(t *testing.T) {
	path := minimalKubeconfig(t)
	base := Config{
		Kubeconfig:     path,
		Namespace:      "nightshift",
		Image:          "registry.local/talea-worker:test",
		BootstrapToken: testToken,
	}
	serviceable, err := New("cmcc-k8s", base)
	if err != nil {
		t.Fatalf("a partner without nodes must build: %v", err)
	}
	if serviceable.cfg.Nodes != nil {
		t.Fatalf("nodes invented from nothing: %+v", serviceable.cfg.Nodes)
	}

	broken := map[string][]NodeProfile{
		"host missing": {{Host: "  "}},
		"host repeated": {
			{Host: "h100-04"},
			{Host: "h100-04"},
		},
	}
	for name, nodes := range broken {
		cfg := base
		cfg.Nodes = nodes
		if _, err := New("cmcc-k8s", cfg); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
}

// TestMachineArgsFollowTheScheduledNode is the regression test for the
// asymmetry that has now cost two real deployments: the machine is picked by
// the placement rotation and the role by the planner, and neither sees the
// other's choice. A device list keyed by role therefore hands one node the
// other node's HCAs, and SGLang exits at start-up while every field the
// control plane can inspect stays correct.
func TestMachineArgsFollowTheScheduledNode(t *testing.T) {
	f := newFakeAPIServer(t)
	adapter := testAdapter(t, f, func(cfg *Config) {
		cfg.Nodes = []NodeProfile{
			{Host: "h100-04", ExtraSGLangArgs: []string{"--disaggregation-ib-device=mlx5_0,mlx5_1"}},
			{Host: "h100-01", ExtraSGLangArgs: []string{"--disaggregation-ib-device=mlx5_2,mlx5_3"}},
		}
	})

	scheduled := func(host string) Pod {
		return Pod{
			Metadata: ObjectMeta{Name: "talea-cmcc-k8s-1010-172229", Labels: map[string]string{LabelPartner: "cmcc-k8s"}},
			Spec:     PodSpec{NodeName: host},
			Status:   PodStatus{Phase: PodRunning, PodIP: "10.214.81.70"},
		}
	}

	for _, test := range []struct {
		host string
		want string
	}{
		{host: "h100-04", want: "mlx5_0,mlx5_1"},
		{host: "h100-01", want: "mlx5_2,mlx5_3"},
	} {
		pod := scheduled(test.host)
		provisioned := adapter.toProvisioned(pod)
		if got := strings.Join(provisioned.Spec.SGLangArgsOverride, " "); !strings.Contains(got, test.want) {
			t.Fatalf("pod on %s got %q, want the device list of that machine (%s)", test.host, got, test.want)
		}
		capacity, err := adapter.toCapacityInstance(pod)
		if err != nil {
			t.Fatalf("toCapacityInstance(%s): %v", test.host, err)
		}
		// The pull path rebuilds the spec on every round, so the override has
		// to be present there too or a pull would erase it.
		if got := strings.Join(capacity.Spec.SGLangArgsOverride, " "); !strings.Contains(got, test.want) {
			t.Fatalf("pulled pod on %s got %q, want %s", test.host, got, test.want)
		}
	}

	// A Pod the scheduler has not placed yet must not be handed an arbitrary
	// machine's flags: a wrong device list fails at start-up, while an absent
	// one at least leaves the role's own list in place.
	if got := adapter.toProvisioned(scheduled("")).Spec.SGLangArgsOverride; got != nil {
		t.Fatalf("an unplaced pod claimed machine flags: %v", got)
	}
	if got := adapter.toProvisioned(scheduled("h100-99")).Spec.SGLangArgsOverride; got != nil {
		t.Fatalf("a pod on an unlisted node claimed machine flags: %v", got)
	}

	// A namespace with no node list keeps the single-node behaviour.
	plain := testAdapter(t, f, nil)
	if got := plain.toProvisioned(scheduled("h100-04")).Spec.SGLangArgsOverride; got != nil {
		t.Fatalf("a partner without nodes invented an override: %v", got)
	}
}

// TestNodeRotationStillDecidesPlacement guards the other half of the contract:
// the override is read from where the Pod landed, not used to choose where it
// goes.
func TestNodeRotationStillDecidesPlacement(t *testing.T) {
	f := newFakeAPIServer(t)
	adapter := testAdapter(t, f, func(cfg *Config) {
		cfg.Nodes = []NodeProfile{
			{Host: "h100-04", ExtraSGLangArgs: []string{"--disaggregation-ib-device=mlx5_0"}},
			{Host: "h100-01", ExtraSGLangArgs: []string{"--disaggregation-ib-device=mlx5_2"}},
		}
	})
	first := adapter.buildPod("glm53-a")
	second := adapter.buildPod("glm53-b")
	if first.Spec.NodeSelector[hostnameLabel] != "h100-04" || second.Spec.NodeSelector[hostnameLabel] != "h100-01" {
		t.Fatalf("rotation changed: %v / %v", first.Spec.NodeSelector, second.Spec.NodeSelector)
	}
	// buildPod must not leak the machine's sglang flags into the request: the
	// Pod does not know its node until the scheduler answers.
	if len(first.Spec.Containers) > 0 {
		for _, env := range first.Spec.Containers[0].Env {
			if strings.Contains(env.Name, "SGLANG") {
				t.Fatalf("machine args leaked into the pod env: %v", env)
			}
		}
	}
}
