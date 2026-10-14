package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/config"
	"github.com/tai-core/tai-talea/internal/planner"
	"github.com/tai-core/tai-talea/internal/routeradapter"
)

// examplePath is the shipped reference configuration.
const examplePath = "../../deploy/tai-talea.example.yaml"

// kubernetesPath is the shipped Kubernetes-namespace configuration. It is the
// file the nightshift deployment was validated against, and the one most
// likely to rot: several of its values differ from what the platform would
// ideally look like, and each difference is a deployment that never starts.
const kubernetesPath = "../../deploy/kubernetes/tai-talea.yaml"

// TestShippedExamplesLoad keeps every example in the repository honest. They
// are copied into real deployments, so one that no longer parses - or that a
// validation rule has outgrown - is a deployment that fails at start-up with
// the operator reading a file the project itself ships. It has happened: a
// comment gained a literal "/r/n" where a line break was meant, which no
// compiler and no linter would have caught.
func TestShippedExamplesLoad(t *testing.T) {
	examples, err := filepath.Glob("../../deploy/*.yaml")
	if err != nil {
		t.Fatalf("glob examples: %v", err)
	}
	examples = append(examples, kubernetesPath)
	if len(examples) < 3 {
		t.Fatalf("expected the shipped examples, found %v", examples)
	}
	for _, path := range examples {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			if _, err := config.Load(path); err != nil {
				t.Fatalf("%s must load and validate: %v", path, err)
			}
		})
	}
}

