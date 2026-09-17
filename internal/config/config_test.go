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
		// The bootstrap interface has no unauthenticated mode, so a control
		// plane without a secret could not drive a single instance.
		"missing bootstrap token": func(c *config.Config) { c.Controller.BootstrapToken = "" },
		"short bootstrap token":   func(c *config.Config) { c.Controller.BootstrapToken = "short" },
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
