package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tai-core/tai-talea/internal/obs"
)

// syntheticServing returns a fake completion endpoint that answers like the
// real one, or with the given status / body when the test wants a failure.
func syntheticServing(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status == 0 {
			status = http.StatusOK
		}
		if body == "" {
			body = `{"choices":[{"message":{"content":"ok"}}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// The probe is the only check that verifies the product rather than a
// component: a pool that answers every health probe but serves no tokens
// must still be noticed. These tests drive runSyntheticRound directly - the
// loop adds nothing but a ticker.
func TestSyntheticProbeAlertsOnlyAfterTwoConsecutiveFailures(t *testing.T) {
	h := newHarness(t)
	server := syntheticServing(t, http.StatusServiceUnavailable, `{"error":"no available workers"}`)
	h.ctrl.cfg.Controller.SyntheticURL = server.URL
	ctx := context.Background()

	// First failure: recorded, not alarmed - one miss is what a busy pool
	// looks like, and the probe queues behind the same batch.
	h.ctrl.runSyntheticRound(ctx)
	if got := h.ctrl.syntheticFailures; got != 1 {
		t.Fatalf("one failed probe left the streak at %d, want 1", got)
	}
	if h.recorder.has(obs.AlertSyntheticFailed) {
		t.Fatal("the first failed probe already alerted")
	}

	// Second failure: an outage, not load. It has to be loud.
	h.ctrl.runSyntheticRound(ctx)
	if got := h.ctrl.syntheticFailures; got != 2 {
		t.Fatalf("two failed probes left the streak at %d, want 2", got)
	}
	if !h.recorder.has(obs.AlertSyntheticFailed) {
		t.Fatalf("two consecutive failures did not alert, got %v", h.recorder.names())
	}

	// Recovery clears the streak, so a later single miss starts over.
	h.ctrl.cfg.Controller.SyntheticURL = syntheticServing(t, 0, "").URL
	h.ctrl.runSyntheticRound(ctx)
	if got := h.ctrl.syntheticFailures; got != 0 {
		t.Fatalf("a successful probe left the streak at %d, want 0", got)
	}
	h.recorder.reset()
	h.ctrl.runSyntheticRound(ctx)
	if h.recorder.has(obs.AlertSyntheticFailed) {
		t.Fatal("a single failure after recovery alerted again")
	}
}

// A reply without choices is not a completion: a reasoning model that ran out
// of tokens, a proxy's HTML error page or an empty 200 all fail callers just
// as thoroughly as a 503.
func TestSyntheticProbeRejectsAChoicelessReply(t *testing.T) {
	h := newHarness(t)
	h.ctrl.cfg.Controller.SyntheticURL = syntheticServing(t, http.StatusOK, `{"choices":[]}`).URL
	if err := h.ctrl.SyntheticProbe(context.Background()); err == nil {
		t.Fatal("a 200 without choices passed the probe")
	}
}

// The probe must carry the API key when the endpoint checks one, and the URL
// may point at a base or a full completion path.
func TestSyntheticProbeSendsTheConfiguredKey(t *testing.T) {
	h := newHarness(t)
	var gotAuth string
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(server.Close)
	h.ctrl.cfg.Controller.SyntheticURL = server.URL + "/v1/chat/completions"
	h.ctrl.cfg.Controller.SyntheticAPIKey = "probe-key"
	if err := h.ctrl.SyntheticProbe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gotAuth != "Bearer probe-key" {
		t.Fatalf("probe sent Authorization %q, want the configured key", gotAuth)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("probe posted to %q, want the configured path", gotPath)
	}
}
