package obs

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRegistryRendersPrometheusText(t *testing.T) {
	registry := NewRegistry()
	registry.IncCounter(MetricCapacityEventsTotal,
		"Capacity events observed by the control plane, by type, source and result.",
		LabelsEvent, "type", "CAPACITY_ADDED", "source", "push", "result", "APPLIED")
	registry.IncCounter(MetricCapacityEventsTotal,
		"Capacity events observed by the control plane, by type, source and result.",
		LabelsEvent, "type", "CAPACITY_ADDED", "source", "push", "result", "APPLIED")
	registry.SetGauge(MetricPDRatioTarget, "Planner target.", nil, 0.5)
	registry.Observe(MetricServiceDrainSeconds, "Drain duration.", LabelsPlannerRole, 1.5, "role", "prefill")

	rendered := registry.Render()
	for _, expected := range []string{
		"# TYPE capacity_events_total counter",
		`capacity_events_total{type="CAPACITY_ADDED",source="push",result="APPLIED"} 2`,
		"# TYPE pd_ratio_target gauge",
		"pd_ratio_target 0.5",
		"# TYPE service_drain_duration_seconds summary",
		`service_drain_duration_seconds_sum{role="prefill"} 1.5`,
		`service_drain_duration_seconds_count{role="prefill"} 1`,
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("rendered metrics are missing %q:\n%s", expected, rendered)
		}
	}
}

func TestRegistryRejectsMismatchedLabels(t *testing.T) {
	registry := NewRegistry()
	// Wrong label key: the series must be dropped rather than silently created
	// with a bogus dimension.
	registry.IncCounter("broken_total", "help", []string{"a"}, "b", "value")
	if strings.Contains(registry.Render(), "broken_total") {
		t.Fatalf("a mismatched label set must be ignored:\n%s", registry.Render())
	}
}

func TestRegistryIsNilSafe(t *testing.T) {
	var registry *Registry
	registry.IncCounter("a_total", "help", nil)
	registry.SetGauge("b", "help", nil, 1)
	registry.Observe("c", "help", nil, 1)
	if registry.PointInTime("b") != 0 || registry.SeriesCount("c") != 0 || registry.Render() != "" {
		t.Fatal("a nil registry must be inert")
	}
}

func TestPointInTimeReadsTheLastGauge(t *testing.T) {
	registry := NewRegistry()
	registry.SetGauge("capacity_lost_rate", "help", nil, 0.25)
	if got := registry.PointInTime("capacity_lost_rate"); got != 0.25 {
		t.Fatalf("value=%v, want 0.25", got)
	}
	registry.SetGauge("x", "help", []string{"k"}, 3, "k", "v")
	if got := registry.PointInTime("x", "k", "v"); got != 3 {
		t.Fatalf("labeled value=%v, want 3", got)
	}
	if got := registry.PointInTime("x", "k", "other"); got != 0 {
		t.Fatalf("unknown series=%v, want 0", got)
	}
}

func TestAlerterDeduplicatesInsideTheCooldown(t *testing.T) {
	registry := NewRegistry()
	var fired []Alert
	sink := AlertSinkFunc(func(_ context.Context, alert Alert) error {
		fired = append(fired, alert)
		return nil
	})
	alerter := NewAlerter(registry, nil, time.Minute, sink)
	alert := Alert{Name: AlertPartnerSyncFailed, InstanceID: "i-1", Message: "partner down"}

	alerter.Fire(context.Background(), alert)
	alerter.Fire(context.Background(), alert)
	alerter.Fire(context.Background(), alert)

	if len(fired) != 1 {
		t.Fatalf("fired=%d, want 1 after deduplication", len(fired))
	}
	if alerter.Suppressed(AlertPartnerSyncFailed, "i-1", "") != 2 {
		t.Fatalf("suppressed=%d, want 2", alerter.Suppressed(AlertPartnerSyncFailed, "i-1", ""))
	}

	// A different instance is a different alert.
	other := alert
	other.InstanceID = "i-2"
	alerter.Fire(context.Background(), other)
	if len(fired) != 2 {
		t.Fatalf("fired=%d, want 2 for a distinct instance", len(fired))
	}

	// Every firing is still counted, including the suppressed ones.
	if got := registry.PointInTime("capacity_alerts_total", "alert", AlertPartnerSyncFailed, "severity", SeverityWarning); got != 4 {
		t.Fatalf("capacity_alerts_total=%v, want 4", got)
	}
}

func TestAlerterSurvivesASinkFailure(t *testing.T) {
	registry := NewRegistry()
	delivered := 0
	failing := AlertSinkFunc(func(context.Context, Alert) error { return context.DeadlineExceeded })
	working := AlertSinkFunc(func(context.Context, Alert) error { delivered++; return nil })

	alerter := NewAlerter(registry, nil, 0, failing, working)
	alerter.Fire(context.Background(), Alert{Name: AlertDrainTimeout})

	if delivered != 1 {
		t.Fatalf("a failing sink must not stop the other sinks, delivered=%d", delivered)
	}
}

func TestAlertEscapesLabelValues(t *testing.T) {
	registry := NewRegistry()
	registry.SetGauge("quoted", "help", []string{"k"}, 1, "k", `a"b\c`)
	if !strings.Contains(registry.Render(), `quoted{k="a\"b\\c"} 1`) {
		t.Fatalf("label values must be escaped:\n%s", registry.Render())
	}
}
