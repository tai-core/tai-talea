// Package api exposes the control plane's HTTP surface: the partner Push API
// (§6.1) and the operator admin API.
package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tai-core/tai-talea/internal/config"
)

// Request headers used by the Push API.
const (
	HeaderTimestamp = "X-Capacity-Timestamp"
	HeaderSignature = "X-Capacity-Signature"
	HeaderPartner   = "X-Capacity-Partner"
)

// Authentication failures.
var (
	// ErrMissingCredentials means the request carried no usable credentials.
	ErrMissingCredentials = errors.New("missing credentials")
	// ErrUnknownPartner means the partner token does not match any configured partner.
	ErrUnknownPartner = errors.New("unknown partner credential")
	// ErrStaleTimestamp means the request is outside the accepted clock skew.
	ErrStaleTimestamp = errors.New("request timestamp outside the accepted window")
	// ErrBadSignature means the HMAC did not verify.
	ErrBadSignature = errors.New("request signature verification failed")
)

// pushAuthenticator implements §6.1 authentication: partner identity, timestamp
// and signature verification, and replay protection.
type pushAuthenticator struct {
	partners map[string]config.PartnerConfig
	byToken  map[string]string

	window time.Duration
	mu     sync.Mutex
	seen   map[string]time.Time
}

func newPushAuthenticator(cfg config.Config) *pushAuthenticator {
	partners := make(map[string]config.PartnerConfig, len(cfg.Partners))
	byToken := make(map[string]string, len(cfg.Partners))
	window := time.Duration(0)
	for _, partner := range cfg.Partners {
		partners[partner.ID] = partner
		byToken[partner.PushToken] = partner.ID
		if partner.HMACTolerance.Duration() > window {
			window = partner.HMACTolerance.Duration()
		}
	}
	if window <= 0 {
		window = 5 * time.Minute
	}
	return &pushAuthenticator{
		partners: partners,
		byToken:  byToken,
		window:   window,
		seen:     map[string]time.Time{},
	}
}

// SignaturePayload is the canonical signed byte string: "<timestamp>.<body>".
func SignaturePayload(timestamp string, body []byte) []byte {
	payload := make([]byte, 0, len(timestamp)+1+len(body))
	payload = append(payload, timestamp...)
	payload = append(payload, '.')
	payload = append(payload, body...)
	return payload
}

// Sign computes the value a partner must send in X-Capacity-Signature.
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(SignaturePayload(timestamp, body))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify authenticates the request and returns the authenticated partner id plus
// the request signature. The caller must already have read the bounded body.
//
// Anti-replay for the Push API is built from three layers, in this order:
//
//  1. the timestamp window bounds how long a captured request stays usable;
//  2. the HMAC covers the whole body, so a modified request cannot pass;
//  3. the unique capacity_events.event_id makes a redelivery a no-op.
//
// A verbatim redelivery is therefore *not* an authentication failure: §5
// requires it to be answered with an idempotent success, and §6.1 requires the
// partner to keep retrying until it sees that success. The signature cache only
// feeds the replay counter so operators can see a partner retrying.
func (a *pushAuthenticator) Verify(request *http.Request, body []byte, now time.Time) (string, string, error) {
	token := bearerToken(request.Header.Get("Authorization"))
	if token == "" {
		return "", "", ErrMissingCredentials
	}
	partnerID, ok := a.byToken[token]
	if !ok {
		return "", "", ErrUnknownPartner
	}
	partner := a.partners[partnerID]

	// A partner that also declares its identity must not disagree with its token.
	if declared := strings.TrimSpace(request.Header.Get(HeaderPartner)); declared != "" && declared != partnerID {
		return "", "", fmt.Errorf("%w: header declares %q", ErrUnknownPartner, declared)
	}

	timestamp := strings.TrimSpace(request.Header.Get(HeaderTimestamp))
	if timestamp == "" {
		return "", "", fmt.Errorf("%w: %s is required", ErrMissingCredentials, HeaderTimestamp)
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return "", "", fmt.Errorf("%w: %s must be a unix timestamp", ErrStaleTimestamp, HeaderTimestamp)
	}
	skew := now.Sub(time.Unix(seconds, 0))
	if skew < 0 {
		skew = -skew
	}
	tolerance := partner.HMACTolerance.Duration()
	if tolerance <= 0 {
		tolerance = a.window
	}
	if skew > tolerance {
		return "", "", fmt.Errorf("%w: skew %s exceeds %s", ErrStaleTimestamp, skew.Round(time.Second), tolerance)
	}

	signature := strings.ToLower(strings.TrimSpace(request.Header.Get(HeaderSignature)))
	signature = strings.TrimPrefix(signature, "sha256=")
	if signature == "" {
		return "", "", fmt.Errorf("%w: %s is required", ErrMissingCredentials, HeaderSignature)
	}
	expected := Sign(partner.PushSecret, timestamp, body)
	if subtle.ConstantTimeCompare([]byte(signature), []byte(expected)) != 1 {
		return "", "", ErrBadSignature
	}
	return partnerID, signature, nil
}

// Seen records a verified signature and reports whether the exact same signed
// request was already observed inside the replay window. It never rejects the
// request: idempotency is enforced by the event id, not by this cache.
func (a *pushAuthenticator) Seen(partnerID, signature string, now time.Time) bool {
	key := partnerID + "\x00" + signature
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneLocked(now)
	if _, exists := a.seen[key]; exists {
		return true
	}
	a.seen[key] = now
	return false
}

// pruneLocked drops replay entries that can no longer be replayed because their
// timestamp would now be rejected by the window.
func (a *pushAuthenticator) pruneLocked(now time.Time) {
	cutoff := now.Add(-a.window)
	for key, observed := range a.seen {
		if observed.Before(cutoff) {
			delete(a.seen, key)
		}
	}
}

// ReplayWindow reports how long signed requests are remembered.
func (a *pushAuthenticator) ReplayWindow() time.Duration { return a.window }

func bearerToken(header string) string {
	value := strings.TrimSpace(header)
	if value == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(value) <= len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(value[len(prefix):])
}

// requireAdmin enforces the operator bearer token with a constant time compare.
func requireAdmin(request *http.Request, expected string) bool {
	token := bearerToken(request.Header.Get("Authorization"))
	if token == "" || expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}
