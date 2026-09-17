package obs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Alert is one structured, actionable observation. Milestone 1 requires an
// alert for every condition listed in development document §14.
type Alert struct {
	Name       string            `json:"name"`
	Severity   string            `json:"severity"`
	Message    string            `json:"message"`
	InstanceID string            `json:"instance_id,omitempty"`
	PartnerID  string            `json:"partner_id,omitempty"`
	EventID    string            `json:"event_id,omitempty"`
	Details    map[string]string `json:"details,omitempty"`
	FiredAt    time.Time         `json:"fired_at"`
}

// Alert names. They double as the deduplication key.
const (
	AlertPartnerSyncFailed        = "partner_sync_failed"
	AlertInstanceLostRateHigh     = "instance_lost_rate_high"
	AlertServiceStartFailed       = "service_start_failed"
	AlertRouterRegistrationFailed = "router_registration_failed"
	AlertReadinessDrainFailed     = "readiness_drain_failed"
	AlertDrainTimeout             = "drain_timeout"
	AlertPDRatioDrift             = "pd_ratio_drift"
	AlertIllegalStateTransition   = "illegal_state_transition"
	AlertLeaseExpiringSoon        = "lease_expiring_soon"
	AlertReconcileFailed          = "reconcile_failed"
	AlertOperationRetryExhausted  = "operation_retry_exhausted"
	AlertBootstrapFailure         = "bootstrap_failure"
	AlertEnvironmentMismatch      = "environment_mismatch"
)

// Severities.
const (
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// AlertSink receives fired alerts.
type AlertSink interface {
	Fire(ctx context.Context, alert Alert) error
}

// AlertSinkFunc adapts a function to the AlertSink interface.
type AlertSinkFunc func(ctx context.Context, alert Alert) error

// Fire implements AlertSink.
func (f AlertSinkFunc) Fire(ctx context.Context, alert Alert) error { return f(ctx, alert) }

// LogSink writes alerts through the structured logger.
type LogSink struct {
	Logger *slog.Logger
}

// Fire implements AlertSink.
func (s LogSink) Fire(_ context.Context, alert Alert) error {
	if s.Logger == nil {
		return nil
	}
	attributes := []any{
		"alert", alert.Name,
		"severity", alert.Severity,
		"message", alert.Message,
	}
	if alert.InstanceID != "" {
		attributes = append(attributes, "instance_id", alert.InstanceID)
	}
	if alert.PartnerID != "" {
		attributes = append(attributes, "partner_id", alert.PartnerID)
	}
	if alert.EventID != "" {
		attributes = append(attributes, "event_id", alert.EventID)
	}
	for _, key := range sortedKeys(alert.Details) {
		attributes = append(attributes, "detail_"+key, alert.Details[key])
	}
	level := slog.LevelWarn
	if alert.Severity == SeverityCritical {
		level = slog.LevelError
	}
	s.Logger.Log(context.Background(), level, "alert", attributes...)
	return nil
}

// WebhookSink posts alerts to an operator endpoint.
type WebhookSink struct {
	URL    string
	Client *http.Client
}

// Fire implements AlertSink.
func (s WebhookSink) Fire(ctx context.Context, alert Alert) error {
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	payload, err := json.Marshal(alert)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, jsonReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("alert webhook returned HTTP %d", response.StatusCode)
	}
	return nil
}

// Alerter fans an alert out to every configured sink while counting and
// rate-limiting duplicates so that a persistent fault cannot flood operators.
type Alerter struct {
	sinks     []AlertSink
	metrics   *Registry
	logger    *slog.Logger
	cooldown  time.Duration
	mu        sync.Mutex
	lastFired map[string]time.Time
	suppress  map[string]int64
}

// NewAlerter builds an alerter. cooldown <= 0 disables deduplication.
func NewAlerter(metrics *Registry, logger *slog.Logger, cooldown time.Duration, sinks ...AlertSink) *Alerter {
	return &Alerter{
		sinks:     sinks,
		metrics:   metrics,
		logger:    logger,
		cooldown:  cooldown,
		lastFired: map[string]time.Time{},
		suppress:  map[string]int64{},
	}
}

// Fire delivers an alert unless an identical one was delivered inside the cooldown.
func (a *Alerter) Fire(ctx context.Context, alert Alert) {
	if a == nil {
		return
	}
	if alert.FiredAt.IsZero() {
		alert.FiredAt = time.Now().UTC()
	}
	if alert.Severity == "" {
		alert.Severity = SeverityWarning
	}
	if a.metrics != nil {
		a.metrics.IncCounter("capacity_alerts_total",
			"Alerts fired by the capacity plane.",
			[]string{"alert", "severity"},
			"alert", alert.Name, "severity", alert.Severity)
	}
	if !a.shouldFire(alert) {
		return
	}
	if a.logger != nil {
		a.logger.Warn("alert raised",
			"alert", alert.Name,
			"severity", alert.Severity,
			"instance_id", alert.InstanceID,
			"partner_id", alert.PartnerID,
			"event_id", alert.EventID,
			"message", alert.Message)
	}
	for _, sink := range a.sinks {
		if sink == nil {
			continue
		}
		if err := sink.Fire(ctx, alert); err != nil && a.logger != nil {
			a.logger.Error("alert sink failed", "alert", alert.Name, "error", err.Error())
		}
	}
}

func (a *Alerter) shouldFire(alert Alert) bool {
	if a.cooldown <= 0 {
		return true
	}
	key := alert.Name + "\x00" + alert.InstanceID + "\x00" + alert.PartnerID
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if last, ok := a.lastFired[key]; ok && now.Sub(last) < a.cooldown {
		a.suppress[key]++
		return false
	}
	a.lastFired[key] = now
	return true
}

// Suppressed reports how many alerts were collapsed for one deduplication key.
func (a *Alerter) Suppressed(name, instanceID, partnerID string) int64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.suppress[name+"\x00"+instanceID+"\x00"+partnerID]
}

func sortedKeys(details map[string]string) []string {
	keys := make([]string, 0, len(details))
	for key := range details {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
