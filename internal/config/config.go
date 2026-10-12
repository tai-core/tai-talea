// Package config loads and validates the control plane configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/partner"
	"github.com/tai-core/tai-talea/internal/planner"
	"github.com/tai-core/tai-talea/internal/routeradapter"
)

// Duration is a YAML/JSON friendly time.Duration.
type Duration time.Duration

// UnmarshalYAML accepts "10s", "1m30s" and bare integers (seconds).
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	value := strings.TrimSpace(node.Value)
	if value == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value, err)
	}
	*d = Duration(parsed)
	return nil
}

// Duration returns the wrapped duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// ServerConfig configures the control plane HTTP server.
type ServerConfig struct {
	Listen           string   `yaml:"listen" json:"listen"`
	AdminToken       string   `yaml:"admin_token" json:"admin_token"`
	MaxPushBodyBytes int64    `yaml:"max_push_body_bytes" json:"max_push_body_bytes"`
	ReadTimeout      Duration `yaml:"read_timeout" json:"read_timeout"`
	WriteTimeout     Duration `yaml:"write_timeout" json:"write_timeout"`
	ShutdownTimeout  Duration `yaml:"shutdown_timeout" json:"shutdown_timeout"`
}

// StorageConfig configures persistence.
type StorageConfig struct {
	SQLitePath string `yaml:"sqlite_path" json:"sqlite_path"`
}

// OnboardingConfig points to the operator-installed, fixed SSH provisioner.
// Credentials and resumable jobs live under StateDir, never in the API output.
type OnboardingConfig struct {
	Python   string `yaml:"python" json:"python"`
	Script   string `yaml:"script" json:"script"`
	Manifest string `yaml:"manifest" json:"manifest"`
	StateDir string `yaml:"state_dir" json:"state_dir"`
}

// ControllerConfig configures lifecycle orchestration.
type ControllerConfig struct {
	ReconcileInterval  Duration `yaml:"reconcile_interval" json:"reconcile_interval"`
	PrepareMaxAttempts int      `yaml:"prepare_max_attempts" json:"prepare_max_attempts"`
	StartMaxAttempts   int      `yaml:"start_max_attempts" json:"start_max_attempts"`
	ReleaseMaxAttempts int      `yaml:"release_max_attempts" json:"release_max_attempts"`
	DrainGrace         Duration `yaml:"drain_grace" json:"drain_grace"`
	OperationRetry     Duration `yaml:"operation_retry" json:"operation_retry"`
	CallTimeout        Duration `yaml:"call_timeout" json:"call_timeout"`
	// StartTimeout bounds how long the control plane waits for a freshly
	// started SGLang to answer its health check. Loading a model legitimately
	// takes minutes, so this is deliberately not call_timeout: that bounds one
	// HTTP call, this bounds the whole startup.
	StartTimeout Duration `yaml:"start_timeout" json:"start_timeout"`
	// ProvisionTimeout bounds one end-to-end on-demand creation: create, wait
	// for the container to reach RUNNING, install, hand over. It exists
	// separately from start_timeout because a partner can queue a creation
	// indefinitely when its pool has no free node: the CCI platform does not
	// auto-cancel a queued request (queueTimeoutCancelEnable is false), so the
	// only bound on a queued creation is this one. It must stay above the
	// partner's wait_timeout, which bounds the RUNNING wait inside it.
	ProvisionTimeout Duration `yaml:"provision_timeout" json:"provision_timeout"`
	// BootstrapToken is the shared secret the container bootstrap requires on
	// every /bootstrap call. Required: an empty value would mean talking to an
	// interface that anyone able to reach the port could drive.
	//
	// One secret covers the deployment. SSH onboarding provisions it on workers;
	// externally prepared containers must be configured with the same value.
	BootstrapToken  string   `yaml:"bootstrap_token" json:"bootstrap_token"`
	SandboxDirs     []string `yaml:"sandbox_dirs" json:"sandbox_dirs"`
	ModelID         string   `yaml:"model_id" json:"model_id"`
	ModelPath       string   `yaml:"model_path" json:"model_path"`
	ExtraSGLangArgs []string `yaml:"extra_sglang_args" json:"extra_sglang_args"`
	// ExtraSGLangArgsByRole holds the arguments that differ between prefill
	// and decode. They are merged over ExtraSGLangArgs per flag, with the role
	// winning, so the shared list stays the place where a flag is declared once
	// and a role list only records the exceptions:
	//
	//	extra_sglang_args:        - --mem-fraction-static=0.60
	//	extra_sglang_args_by_role:
	//	  decode:                 - --mem-fraction-static=0.55
	//
	// Keys are the PD roles ("prefill", "decode"). A one-sided flag - decode's
	// --cuda-graph-max-bs-decode, prefill's --chunked-prefill-size - simply
	// appears in that role's list and nowhere else.
	ExtraSGLangArgsByRole map[string][]string `yaml:"extra_sglang_args_by_role" json:"extra_sglang_args_by_role"`
	LeaseWarning          Duration            `yaml:"lease_warning" json:"lease_warning"`
	LostRateThreshold     float64             `yaml:"lost_rate_threshold" json:"lost_rate_threshold"`
}

