// Package config loads and validates the control plane configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tai-core/tai-talea/internal/domain"
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

// ControllerConfig configures lifecycle orchestration.
type ControllerConfig struct {
	ReconcileInterval  Duration `yaml:"reconcile_interval" json:"reconcile_interval"`
	PrepareMaxAttempts int      `yaml:"prepare_max_attempts" json:"prepare_max_attempts"`
	StartMaxAttempts   int      `yaml:"start_max_attempts" json:"start_max_attempts"`
	ReleaseMaxAttempts int      `yaml:"release_max_attempts" json:"release_max_attempts"`
	DrainGrace         Duration `yaml:"drain_grace" json:"drain_grace"`
	OperationRetry     Duration `yaml:"operation_retry" json:"operation_retry"`
	CallTimeout        Duration `yaml:"call_timeout" json:"call_timeout"`
	// BootstrapToken is the shared secret the container bootstrap requires on
	// every /bootstrap call. Required: an empty value would mean talking to an
	// interface that anyone able to reach the port could drive.
	//
	// One secret covers the whole deployment because partner consoles create the
	// containers, so the value has to be provisioned on both sides by hand. Per
	// partner or per instance secrets would need programmatic instance creation,
	// which the M1 partner platform does not expose.
	BootstrapToken    string   `yaml:"bootstrap_token" json:"bootstrap_token"`
	SandboxDirs       []string `yaml:"sandbox_dirs" json:"sandbox_dirs"`
	ModelID           string   `yaml:"model_id" json:"model_id"`
	ModelPath         string   `yaml:"model_path" json:"model_path"`
	ExtraSGLangArgs   []string `yaml:"extra_sglang_args" json:"extra_sglang_args"`
	LeaseWarning      Duration `yaml:"lease_warning" json:"lease_warning"`
	LostRateThreshold float64  `yaml:"lost_rate_threshold" json:"lost_rate_threshold"`
}

// RouterConfig wraps the Router adapter configuration.
type RouterConfig struct {
	Name    string   `yaml:"name" json:"name"`
	URL     string   `yaml:"url" json:"url"`
	Mode    string   `yaml:"mode" json:"mode"`
	Timeout Duration `yaml:"timeout" json:"timeout"`
}

// ToAdapter converts the configuration into an adapter configuration.
func (c RouterConfig) ToAdapter() routeradapter.Config {
	return routeradapter.Config{
		Name:    c.Name,
		URL:     c.URL,
		Mode:    c.Mode,
		Timeout: c.Timeout.Duration(),
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
// Milestone 1 keeps this disabled: the design document schedules
// "Pull 对账成为权威来源" for M2. The mechanism is implemented and tested so that
// enabling it is a configuration change.
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
	ID string `yaml:"id" json:"id"`
	// Adapter is "static" or "http".
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
}

// StaticInstanceConfig is one container of the static adapter.
type StaticInstanceConfig struct {
	ID       string              `yaml:"id" json:"id"`
	Endpoint string              `yaml:"endpoint" json:"endpoint"`
	LeaseID  string              `yaml:"lease_id" json:"lease_id"`
	Spec     domain.InstanceSpec `yaml:"spec" json:"spec"`
}

// Config is the whole control plane configuration.
type Config struct {
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
	if len(c.Partners) == 0 {
		return errors.New("at least one partner must be configured")
	}
	seen := make(map[string]bool, len(c.Partners))
	for index, partner := range c.Partners {
		if err := partner.Validate(); err != nil {
			return fmt.Errorf("partners[%d]: %w", index, err)
		}
		if seen[partner.ID] {
			return fmt.Errorf("partners[%d]: duplicate partner id %q", index, partner.ID)
		}
		seen[partner.ID] = true
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
		if len(p.Instances) == 0 {
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
		if strings.TrimSpace(p.HTTP.BaseURL) == "" {
			return errors.New("http adapter requires http.base_url")
		}
	default:
		return fmt.Errorf("adapter must be static or http, got %q", p.Adapter)
	}
	if p.ReplayCacheSize < 0 {
		return errors.New("replay_cache_size must not be negative")
	}
	return nil
}

// Validate checks one static instance configuration.
func (i StaticInstanceConfig) Validate() error {
	payload := domain.EventInstance{ID: i.ID, Endpoint: i.Endpoint, LeaseID: i.LeaseID, Spec: &i.Spec}
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