// TestKubernetesConfigurationIsValid pins the values whose failure modes are
// invisible from the outside: a Pod that pends, an instance that is released
// minutes after it was created, or a worker that starts and then dies at the
// first KV transfer.
func TestKubernetesConfigurationIsValid(t *testing.T) {
	cfg, err := config.Load(kubernetesPath)
	if err != nil {
		t.Fatalf("the kubernetes configuration must load: %v", err)
	}
	if len(cfg.Partners) != 1 || cfg.Partners[0].Kubernetes == nil {
		t.Fatalf("unexpected partners: %+v", cfg.Partners)
	}
	cluster := cfg.Partners[0].Kubernetes

	// Without all three an H100 worker cannot reach the fabric at all.
	if !cluster.HostNetwork || !cluster.HostIPC || !cluster.Privileged {
		t.Fatalf("host access must be requested: %+v", cluster)
	}
	// No node has this image cached, so a conditional pull looks for a local
	// copy and gives up instead of fetching one.
	if cluster.ImagePullPolicy != "Always" {
		t.Fatalf("image_pull_policy = %q, want Always", cluster.ImagePullPolicy)
	}
	// The profile lives inside the worker image. The adapter injects this as
	// TAI_TALEA_PROFILE and the injected value overrides the image's own env,
	// so a wrong path is a failed self-check, exit 29, and a released
	// instance for no visible reason.
	if cluster.ProfilePath != "/opt/tai-talea/profile.json" {
		t.Fatalf("profile_path = %q", cluster.ProfilePath)
	}
	// Overriding either one drops the supplementary video group through which
	// the GPU device nodes are reachable.
	if cluster.RunAsUser != nil || cluster.RunAsGroup != nil {
		t.Fatalf("run_as_user/run_as_group must stay unset: %+v", cluster)
	}
	// The internal fork hardcodes Mooncake over RDMA; the TCP variant fails
	// the startup outright.
	for _, argument := range cfg.Controller.ExtraSGLangArgs {
		if strings.Contains(argument, "mooncake_tcp") {
			t.Fatalf("extra_sglang_args must not carry a TCP transfer backend: %q", argument)
		}
	}

	var infiniband bool
	for _, volume := range cluster.Volumes {
		if volume.MountPath != "/dev/infiniband" {
			continue
		}
		infiniband = true
		if volume.ReadOnly {
			t.Fatal("/dev/infiniband must not be read-only: a device node on a read-only mount cannot be opened for writing")
		}
	}
	if !infiniband {
		t.Fatalf("the RDMA device directory is not mounted: %+v", cluster.Volumes)
	}

	// The two GPU machines are not interchangeable, so the deployment names
	// both and gives the one whose cards need steering its own environment. A
	// node list that lost an entry would silently stack every worker onto the
	// first machine.
	hosts := map[string]config.KubernetesNodeConfig{}
	for _, node := range cluster.Nodes {
		hosts[node.Host] = node
	}
	for _, host := range []string{"h100-04", "h100-01"} {
		if _, ok := hosts[host]; !ok {
			t.Fatalf("node %s is missing from %+v", host, cluster.Nodes)
		}
	}
	if hosts["h100-04"].Env["NCCL_IB_HCA"] == "" {
		t.Fatalf("h100-04 needs its RDMA device list: %+v", hosts["h100-04"])
	}
	if hosts["h100-01"].Env["NCCL_IB_HCA"] != "" {
		t.Fatalf("h100-01 must not copy the decode device list: %+v", hosts["h100-01"])
	}
	// The sglang device list is a property of the machine, so it hangs off the
	// node entry rather than off a role. It is the difference between a worker
	// that starts and one that exits with a flag naming a card its node does
	// not have, and the role-keyed version of this failed on real hardware.
	for host, want := range map[string]string{
		"h100-04": "mlx5_0,mlx5_1,mlx5_2,mlx5_3,mlx5_5,mlx5_6,mlx5_7,mlx5_8",
		"h100-01": "mlx5_2,mlx5_3,mlx5_4,mlx5_5,mlx5_7,mlx5_8,mlx5_9,mlx5_10",
	} {
		node := hosts[host]
		if len(node.ExtraSGLangArgs) != 1 || node.ExtraSGLangArgs[0] != "--disaggregation-ib-device="+want {
			t.Fatalf("%s device list = %v, want %s", host, node.ExtraSGLangArgs, want)
		}
		// The NCCL list and the sglang list describe the same eight cards.
		// Letting them drift means one of the two layers names a device the
		// node does not have.
		if hca := node.Env["NCCL_IB_HCA"]; hca != "" && hca != want {
			t.Fatalf("%s: NCCL_IB_HCA=%q but the sglang list is %q", host, hca, want)
		}
	}
	// Both machines talk over the same bonded interface; leaving NCCL to guess
	// on a host with eight RDMA devices is how a collective hangs.
	if cluster.Env["NCCL_SOCKET_IFNAME"] == "" || cluster.Env["GLOO_SOCKET_IFNAME"] == "" {
		t.Fatalf("the shared interface must be pinned: %+v", cluster.Env)
	}
}

// TestExampleConfigurationIsValid keeps the documented example honest: it must
// load through the same strict decoder the binary uses.
func TestExampleConfigurationIsValid(t *testing.T) {
	if _, err := os.Stat(examplePath); err != nil {
		t.Fatalf("the example configuration is missing: %v", err)
	}
	cfg, err := config.Load(examplePath)
	if err != nil {
		t.Fatalf("the example configuration must load: %v", err)
	}
	if cfg.Router.ToAdapter().EffectiveMode() != routeradapter.ModeReadinessV1 {
		t.Fatalf("router mode=%s, want readiness-v1", cfg.Router.ToAdapter().EffectiveMode())
	}
	if cfg.Planner.Gear != "1:1" {
		t.Fatalf("planner gear=%s, want 1:1", cfg.Planner.Gear)
	}
	if _, ok := planner.Gears[cfg.Planner.Gear]; !ok {
		t.Fatalf("the example gear %q is not supported", cfg.Planner.Gear)
	}
	if cfg.Controller.DrainGrace.Duration() != 300*time.Second {
		t.Fatalf("drain_grace=%s, want 5m", cfg.Controller.DrainGrace.Duration())
	}
	if len(cfg.Partners) != 1 || cfg.Partners[0].ID != "partner-a" {
		t.Fatalf("unexpected partners: %+v", cfg.Partners)
	}
	if len(cfg.Controller.ModelID) == 0 {
		t.Fatal("controller.model_id must be set")
	}
}