// RouterConfig wraps the Router adapter configuration.
type RouterConfig struct {
	Name    string   `yaml:"name" json:"name"`
	URL     string   `yaml:"url" json:"url"`
	Mode    string   `yaml:"mode" json:"mode"`
	Timeout Duration `yaml:"timeout" json:"timeout"`
	// PrefillBootstrapPort is the port of the disaggregation bootstrap server
	// running on every prefill worker (sglang default 8998). The Router hands
	// this address to the decode side so it can pull KV. Zero omits the field
	// from the registration, which leaves the Router nothing to inject and
	// breaks PD routing - so a PD deployment must set it.
	PrefillBootstrapPort int `yaml:"prefill_bootstrap_port" json:"prefill_bootstrap_port"`
}

// ToAdapter converts the configuration into an adapter configuration.
func (c RouterConfig) ToAdapter() routeradapter.Config {
	return routeradapter.Config{
		Name:    c.Name,
		URL:     c.URL,
		Mode:    c.Mode,
		Timeout: c.Timeout.Duration(),

		PrefillBootstrapPort: c.PrefillBootstrapPort,
	}
}

// PlannerConfig wraps the planner configuration.
type PlannerConfig struct {
	Gear              string   `yaml:"gear" json:"gear"`
	MaxChangePerRound int      `yaml:"max_change_per_round" json:"max_change_per_round"`
	MinHold           Duration `yaml:"min_hold" json:"min_hold"`
	Cooldown          Duration `yaml:"cooldown" json:"cooldown"`
	MaxServing        int      `yaml:"max_serving" json:"max_serving"`
}

// ToPlanner converts the configuration into a planner configuration.
func (c PlannerConfig) ToPlanner() planner.Config {
	return planner.Config{
		Gear:              c.Gear,
		MaxChangePerRound: c.MaxChangePerRound,
		MinHold:           c.MinHold.Duration(),
		Cooldown:          c.Cooldown.Duration(),
		MaxServing:        c.MaxServing,
	}
}

// PullConfig configures partner reconciliation.
//
// Disabled by default. Enable only after validating the partner adapter's
// complete snapshot and release contract against the target platform.
type PullConfig struct {
	Enabled          bool     `yaml:"enabled" json:"enabled"`
	Interval         Duration `yaml:"interval" json:"interval"`
	FailureThreshold int      `yaml:"failure_threshold" json:"failure_threshold"`
}

// AlertsConfig configures alert delivery.
type AlertsConfig struct {
	WebhookURL     string   `yaml:"webhook_url" json:"webhook_url"`
	Cooldown       Duration `yaml:"cooldown" json:"cooldown"`
	RatioTolerance float64  `yaml:"ratio_tolerance" json:"ratio_tolerance"`
}

// ImageProfile is the controlled baseline image contract from §10 (M1 route).
type ImageProfile struct {
	OS         string `yaml:"os" json:"os"`
	CUDA       string `yaml:"cuda" json:"cuda"`
	Python     string `yaml:"python" json:"python"`
	SGLang     string `yaml:"sglang" json:"sglang"`
	Wheelhouse string `yaml:"wheelhouse" json:"wheelhouse"`
	Virtualenv string `yaml:"virtualenv" json:"virtualenv"`
}

