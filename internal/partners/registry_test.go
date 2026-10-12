package partners

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/config"
	"github.com/tai-core/tai-talea/internal/partner"
)

func writeTestKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	body := `
apiVersion: v1
kind: Config
current-context: nightshift
clusters:
  - name: tenant-cluster
    cluster:
      server: https://192.168.203.2:6443
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
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

func kubernetesConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Server.AdminToken = "test-admin-token-32-characters"
	cfg.Router.URL = "http://127.0.0.1:30001"
	cfg.Controller.BootstrapToken = "0123456789abcdef0123456789abcdef"
	cfg.Controller.ModelID = "zai-org/GLM-5.3-Flash"
	cfg.Partners = []config.PartnerConfig{{
		ID:            "cmcc-k8s",
		Adapter:       "kubernetes",
		PushToken:     "cmcc-k8s-push-token",
		PushSecret:    "cmcc-k8s-push-secret-0123456789",
		HMACTolerance: config.Duration(time.Minute),
		Kubernetes: &config.PartnerKubernetesConfig{
			Kubeconfig:    writeTestKubeconfig(t),
			Context:       "nightshift",
			Namespace:     "nightshift",
			Image:         "registry.paas/cmss/talea-worker:test",
			GPUCount:      8,
			ShmSize:       "512Gi",
			BootstrapPort: 9001,
			ServicePort:   9002,
		},
	}}
	return cfg
}

func TestBuildWiresTheKubernetesAdapter(t *testing.T) {
	cfg := kubernetesConfig(t)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config must be valid: %v", err)
	}
	registry, err := Build(cfg, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	adapter, ok := registry.Adapter("cmcc-k8s")
	if !ok {
		t.Fatal("adapter not registered")
	}
	// The Kubernetes route prepares its own containers: the provisioning entry
	// must discover it as self-provisioning and never fall back to SSH.
	if _, ok := adapter.(partner.SelfProvisioningAdapter); !ok {
		t.Fatal("kubernetes adapter must implement SelfProvisioningAdapter")
	}
	if _, ok := adapter.(partner.ProvisioningAdapter); ok {
		t.Fatal("kubernetes adapter must not advertise the SSH provisioning shape")
	}
}

func TestKubernetesPartnerIDMustBeLabelSafe(t *testing.T) {
	cfg := kubernetesConfig(t)
	// A slash is legal in neither a label value nor an object name, and the
	// partner id becomes a label on every Pod this adapter creates.
	cfg.Partners[0].ID = "cmcc/k8s"
	if err := cfg.Validate(); err == nil {
		t.Fatal("an id Kubernetes would reject as a label value must fail validation")
	}
}

func TestKubernetesConfigRequiresTheEssentials(t *testing.T) {
	cases := map[string]func(*config.PartnerKubernetesConfig){
		"no namespace": func(k *config.PartnerKubernetesConfig) { k.Namespace = "" },
		"no image":     func(k *config.PartnerKubernetesConfig) { k.Image = "" },
		"negative gpu": func(k *config.PartnerKubernetesConfig) { k.GPUCount = -1 },
		"relative volume": func(k *config.PartnerKubernetesConfig) {
			k.Volumes = []config.KubernetesVolumeConfig{{Name: "models", HostPath: "apps", MountPath: "/models"}}
		},
		"same ports": func(k *config.PartnerKubernetesConfig) { k.BootstrapPort, k.ServicePort = 9001, 9001 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := kubernetesConfig(t)
			mutate(cfg.Partners[0].Kubernetes)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation to fail")
			}
		})
	}
}
