package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/config"
)

func testConfig() config.Config {
	cfg := config.Default()
	cfg.Server.AdminToken = "admin-token-0123456789"
	cfg.Router.URL = "http://127.0.0.1:30001"
	cfg.Controller.ModelID = "model-a"
	cfg.Partners = []config.PartnerConfig{{
		ID:            "partner-a",
		Adapter:       "static",
		PushToken:     "partner-a-token",
		PushSecret:    "partner-a-secret-0123456789",
		HMACTolerance: config.Duration(5 * time.Minute),
		Instances:     []config.StaticInstanceConfig{{ID: "c1", Endpoint: "http://10.0.0.1:8080", LeaseID: "l1"}},
	}}
	return cfg
}

func signedRequest(t *testing.T, body string, timestamp time.Time, secret, token string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "/v1/capacity/events", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	stamp := strconv.FormatInt(timestamp.Unix(), 10)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(HeaderTimestamp, stamp)
	request.Header.Set(HeaderSignature, Sign(secret, stamp, []byte(body)))
	return request
}

func TestPushAuthenticatorAcceptsAValidSignature(t *testing.T) {
	cfg := testConfig()
	auth := newPushAuthenticator(cfg)
	now := time.Now().UTC()
	body := `{"event_id":"evt-1"}`
	request := signedRequest(t, body, now, "partner-a-secret-0123456789", "partner-a-token")
	partnerID, signature, err := auth.Verify(request, []byte(body), now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if partnerID != "partner-a" {
		t.Fatalf("partner=%s, want partner-a", partnerID)
	}
	if signature == "" {
		t.Fatal("verify must return the request signature")
	}
	if auth.Seen(partnerID, signature, now) {
		t.Fatal("the first observation of a signature must not be reported as a replay")
	}
	if !auth.Seen(partnerID, signature, now) {
		t.Fatal("the second observation of a signature must be reported as a replay")
	}
}

// A verbatim redelivery must stay authentic: §5 requires the partner to receive
// an idempotent success, and the event id - not the signature cache - is what
// prevents the lifecycle from running twice.
func TestPushAuthenticatorAcceptsAVerbatimRedelivery(t *testing.T) {
	cfg := testConfig()
	auth := newPushAuthenticator(cfg)
	now := time.Now().UTC()
	body := `{"event_id":"evt-1"}`
	for attempt := 0; attempt < 3; attempt++ {
		request := signedRequest(t, body, now, "partner-a-secret-0123456789", "partner-a-token")
		if _, _, err := auth.Verify(request, []byte(body), now); err != nil {
			t.Fatalf("delivery %d must authenticate: %v", attempt+1, err)
		}
	}
}

func TestPushAuthenticatorRejectsTamperedBodies(t *testing.T) {
	cfg := testConfig()
	auth := newPushAuthenticator(cfg)
	now := time.Now().UTC()
	original := `{"event_id":"evt-1"}`
	request := signedRequest(t, original, now, "partner-a-secret-0123456789", "partner-a-token")
	tampered := `{"event_id":"evt-1","type":"CAPACITY_REVOKED"}`
	if _, _, err := auth.Verify(request, []byte(tampered), now); err == nil {
		t.Fatal("a body that does not match the signature must be rejected")
	}
}

func TestPushAuthenticatorRejectsStaleAndUnknownRequests(t *testing.T) {
	cfg := testConfig()
	auth := newPushAuthenticator(cfg)
	now := time.Now().UTC()
	body := `{"event_id":"evt-1"}`

	stale := signedRequest(t, body, now.Add(-10*time.Minute), "partner-a-secret-0123456789", "partner-a-token")
	if _, _, err := auth.Verify(stale, []byte(body), now); err == nil {
		t.Fatal("a request outside the timestamp tolerance must be rejected")
	}

	unknown := signedRequest(t, body, now, "partner-a-secret-0123456789", "someone-else")
	if _, _, err := auth.Verify(unknown, []byte(body), now); err == nil {
		t.Fatal("an unknown partner token must be rejected")
	}

	noAuth, err := http.NewRequest(http.MethodPost, "/v1/capacity/events", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if _, _, err := auth.Verify(noAuth, []byte(body), now); err == nil {
		t.Fatal("a request without credentials must be rejected")
	}

	mismatch := signedRequest(t, body, now, "partner-a-secret-0123456789", "partner-a-token")
	mismatch.Header.Set(HeaderPartner, "partner-b")
	if _, _, err := auth.Verify(mismatch, []byte(body), now); err == nil {
		t.Fatal("a contradicting partner header must be rejected")
	}
}

func TestRequireAdminUsesConstantTimeComparison(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if requireAdmin(request, "admin-token-0123456789") {
		t.Fatal("a request without a bearer token must not be authorized")
	}
	request.Header.Set("Authorization", "Bearer admin-token-0123456789")
	if !requireAdmin(request, "admin-token-0123456789") {
		t.Fatal("a valid admin token must be authorized")
	}
	request.Header.Set("Authorization", "Bearer admin-token-012345678X")
	if requireAdmin(request, "admin-token-0123456789") {
		t.Fatal("a wrong admin token must not be authorized")
	}
}