// PartnerHTTPConfig is the generic HTTP adapter configuration.
type PartnerHTTPConfig struct {
	BaseURL             string   `yaml:"base_url" json:"base_url"`
	Token               string   `yaml:"token" json:"token"`
	AvailablePath       string   `yaml:"available_path" json:"available_path"`
	LeasesPath          string   `yaml:"leases_path" json:"leases_path"`
	ReleasePathTemplate string   `yaml:"release_path_template" json:"release_path_template"`
	Timeout             Duration `yaml:"timeout" json:"timeout"`
}

// PartnerConfig describes one partner integration.
type PartnerConfig struct {
	ID           string `yaml:"id" json:"id"`
	ConsoleToken string `yaml:"console_token" json:"console_token"`
	// Adapter is "static", "http" or "alayanew".
	Adapter string `yaml:"adapter" json:"adapter"`
	// PushToken authenticates the partner on POST /v1/capacity/events.
	PushToken string `yaml:"push_token" json:"push_token"`
	// PushSecret signs the request body with HMAC-SHA256.
	PushSecret string `yaml:"push_secret" json:"push_secret"`
	// HMACTolerance bounds the accepted clock skew of a push request.
	HMACTolerance Duration `yaml:"hmac_tolerance" json:"hmac_tolerance"`
	// ReplayCacheSize bounds the in-memory replay window.
	ReplayCacheSize int `yaml:"replay_cache_size" json:"replay_cache_size"`
	// Static instances are only used by the static adapter.
	Instances []StaticInstanceConfig `yaml:"instances" json:"instances"`
	// HTTP configures the generic HTTP adapter.
	HTTP PartnerHTTPConfig `yaml:"http" json:"http"`
	// AlayaNeW configures the 九章智算云 CCI adapter.
	AlayaNeW *PartnerAlayaNeWConfig `yaml:"alayanew" json:"alayanew"`
	// Kubernetes configures the Kubernetes namespace adapter.
	Kubernetes *PartnerKubernetesConfig `yaml:"kubernetes" json:"kubernetes"`
}

// PartnerKubernetesConfig is the Kubernetes namespace adapter wiring. It serves
// platforms where the tenant may create and delete Pods but has no login: the
// container carries the bootstrap, so creation doubles as preparation.
type PartnerKubernetesConfig struct {
	// Kubeconfig is a path to a kubeconfig file. Empty means in-cluster, which
	// is the expected deployment: Pod addresses are only routable inside.
	Kubeconfig string `yaml:"kubeconfig" json:"kubeconfig"`
	Context    string `yaml:"context" json:"context"`
	Namespace  string `yaml:"namespace" json:"namespace"`

	Image           string   `yaml:"image" json:"image"`
	ImagePullPolicy string   `yaml:"image_pull_policy" json:"image_pull_policy"`
	Entrypoint      []string `yaml:"entrypoint" json:"entrypoint"`

	GPUResource  string            `yaml:"gpu_resource" json:"gpu_resource"`
	GPUCount     int               `yaml:"gpu_count" json:"gpu_count"`
	NodeSelector map[string]string `yaml:"node_selector" json:"node_selector"`

	// Nodes lists the machines this partner may place workers on. It exists
	// because a cluster's GPU nodes are not interchangeable: each one has its
	// own RDMA cards, and the container has to be told which of them to use
	// through environment variables that are fixed at creation time. A node
	// here contributes both its hostname selector and its environment.
	//
	// Empty keeps the single-node behaviour: NodeSelector alone, Config.Env
	// alone. Setting it makes the adapter rotate through the list, so a
	// provisioning request for N workers spreads them over N nodes.
	Nodes []KubernetesNodeConfig `yaml:"nodes" json:"nodes"`

	Volumes     []KubernetesVolumeConfig `yaml:"volumes" json:"volumes"`
	ShmSize     string                   `yaml:"shm_size" json:"shm_size"`
	ProfilePath string                   `yaml:"profile_path" json:"profile_path"`

	// HostNetwork / HostIPC / Privileged put the worker into the node's
	// network and IPC namespaces and open the device cgroup. An H100 worker
	// cannot reach /dev/infiniband, nor exchange the CUDA IPC handles its
	// tensor-parallel ranks pass between each other, without all three. They
	// stay false by default: a cluster that does not ask never gets them.
	HostNetwork bool `yaml:"host_network" json:"host_network"`
	HostIPC     bool `yaml:"host_ipc" json:"host_ipc"`
	Privileged  bool `yaml:"privileged" json:"privileged"`

	BootstrapPort int `yaml:"bootstrap_port" json:"bootstrap_port"`
	ServicePort   int `yaml:"service_port" json:"service_port"`

	ServiceAccount string `yaml:"service_account" json:"service_account"`
	RunAsUser      *int64 `yaml:"run_as_user" json:"run_as_user"`
	RunAsGroup     *int64 `yaml:"run_as_group" json:"run_as_group"`
	FSGroup        *int64 `yaml:"fs_group" json:"fs_group"`

	Env          map[string]string `yaml:"env" json:"env"`
	ModelSupport []string          `yaml:"model_support" json:"model_support"`

	Timeout     Duration `yaml:"timeout" json:"timeout"`
	WaitTimeout Duration `yaml:"wait_timeout" json:"wait_timeout"`
}

