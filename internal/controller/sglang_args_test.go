package controller

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tai-core/tai-talea/internal/config"
)

func TestMergeSGLangArgsAppendsWhatOnlyTheRoleNeeds(t *testing.T) {
	base := []string{"--tp-size=8", "--mem-fraction-static=0.60"}
	override := []string{"--cuda-graph-max-bs-decode=128"}

	got := mergeSGLangArgs(base, override)
	want := []string{"--tp-size=8", "--mem-fraction-static=0.60", "--cuda-graph-max-bs-decode=128"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestMergeSGLangArgsLetsTheRoleWinPerFlag(t *testing.T) {
	base := []string{"--tp-size=8", "--mem-fraction-static=0.60", "--kv-cache-dtype=bfloat16"}
	override := []string{"--mem-fraction-static=0.55"}

	got := mergeSGLangArgs(base, override)
	want := []string{"--tp-size=8", "--mem-fraction-static=0.55", "--kv-cache-dtype=bfloat16"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// The override must not leave the shared value behind. SGLang parses with
	// argparse, where a repeated flag is legal and only the last one counts,
	// so an append would produce a vector whose meaning depends on order.
	for _, argument := range got {
		if argument == "--mem-fraction-static=0.60" {
			t.Fatalf("the overridden value survived: %v", got)
		}
	}
}

func TestMergeSGLangArgsTreatsEqualsAndSpaceFormsAsOneFlag(t *testing.T) {
	// Operators write both spellings, and both reach SGLang as separate argv
	// entries, so both must be recognised as the same flag here.
	base := []string{"--tp-size 8"}
	override := []string{"--tp-size=4"}
	got := mergeSGLangArgs(base, override)
	if !reflect.DeepEqual(got, []string{"--tp-size=4"}) {
		t.Fatalf("got %v, want the role value alone", got)
	}
}

func TestMergeSGLangArgsIsEmptySafeAndDoesNotAlias(t *testing.T) {
	base := []string{"--tp-size=8"}
	if got := mergeSGLangArgs(base, nil); !reflect.DeepEqual(got, base) {
		t.Fatalf("got %v, want %v", got, base)
	}
	if got := mergeSGLangArgs(nil, nil); len(got) != 0 {
		t.Fatalf("got %v, want nothing", got)
	}

	// The result feeds a request payload; mutating it must not rewrite the
	// shared configuration the control plane keeps in memory.
	got := mergeSGLangArgs(base, []string{"--ep-size=8"})
	got[0] = "--tp-size=1"
	if base[0] != "--tp-size=8" {
		t.Fatalf("the merged slice aliases the shared list: %v", base)
	}
}

func TestMergeSGLangArgsDropsRepeatsInsideOneList(t *testing.T) {
	// A hand-maintained shared list can end up carrying a flag twice. The last
	// one is what SGLang would use, so it is the one that survives.
	got := mergeSGLangArgs([]string{"--tp-size=8", "--tp-size=8"}, []string{"--tp-size=4"})
	if !reflect.DeepEqual(got, []string{"--tp-size=4"}) {
		t.Fatalf("got %v, want a single --tp-size=4", got)
	}
}

func TestMergeSGLangArgsKeepsBlankEntriesOut(t *testing.T) {
	// validation rejects blank arguments, but the merge must not turn one into
	// an argv entry that SGLang would read as the model path.
	got := mergeSGLangArgs([]string{"--tp-size=8", "  "}, []string{""})
	if !reflect.DeepEqual(got, []string{"--tp-size=8"}) {
		t.Fatalf("got %v", got)
	}
}

// TestDeploymentTemplateSplitsItsArgumentsIntoThreeLayers runs the real merge
// over the configuration the nightshift deployment uses, in the shape the
// controller applies it: shared list, then the role, then the machine.
//
// This is the closest thing to an end-to-end check that exists while the
// control plane cannot be observed starting a worker from outside the cluster.
// It is also the regression test for the deployment that failed twice: the
// device list used to be filed under the role, which is only correct while each
// role happens to run on one machine. The planner decides the role and the
// placement rotation decides the machine, so the first assignment the two
// disagree on hands a node the other node's HCAs.
func TestDeploymentTemplateSplitsItsArgumentsIntoThreeLayers(t *testing.T) {
	cfg, err := config.Load("../../deploy/kubernetes/tai-talea.yaml")
	if err != nil {
		t.Fatalf("load the deployment configuration: %v", err)
	}
	if len(cfg.Controller.ExtraSGLangArgsByRole) == 0 {
		t.Fatal("the deployment must configure per-role arguments")
	}

	var cluster *config.PartnerKubernetesConfig
	for index := range cfg.Partners {
		if cfg.Partners[index].Kubernetes != nil {
			cluster = cfg.Partners[index].Kubernetes
			break
		}
	}
	if cluster == nil {
		t.Fatal("the deployment must configure a kubernetes partner")
	}
	if len(cluster.Nodes) != 2 {
		t.Fatalf("the deployment must list both H100 machines, got %d", len(cluster.Nodes))
	}

	render := func(role string, node config.KubernetesNodeConfig) []string {
		return mergeSGLangArgs(
			mergeSGLangArgs(cfg.Controller.ExtraSGLangArgs, cfg.Controller.ExtraSGLangArgsByRole[role]),
			node.ExtraSGLangArgs)
	}

	roles := []string{"prefill", "decode"}
	devices := map[string]string{}
	for _, node := range cluster.Nodes {
		list := valueOf(render("prefill", node), "--disaggregation-ib-device")
		if list == "" {
			t.Fatalf("%s has no device list", node.Host)
		}
		devices[node.Host] = list

		for _, role := range roles {
			arguments := render(role, node)
			// The machine is the authority on its own cards: whichever role the
			// planner picks for this node, the device list must not move.
			if got := valueOf(arguments, "--disaggregation-ib-device"); got != list {
				t.Fatalf("%s as %s got devices %q, want its own %q", node.Host, role, got, list)
			}
			// Every flag appears once. A duplicate is not a syntax error for
			// SGLang - argparse takes the last one - which is exactly why it
			// would go unnoticed in a rendered command.
			seen := map[string]bool{}
			for _, argument := range arguments {
				flag := argFlag(argument)
				if seen[flag] {
					t.Fatalf("%s as %s carries %s twice: %v", node.Host, role, flag, arguments)
				}
				seen[flag] = true
			}
		}
	}

	// The two machines do not have the same cards, and each list has to route
	// around its own slow link: h100-01's mlx5_6 reports an active link to the
	// peer and carries nothing, and h100-04's mlx5_4 is the one HDR card.
	if devices["h100-01"] == devices["h100-04"] {
		t.Fatalf("both machines share one device list: %q", devices["h100-01"])
	}
	if strings.Contains(devices["h100-01"], "mlx5_6") {
		t.Fatalf("h100-01 must route around mlx5_6, got %q", devices["h100-01"])
	}
	if strings.Contains(devices["h100-04"], "mlx5_4") {
		t.Fatalf("h100-04 must route around mlx5_4, got %q", devices["h100-04"])
	}
	// A list naming a card the machine does not have is the failure this whole
	// layering exists to prevent, and it is silent from the control plane.
	if strings.Contains(devices["h100-01"], "mlx5_0,") || strings.HasSuffix(devices["h100-01"], "mlx5_0") {
		t.Fatalf("h100-01 was given a card it does not own: %q", devices["h100-01"])
	}
	if strings.Contains(devices["h100-04"], "mlx5_9") || strings.Contains(devices["h100-04"], "mlx5_10") {
		t.Fatalf("h100-04 was given a card it does not own: %q", devices["h100-04"])
	}

	// What is genuinely role-shaped stays with the role, on either machine.
	decode := mergeSGLangArgs(cfg.Controller.ExtraSGLangArgs, cfg.Controller.ExtraSGLangArgsByRole["decode"])
	prefill := mergeSGLangArgs(cfg.Controller.ExtraSGLangArgs, cfg.Controller.ExtraSGLangArgsByRole["prefill"])
	if valueOf(decode, "--max-running-requests") == "" || valueOf(decode, "--cuda-graph-max-bs-decode") == "" {
		t.Fatalf("decode bounds missing: %v", decode)
	}
	if valueOf(prefill, "--max-running-requests") != "" {
		t.Fatalf("prefill must not inherit the decode bound: %v", prefill)
	}
	if valueOf(prefill, "--disaggregation-ib-device") != "" {
		t.Fatalf("the role layer must not carry a machine's device list: %v", prefill)
	}
	if valueOf(decode, "--disaggregation-ib-device") != "" {
		t.Fatalf("the role layer must not carry a machine's device list: %v", decode)
	}

	// What the two roles share is still there in both.
	for _, arguments := range [][]string{prefill, decode} {
		if valueOf(arguments, "--chunked-prefill-size") != "512" {
			t.Fatalf("lost the chunked prefill size: %v", arguments)
		}
		if valueOf(arguments, "--served-model-name") == "" {
			t.Fatalf("must advertise the model name the Router expects: %v", arguments)
		}
	}
}

// valueOf returns the value of a --flag=value argument, or "" when absent.
func valueOf(arguments []string, flag string) string {
	for _, argument := range arguments {
		if name, value, found := strings.Cut(argument, "="); found && name == flag {
			return value
		}
	}
	return ""
}
