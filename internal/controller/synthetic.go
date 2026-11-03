package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tai-core/tai-talea/internal/obs"
)

// AlertSyntheticFailed reports that the end-to-end probe could not get a real
// completion through the serving path.
const AlertSyntheticFailed = "synthetic_probe_failed"

// syntheticFailureThreshold is how many consecutive failed probes raise the
// alert. Two, mirroring ReadinessFailureThreshold: the probe queues behind
// whatever the pool is busy with, so one miss is evidence of *load*, while
// two in a row inside a five-minute interval is evidence of an outage.
const syntheticFailureThreshold = 2

// syntheticTimeout bounds one probe. It covers a full generation of the
// reply tokens, not one HTTP call: the point of the probe is to prove the
// tokens arrive.
const syntheticTimeout = 60 * time.Second

// RunSynthetic drives the end-to-end probe until the context is cancelled.
// It has its own ticker because its interval is minutes, where the periodic
// drivers tick at the reconcile interval.
func (c *Controller) RunSynthetic(ctx context.Context) {
	interval := c.cfg.Controller.SyntheticInterval.Duration()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runSyntheticRound(ctx)
		}
	}
}

// runSyntheticRound performs one probe and maintains the failure streak. The
// streak only fires the alert once it crosses the threshold: a single miss is
// what a busy pool looks like, and condemning on the first miss makes the
// probe an alarm that cries with the load. Only the synthetic loop (and a
// test calling this method directly) writes the counter.
func (c *Controller) runSyntheticRound(ctx context.Context) {
	err := c.SyntheticProbe(ctx)
	c.mu.Lock()
	c.syntheticFailures++
	fails := c.syntheticFailures
	if err == nil {
		c.syntheticFailures = 0
	}
	c.mu.Unlock()

	if err == nil {
		if fails > 1 {
			c.log().Info("synthetic probe recovered", "previous_failures", fails-1)
		}
		return
	}
	c.log().Warn("synthetic probe failed", "error", err.Error(), "consecutive", fails)
	if fails < syntheticFailureThreshold {
		return
	}
	c.alert(ctx, obs.Alert{
		Name:     obs.AlertSyntheticFailed,
		Severity: obs.SeverityCritical,
		Message:  "the end-to-end probe cannot get a completion through the serving path",
		Details: map[string]string{
			"error":       err.Error(),
			"consecutive": fmt.Sprintf("%d", fails),
		},
	})
}

// SyntheticProbe pushes one small real completion through the serving path
// and expects HTTP 200 with at least one choice back. Anything short of that
// - a refused connection, a 503 from an empty pool, a body without choices -
// is a failure, because all of them mean a caller would not have got a reply.
func (c *Controller) SyntheticProbe(ctx context.Context) error {
	url := c.cfg.Controller.SyntheticURL
	if url == "" {
		url = c.cfg.Router.URL
	}
	if url == "" {
		return fmt.Errorf("no synthetic_url and no router url configured")
	}
	if !endsWithPath(url) {
		url += "/v1/chat/completions"
	}
	model := c.cfg.Controller.SyntheticModel
	if model == "" {
		model = c.cfg.Controller.ModelID
	}
	payload, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "Reply with the single word: ok"},
		},
		"max_tokens": 512,
		"stream":     false,
	})
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, syntheticTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if key := c.cfg.Controller.SyntheticAPIKey; key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	if c.metrics != nil {
		defer func() {
			result := "ok"
			if err != nil {
				result = "error"
			}
			c.metrics.IncCounter("synthetic_probe_total",
				"End-to-end probes through the serving path.",
				[]string{"result"}, "result", result)
		}()
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("probe request to %s: %w", url, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("probe to %s answered HTTP %d: %s", url, response.StatusCode, truncate(string(body), 200))
	}
	var parsed struct {
		Choices []json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("probe reply from %s is not JSON: %w", url, err)
	}
	if len(parsed.Choices) == 0 {
		return fmt.Errorf("probe reply from %s has no choices", url)
	}
	return nil
}

// endsWithPath is true when the configured URL already points at a specific
// completion endpoint rather than a base URL.
func endsWithPath(url string) bool {
	return strings.Contains(url, "/v1/chat/completions") || strings.Contains(url, "/v1/completions")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