// KubernetesVolumeConfig mounts one node path into the worker container. With
// no PersistentVolumeClaim permission a host path is the only way model weights
// reach the container.
type KubernetesVolumeConfig struct {
	Name      string `yaml:"name" json:"name"`
	HostPath  string `yaml:"host_path" json:"host_path"`
	MountPath string `yaml:"mount_path" json:"mount_path"`
	ReadOnly  bool   `yaml:"read_only" json:"read_only"`
}

// KubernetesNodeConfig is one machine the partner may place workers on.
//
// The environment is the point of it. NCCL_IB_HCA and NCCL_IB_TC are read by
// the container before the first collective, and on a cluster whose nodes hold
// different generations of HCA the correct value is a property of the machine,
// not of the role: the wrong list either fails to initialise or, worse, picks
// a card whose path to the peer is dead and hangs the first KV transfer.
type KubernetesNodeConfig struct {
	// Host is the node's kubernetes.io/hostname value. Required, and unique
	// within a partner: two entries for one machine would rotate workers onto
	// the same node twice while leaving another idle.
	Host string `yaml:"host" json:"host"`
	// Env is merged over the partner's own env for Pods placed on this node.
	Env map[string]string `yaml:"env" json:"env"`
	// NodeSelector overrides the partner's node_selector when set. Leave it
	// empty and the adapter pins the Pod by kubernetes.io/hostname: Host.
	NodeSelector map[string]string `yaml:"node_selector" json:"node_selector"`

	// ExtraSGLangArgs are the sglang flags only this machine can accept, most
	// often --disaggregation-ib-device. They exist because the planner decides
	// the role and the rotation decides the machine, independently: a list
	// filed under a role is correct only while each role happens to sit on one
	// machine, and the first assignment the two disagree on hands a node the
	// other node's device names. The controller merges these over both the
	// shared list and the role's exceptions, per flag.
	ExtraSGLangArgs []string `yaml:"extra_sglang_args" json:"extra_sglang_args"`
}

// PartnerAlayaNeWConfig is the 九章智算云 adapter wiring. The credentials are
// platform keys and must not appear in API responses or audit records.
type PartnerAlayaNeWConfig struct {
	AccessKey string   `yaml:"access_key" json:"-"`
	SecretKey string   `yaml:"secret_key" json:"-"`
	AIDCId    int      `yaml:"aidc_id" json:"aidc_id"`
	Timeout   Duration `yaml:"timeout" json:"timeout"`
	// ModelSupport is advertised on every discovered instance.
	ModelSupport []string `yaml:"model_support" json:"model_support"`
	// BootstrapPort / ServicePort are the container ports CCI publishes.
	BootstrapPort int `yaml:"bootstrap_port" json:"bootstrap_port"`
	ServicePort   int `yaml:"service_port" json:"service_port"`
	// Creation defaults; empty product_code disables on-demand creation.
	ProductCode      string `yaml:"product_code" json:"product_code"`
	Image            string `yaml:"image" json:"image"`
	ComputeResource  string `yaml:"compute_resource" json:"compute_resource"`
	NASStorageID     string `yaml:"nas_storage_id" json:"nas_storage_id"`
	NASFileDirectory string `yaml:"nas_file_directory" json:"nas_file_directory"`
	NASMountPath     string `yaml:"nas_mount_path" json:"nas_mount_path"`
	NASReadOnly      bool   `yaml:"nas_read_only" json:"nas_read_only"`
	// Priority is the scheduling priority for created instances. The platform
	// queues requests when the product's pool has no free node, and serves the
	// queue by priority (observed default 3, lower wins). It is a pointer so an
	// absent setting leaves the platform's own default in place instead of
	// asserting a priority the operator never chose.
	Priority *int              `yaml:"priority" json:"priority"`
	Env      map[string]string `yaml:"env" json:"env"`
	// AdvertiseHosts maps an instance name to the internal address peers in
	// the same intelligent computing center should use; CCI's published
	// endpoints are public-ingress IPs unreachable from the containers.
	AdvertiseHosts map[string]string `yaml:"advertise_hosts" json:"advertise_hosts"`
	// WaitTimeout bounds waiting for a created instance to reach RUNNING.
	WaitTimeout Duration `yaml:"wait_timeout" json:"wait_timeout"`
}