func TestDefaultsAreValid(t *testing.T) {
	cfg := config.Default()
	cfg.Server.AdminToken = "admin-token-0123456789"
	cfg.Router.URL = "http://127.0.0.1:30001"
	cfg.Controller.ModelID = "model-a"
	cfg.Controller.BootstrapToken = "bootstrap-token-0123456789"
	cfg.Partners = []config.PartnerConfig{{
		ID: "partner-a", Adapter: "static", PushToken: "token-12345678",
		PushSecret: "secret-0123456789", HMACTolerance: config.Duration(5 * time.Minute),
		Instances: []config.StaticInstanceConfig{{ID: "c1", Endpoint: "http://10.0.0.1:8080", LeaseID: "l1"}},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	if cfg.Pull.Enabled {
		t.Fatal("pull must stay disabled in milestone 1")
	}
}

// asKubernetes swaps the base fixture's partner for a Kubernetes one, so the
// node-profile rules can be exercised against the same otherwise-valid
// configuration.
func asKubernetes(c *config.Config) {
	c.Partners[0].Adapter = "kubernetes"
	c.Partners[0].Instances = nil
	c.Partners[0].Kubernetes = &config.PartnerKubernetesConfig{
		Namespace: "nightshift",
		Image:     "registry.local/talea-worker:test",
	}
}

func TestValidationRejectsIncompleteConfiguration(t *testing.T) {
	base := func() config.Config {
		cfg := config.Default()
		cfg.Server.AdminToken = "admin-token-0123456789"
		cfg.Router.URL = "http://127.0.0.1:30001"
		cfg.Controller.ModelID = "model-a"
		cfg.Controller.BootstrapToken = "bootstrap-token-0123456789"
		cfg.Partners = []config.PartnerConfig{{
			ID: "partner-a", Adapter: "static", PushToken: "token-12345678",
			PushSecret: "secret-0123456789", HMACTolerance: config.Duration(5 * time.Minute),
			Instances: []config.StaticInstanceConfig{{ID: "c1", Endpoint: "http://10.0.0.1:8080", LeaseID: "l1"}},
		}}
		return cfg
	}

	cases := map[string]func(*config.Config){
		"short admin token":        func(c *config.Config) { c.Server.AdminToken = "short" },
		"no partners":              func(c *config.Config) { c.Partners = nil },
		"unknown adapter":          func(c *config.Config) { c.Partners[0].Adapter = "ssh" },
		"weak push secret":         func(c *config.Config) { c.Partners[0].PushSecret = "short" },
		"bad hmac window":          func(c *config.Config) { c.Partners[0].HMACTolerance = 0 },
		"static without instances": func(c *config.Config) { c.Partners[0].Instances = nil },
		"missing model":            func(c *config.Config) { c.Controller.ModelID = "" },
		"bad planner gear":         func(c *config.Config) { c.Planner.Gear = "3:1" },
		"no reconcile loop":        func(c *config.Config) { c.Controller.ReconcileInterval = 0 },
		"no sqlite path":           func(c *config.Config) { c.Storage.SQLitePath = "" },
		"incomplete profile":       func(c *config.Config) { c.ImageProfile.Wheelhouse = "" },
		"duplicate partner":        func(c *config.Config) { c.Partners = append(c.Partners, c.Partners[0]) },
		"push token equals admin":  func(c *config.Config) { c.Partners[0].PushToken = c.Server.AdminToken },
		"duplicate push token": func(c *config.Config) {
			other := c.Partners[0]
			other.ID = "partner-b"
			c.Partners = append(c.Partners, other)
		},
		"invalid http adapter": func(c *config.Config) {
			c.Partners[0].Adapter = "http"
			c.Partners[0].HTTP.BaseURL = "file:///tmp/capacity"
		},
		// The bootstrap interface has no unauthenticated mode, so a control
		// plane without a secret could not drive a single instance.
		// An instance address that would only fail at dial time must be caught
		// by --check-config instead.
		"bad service endpoint": func(c *config.Config) {
			c.Partners[0].Instances[0].ServiceEndpoint = "not-a-url"
		},
		"missing bootstrap token": func(c *config.Config) { c.Controller.BootstrapToken = "" },
		"short bootstrap token":   func(c *config.Config) { c.Controller.BootstrapToken = "short" },

		// A node entry is the only place a per-machine environment can be
		// declared, and every mistake in one is invisible until a worker is
		// already running on the wrong hardware.
		"kubernetes node without a host": func(c *config.Config) {
			asKubernetes(c)
			c.Partners[0].Kubernetes.Nodes = []config.KubernetesNodeConfig{{Host: "   "}}
		},
		"kubernetes node listed twice": func(c *config.Config) {
			asKubernetes(c)
			c.Partners[0].Kubernetes.Nodes = []config.KubernetesNodeConfig{{Host: "h100-04"}, {Host: "h100-04"}}
		},
		"kubernetes node host is not a node name": func(c *config.Config) {
			asKubernetes(c)
			c.Partners[0].Kubernetes.Nodes = []config.KubernetesNodeConfig{{Host: "h100 04"}}
		},
		"kubernetes env name with a dash": func(c *config.Config) {
			asKubernetes(c)
			c.Partners[0].Kubernetes.Nodes = []config.KubernetesNodeConfig{{
				Host: "h100-04",
				Env:  map[string]string{"NCCL-IB-HCA": "mlx5_0"},
			}}
		},
		"unknown role in the per-role arguments": func(c *config.Config) {
			c.Controller.ExtraSGLangArgsByRole = map[string][]string{"prefilling": {"--tp-size=8"}}
		},
		"blank per-role argument": func(c *config.Config) {
			c.Controller.ExtraSGLangArgsByRole = map[string][]string{"decode": {"   "}}
		},
		"blank per-machine argument": func(c *config.Config) {
			asKubernetes(c)
			c.Partners[0].Kubernetes.Nodes = []config.KubernetesNodeConfig{{
				Host:            "h100-04",
				ExtraSGLangArgs: []string{"--tp-size=8", ""},
			}}
		},
		// An unbounded automatic retry of a broken launch reloads eight GPUs of
		// weights every cooldown forever, so the cap is required rather than
		// defaulted: whoever turns the cooldown on has to say how many times.
		"start retry cooldown without a rearm cap": func(c *config.Config) {
			c.Controller.StartRetryCooldown = config.Duration(30 * time.Minute)
		},
		"negative start retry cooldown": func(c *config.Config) {
			c.Controller.StartRetryCooldown = config.Duration(-time.Minute)
		},
	}
	for name, mutate := range cases {
		cfg := base()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
}

func TestLoadRejectsUnknownFieldsAndBadDurations(t *testing.T) {
	directory := t.TempDir()

	unknown := filepath.Join(directory, "unknown.yaml")
	if err := os.WriteFile(unknown, []byte("unknown_section:\n  value: 1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := config.Load(unknown); err == nil {
		t.Fatal("an unknown field must be rejected so typos cannot silently disable behaviour")
	}

	badDuration := filepath.Join(directory, "duration.yaml")
	if err := os.WriteFile(badDuration, []byte("controller:\n  reconcile_interval: soon\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := config.Load(badDuration)
	if err == nil {
		t.Fatal("an invalid duration must be rejected")
	}
	if !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadRejectsTrailingDocument(t *testing.T) {
	raw, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, append(raw, []byte("\n---\nserver:\n  listen: 0.0.0.0:9999\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err == nil {
		t.Fatal("second config document silently ignored")
	}
}

func TestRouterConfigConvertsToTheAdapter(t *testing.T) {
	cfg := config.Default()
	cfg.Router.Name = "primary"
	cfg.Router.URL = "https://router.internal:30001"
	cfg.Router.Mode = routeradapter.ModeReadinessV1
	cfg.Router.Timeout = config.Duration(3 * time.Second)
	adapterConfig := cfg.Router.ToAdapter()
	if adapterConfig.URL != cfg.Router.URL || adapterConfig.Timeout != 3*time.Second {
		t.Fatalf("unexpected adapter config: %+v", adapterConfig)
	}
	if err := adapterConfig.Validate(); err != nil {
		t.Fatalf("adapter config must validate: %v", err)
	}
}

func TestPlannerConfigConvertsToThePlanner(t *testing.T) {
	cfg := config.Default()
	cfg.Planner = config.PlannerConfig{
		Gear: "2:1", MaxChangePerRound: 2, MinHold: config.Duration(90 * time.Second),
		Cooldown: config.Duration(30 * time.Second), MaxServing: 8,
	}
	plannerConfig := cfg.Planner.ToPlanner()
	if plannerConfig.Gear != "2:1" || plannerConfig.MaxChangePerRound != 2 ||
		plannerConfig.MinHold != 90*time.Second || plannerConfig.MaxServing != 8 {
		t.Fatalf("unexpected planner config: %+v", plannerConfig)
	}
	if err := plannerConfig.Validate(); err != nil {
		t.Fatalf("planner config must validate: %v", err)
	}
}

// A partner whose pool is full can leave a creation queued indefinitely and the
// platform never cancels it, so controller.provision_timeout is the only bound.
// The partner's own wait must sit strictly inside it: a wait that reaches the
// budget is cut off by the outer context, and the job then reports a context
// deadline instead of "the container never reached RUNNING".
func TestQueuedCreationWaitMustFitInsideTheProvisionBudget(t *testing.T) {
	base := func() config.Config {
		cfg := config.Default()
		cfg.Server.AdminToken = "admin-token-0123456789"
		cfg.Router.URL = "http://127.0.0.1:30001"
		cfg.Controller.ModelID = "model-a"
		cfg.Controller.BootstrapToken = "bootstrap-token-0123456789"
		cfg.Partners = []config.PartnerConfig{{
			ID: "partner-a", Adapter: "alayanew", PushToken: "token-12345678",
			PushSecret: "secret-0123456789", HMACTolerance: config.Duration(5 * time.Minute),
			AlayaNeW: &config.PartnerAlayaNeWConfig{
				AccessKey: "ak-test", SecretKey: "sk-test",
				Timeout:     config.Duration(30 * time.Second),
				WaitTimeout: config.Duration(35 * time.Minute),
			},
		}}
		return cfg
	}

	if err := base().Validate(); err != nil {
		t.Fatalf("a wait inside the default provision budget must validate: %v", err)
	}

	cases := map[string]func(*config.Config){
		"wait reaches the budget": func(c *config.Config) { c.Partners[0].AlayaNeW.WaitTimeout = config.Duration(45 * time.Minute) },
		"wait exceeds the budget": func(c *config.Config) { c.Partners[0].AlayaNeW.WaitTimeout = config.Duration(6 * time.Hour) },
		"budget is not positive":  func(c *config.Config) { c.Controller.ProvisionTimeout = 0 },
		"budget raised for a pool": func(c *config.Config) {
			c.Controller.ProvisionTimeout = config.Duration(6 * time.Hour)
			c.Partners[0].AlayaNeW.WaitTimeout = config.Duration(6 * time.Hour)
		},
	}
	for name, mutate := range cases {
		cfg := base()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}

	// The dial the operator actually turns: a long budget with the wait kept
	// just inside it must be accepted.
	cfg := base()
	cfg.Controller.ProvisionTimeout = config.Duration(6 * time.Hour)
	cfg.Partners[0].AlayaNeW.WaitTimeout = config.Duration(5*time.Hour + 30*time.Minute)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a long provision budget must validate: %v", err)
	}
}