// StaticInstanceConfig is one container of the static adapter.
type StaticInstanceConfig struct {
	ID       string `yaml:"id" json:"id"`
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	// ServiceEndpoint is optional. Set it when the Router must reach SGLang on a
	// different address than the bootstrap control interface, which is the case
	// on partner platforms that publish one container port per purpose.
	ServiceEndpoint string              `yaml:"service_endpoint" json:"service_endpoint"`
	LeaseID         string              `yaml:"lease_id" json:"lease_id"`
	Spec            domain.InstanceSpec `yaml:"spec" json:"spec"`
}

// Config is the whole control plane configuration.
type Config struct {
	Onboarding   OnboardingConfig `yaml:"onboarding" json:"onboarding"`
	Server       ServerConfig     `yaml:"server" json:"server"`
	Storage      StorageConfig    `yaml:"storage" json:"storage"`
	Controller   ControllerConfig `yaml:"controller" json:"controller"`
	Router       RouterConfig     `yaml:"router" json:"router"`
	Planner      PlannerConfig    `yaml:"planner" json:"planner"`
	Pull         PullConfig       `yaml:"pull" json:"pull"`
	Alerts       AlertsConfig     `yaml:"alerts" json:"alerts"`
	ImageProfile ImageProfile     `yaml:"image_profile" json:"image_profile"`
	Partners     []PartnerConfig  `yaml:"partners" json:"partners"`
}

// Default returns a configuration that runs locally with sane safety margins.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Listen:           "127.0.0.1:8787",
			MaxPushBodyBytes: 1 << 20,
			ReadTimeout:      Duration(15 * time.Second),
			WriteTimeout:     Duration(30 * time.Second),
			ShutdownTimeout:  Duration(10 * time.Second),
		},
		Storage: StorageConfig{SQLitePath: "tai-talea.db"},
		Controller: ControllerConfig{
			ReconcileInterval:  Duration(10 * time.Second),
			PrepareMaxAttempts: 3,
			StartMaxAttempts:   3,
			ReleaseMaxAttempts: 3,
			DrainGrace:         Duration(300 * time.Second),
			OperationRetry:     Duration(15 * time.Second),
			CallTimeout:        Duration(10 * time.Second),
			StartTimeout:       Duration(10 * time.Minute),
			ProvisionTimeout:   Duration(45 * time.Minute),
			LeaseWarning:       Duration(120 * time.Second),
			LostRateThreshold:  0.2,
		},
		Router: RouterConfig{
			Name:    "primary",
			Mode:    routeradapter.ModeReadinessV1,
			Timeout: Duration(10 * time.Second),
		},
		Planner: PlannerConfig{
			Gear:              "1:1",
			MaxChangePerRound: 1,
			MinHold:           Duration(120 * time.Second),
			Cooldown:          Duration(60 * time.Second),
		},
		Pull: PullConfig{
			Enabled:          false,
			Interval:         Duration(30 * time.Second),
			FailureThreshold: 3,
		},
		Alerts: AlertsConfig{
			Cooldown:       Duration(5 * time.Minute),
			RatioTolerance: 0.15,
		},
		ImageProfile: ImageProfile{
			OS:         "ubuntu-22.04",
			CUDA:       "12.4",
			Python:     "3.11",
			SGLang:     "0.4.x",
			Wheelhouse: "/opt/tai-talea/wheelhouse",
			Virtualenv: "/opt/tai-talea/venv",
		},
	}
}

// Load reads, parses and validates a YAML configuration file.
func Load(path string) (Config, error) {
	config := Default()
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("config must contain exactly one YAML document")
	}
	if err := config.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return config, nil
}

// Validate enforces the invariants the control plane relies on.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Server.Listen) == "" {
		return errors.New("server.listen is required")
	}
	if len(c.Server.AdminToken) < 16 {
		return errors.New("server.admin_token must be at least 16 characters")
	}
	if c.Server.MaxPushBodyBytes <= 0 {
		return errors.New("server.max_push_body_bytes must be positive")
	}
	if strings.TrimSpace(c.Storage.SQLitePath) == "" {
		return errors.New("storage.sqlite_path is required")
	}
	if err := c.Router.ToAdapter().Validate(); err != nil {
		return fmt.Errorf("router: %w", err)
	}
	if err := c.Planner.ToPlanner().Validate(); err != nil {
		return fmt.Errorf("planner: %w", err)
	}
	if c.Controller.ReconcileInterval.Duration() <= 0 {
		return errors.New("controller.reconcile_interval must be positive")
	}
	if c.Controller.CallTimeout.Duration() <= 0 {
		return errors.New("controller.call_timeout must be positive")
	}
	if c.Controller.StartTimeout.Duration() <= 0 {
		return errors.New("controller.start_timeout must be positive")
	}
	if c.Controller.ProvisionTimeout.Duration() <= 0 {
		return errors.New("controller.provision_timeout must be positive")
	}
	if len(strings.TrimSpace(c.Controller.BootstrapToken)) < 16 {
		// Required, not optional: the container bootstrap refuses to serve
		// without a secret, so a control plane without one could never drive a
		// single instance.
		return errors.New("controller.bootstrap_token must be at least 16 characters")
	}
	if c.Controller.DrainGrace.Duration() < 0 {
		return errors.New("controller.drain_grace must not be negative")
	}
	if c.Controller.PrepareMaxAttempts <= 0 || c.Controller.StartMaxAttempts <= 0 || c.Controller.ReleaseMaxAttempts <= 0 {
		return errors.New("controller attempt limits must be positive")
	}
	if strings.TrimSpace(c.Controller.ModelID) == "" {
		return errors.New("controller.model_id is required")
	}
	for role, arguments := range c.Controller.ExtraSGLangArgsByRole {
		if role != string(domain.RolePrefill) && role != string(domain.RoleDecode) {
			return fmt.Errorf("controller.extra_sglang_args_by_role: %q is not a prefill/decode role", role)
		}
		for _, argument := range arguments {
			if strings.TrimSpace(argument) == "" {
				return fmt.Errorf("controller.extra_sglang_args_by_role.%s: arguments must not be blank", role)
			}
		}
	}
	if len(c.Partners) == 0 {
		return errors.New("at least one partner must be configured")
	}
	seen := make(map[string]bool, len(c.Partners))
	consoleTokens := map[string]bool{c.Server.AdminToken: true}
	for _, p := range c.Partners {
		if consoleTokens[p.PushToken] {
			return errors.New("partner push_token must be distinct from admin_token and other partner tokens")
		}
		consoleTokens[p.PushToken] = true
	}
	for index, partner := range c.Partners {
		if err := partner.Validate(); err != nil {
			return fmt.Errorf("partners[%d]: %w", index, err)
		}
		if seen[partner.ID] {
			return fmt.Errorf("partners[%d]: duplicate partner id %q", index, partner.ID)
		}
		seen[partner.ID] = true
		if partner.AlayaNeW != nil && partner.AlayaNeW.WaitTimeout.Duration() >= c.Controller.ProvisionTimeout.Duration() {
			// A wait_timeout that reaches the provision budget would be cut off
			// by the outer context mid-wait: the job would report a context
			// deadline instead of "the container never reached RUNNING", and
			// the operator would lose the reason. Keep them ordered.
			return fmt.Errorf("partners[%d]: alayanew.wait_timeout (%s) must be below controller.provision_timeout (%s)",
				index, partner.AlayaNeW.WaitTimeout.Duration(), c.Controller.ProvisionTimeout.Duration())
		}
		if partner.ConsoleToken != "" {
			if len(partner.ConsoleToken) < 16 || consoleTokens[partner.ConsoleToken] {
				return fmt.Errorf("partners[%d]: console_token must be at least 16 characters and distinct from all other credentials", index)
			}
			consoleTokens[partner.ConsoleToken] = true
		}
	}
	if c.Onboarding.Script != "" && (c.Onboarding.StateDir == "" || c.Onboarding.Manifest == "") {
		return errors.New("onboarding.state_dir and manifest are required")
	}
	if err := c.ImageProfile.Validate(); err != nil {
		return fmt.Errorf("image_profile: %w", err)
	}
	if c.Pull.Enabled && c.Pull.Interval.Duration() <= 0 {
		return errors.New("pull.interval must be positive when pull is enabled")
	}
	return nil
}

// Adapters lists the partner ids in configuration order.
func (c Config) PartnerIDs() []string {
	ids := make([]string, 0, len(c.Partners))
	for _, partner := range c.Partners {
		ids = append(ids, partner.ID)
	}
	return ids
}

// Validate checks one partner configuration.
func (p PartnerConfig) Validate() error {
	if strings.TrimSpace(p.ID) == "" {
		return errors.New("id is required")
	}
	if len(p.ID) > domain.MaxPartnerIDLength {
		return errors.New("id is too long")
	}
	if len(p.PushToken) < 8 {
		return errors.New("push_token must be at least 8 characters")
	}
	if len(p.PushSecret) < 16 {
		return errors.New("push_secret must be at least 16 characters")
	}
	if p.HMACTolerance.Duration() <= 0 {
		return errors.New("hmac_tolerance must be positive")
	}
	switch p.Adapter {
	case "static":
		if len(p.Instances) == 0 && p.ConsoleToken == "" {
			return errors.New("static adapter requires at least one instance")
		}
		seen := make(map[string]bool, len(p.Instances))
		for _, instance := range p.Instances {
			if err := instance.Validate(); err != nil {
				return err
			}
			if seen[instance.ID] {
				return fmt.Errorf("duplicate static instance %q", instance.ID)
			}
			seen[instance.ID] = true
		}
	case "http":
		if err := (partner.HTTPAdapterConfig{PartnerID: p.ID, BaseURL: p.HTTP.BaseURL, Timeout: p.HTTP.Timeout.Duration()}).Validate(); err != nil {
			return fmt.Errorf("http adapter: %w", err)
		}
	case "alayanew":
		if p.AlayaNeW == nil {
			return errors.New("alayanew adapter requires alayanew configuration")
		}
		if p.AlayaNeW.AccessKey == "" || p.AlayaNeW.SecretKey == "" {
			return errors.New("alayanew adapter requires access_key and secret_key")
		}
		if p.AlayaNeW.Timeout.Duration() <= 0 {
			return errors.New("alayanew adapter requires a positive timeout")
		}
	case "kubernetes":
		if p.Kubernetes == nil {
			return errors.New("kubernetes adapter requires kubernetes configuration")
		}
		if err := p.Kubernetes.Validate(p.ID); err != nil {
			return err
		}
	default:
		return fmt.Errorf("adapter must be static, http, alayanew or kubernetes, got %q", p.Adapter)
	}
	if p.ReplayCacheSize < 0 {
		return errors.New("replay_cache_size must not be negative")
	}
	return nil
}

// Validate checks the Kubernetes namespace adapter configuration.
func (k PartnerKubernetesConfig) Validate(partnerID string) error {
	// The partner id travels as a Pod label, and the adapter lists its own
	// capacity by that label. A value Kubernetes would reject at apply time is
	// caught here instead of on the first provisioning request.
	if !kubernetesLabelValue.MatchString(partnerID) {
		return errors.New("kubernetes adapter requires a partner id usable as a label value")
	}
	if strings.TrimSpace(k.Namespace) == "" {
		return errors.New("kubernetes adapter requires a namespace")
	}
	if strings.TrimSpace(k.Image) == "" {
		return errors.New("kubernetes adapter requires an image")
	}
	if k.GPUCount < 0 {
		return errors.New("kubernetes gpu_count must not be negative")
	}
	ports := map[string]int{"bootstrap_port": k.BootstrapPort, "service_port": k.ServicePort}
	for name, port := range ports {
		if port != 0 && (port < 1 || port > 65535) {
			return fmt.Errorf("kubernetes %s must be between 1 and 65535", name)
		}
	}
	if k.BootstrapPort != 0 && k.ServicePort != 0 && k.BootstrapPort == k.ServicePort {
		return errors.New("kubernetes bootstrap_port and service_port must differ")
	}
	for index, volume := range k.Volumes {
		if strings.TrimSpace(volume.Name) == "" {
			return fmt.Errorf("kubernetes volumes[%d]: name is required", index)
		}
		if !strings.HasPrefix(volume.HostPath, "/") || !strings.HasPrefix(volume.MountPath, "/") {
			return fmt.Errorf("kubernetes volumes[%d]: host_path and mount_path must be absolute", index)
		}
	}
	// A node entry is how a per-machine environment reaches the container, and
	// a mistake here is invisible until a worker hangs on its first collective
	// transfer: nothing about a Pod that started with the wrong NCCL_IB_HCA
	// looks wrong from the control plane's side.
	for name := range k.Env {
		if !environmentName.MatchString(name) {
			return fmt.Errorf("kubernetes env: %q is not an environment variable name", name)
		}
	}
	seen := make(map[string]bool, len(k.Nodes))
	for index, node := range k.Nodes {
		host := strings.TrimSpace(node.Host)
		if host == "" {
			return fmt.Errorf("kubernetes nodes[%d]: host is required", index)
		}
		if !kubernetesLabelValue.MatchString(host) {
			return fmt.Errorf("kubernetes nodes[%d]: host %q is not usable as a node name", index, node.Host)
		}
		if seen[host] {
			return fmt.Errorf("kubernetes nodes[%d]: node %q is listed twice", index, host)
		}
		seen[host] = true
		for name := range node.Env {
			if !environmentName.MatchString(name) {
				return fmt.Errorf("kubernetes nodes[%d]: %q is not an environment variable name", index, name)
			}
		}
		for _, argument := range node.ExtraSGLangArgs {
			if strings.TrimSpace(argument) == "" {
				return fmt.Errorf("kubernetes nodes[%d]: extra_sglang_args must not contain a blank argument", index)
			}
		}
	}
	return nil
}

// kubernetesLabelValue is the character set Kubernetes accepts for a label
// value: it is wider than the object-name set.
var kubernetesLabelValue = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)

// environmentName is the portable environment variable name a container
// runtime accepts. A name outside it is dropped by the runtime rather than
// reported, which turns a typo into a variable that is simply never set.
var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate checks one static instance configuration.
// ToEventInstance is the single mapping from a configured container to the
// partner event shape. Validation and the adapter builder both go through it,
// so a new field cannot be honoured in one place and forgotten in the other.
// (It already happened once: ServiceEndpoint was added to the adapter mapping
// but not to the hand-built payload in Validate, so a malformed value passed
// --check-config and only failed at start-up.)
func (i StaticInstanceConfig) ToEventInstance() domain.EventInstance {
	spec := i.Spec
	return domain.EventInstance{
		ID:              i.ID,
		Endpoint:        i.Endpoint,
		ServiceEndpoint: i.ServiceEndpoint,
		LeaseID:         i.LeaseID,
		Spec:            &spec,
	}
}

func (i StaticInstanceConfig) Validate() error {
	payload := i.ToEventInstance()
	if err := payload.Validate(); err != nil {
		return err
	}
	if i.Endpoint == "" {
		return errors.New("static instance requires an endpoint")
	}
	return nil
}

// Validate checks the controlled image profile.
func (p ImageProfile) Validate() error {
	required := map[string]string{
		"os":         p.OS,
		"cuda":       p.CUDA,
		"python":     p.Python,
		"sglang":     p.SGLang,
		"wheelhouse": p.Wheelhouse,
		"virtualenv": p.Virtualenv,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required in the controlled image route", name)
		}
	}
	return nil
}
