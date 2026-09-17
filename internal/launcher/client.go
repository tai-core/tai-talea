// Package launcher speaks to the container-local bootstrap process
// (development document §9). The control plane never runs arbitrary commands
// inside a container: it only calls the four fixed bootstrap endpoints.
//
// Every call carries the shared secret in the X-Bootstrap-Token header. The
// bootstrap refuses to serve without one, and this client refuses to be built
// without one, so there is no configuration in which the interface is open.
package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
)

// Bootstrap paths.
const (
	PathHealth = "/bootstrap/health"
	PathStatus = "/bootstrap/status"
	PathStart  = "/bootstrap/start"
	PathStop   = "/bootstrap/stop"
)

// HeaderToken carries the bootstrap shared secret. The container must be
// provisioned with the same value before the control plane can drive it.
const HeaderToken = "X-Bootstrap-Token"

// Bootstrap phases reported by GET /bootstrap/status.
const (
	PhaseIdle       = "IDLE"
	PhaseInstalling = "INSTALLING"
	PhaseStarting   = "STARTING"
	PhaseRunning    = "RUNNING"
	PhaseStopping   = "STOPPING"
	PhaseStopped    = "STOPPED"
	PhaseFailed     = "FAILED"
)

// maxResponseBytes bounds what a container can send back to the control plane.
const maxResponseBytes = 1 << 20

// Health is the response of GET /bootstrap/health.
type Health struct {
	Status    string `json:"status"`
	Phase     string `json:"phase"`
	Version   string `json:"version,omitempty"`
	Detail    string `json:"detail,omitempty"`
	ServiceID string `json:"service_id,omitempty"`
}

// Healthy reports whether the bootstrap reports itself operational.
func (h Health) Healthy() bool { return h.Status == "ok" }

// Environment reports the compatibility matrix observed inside the container.
type Environment struct {
	OS         string `json:"os"`
	CUDA       string `json:"cuda"`
	Python     string `json:"python"`
	SGLang     string `json:"sglang"`
	Wheelhouse string `json:"wheelhouse,omitempty"`
	Virtualenv string `json:"virtualenv,omitempty"`
}

// Status is the response of GET /bootstrap/status.
type Status struct {
	Phase       string      `json:"phase"`
	Role        string      `json:"role,omitempty"`
	ModelID     string      `json:"model_id,omitempty"`
	ServicePID  int         `json:"service_pid,omitempty"`
	StartedAt   time.Time   `json:"started_at,omitempty"`
	StoppedAt   time.Time   `json:"stopped_at,omitempty"`
	ExitCode    int         `json:"exit_code,omitempty"`
	LastError   string      `json:"last_error,omitempty"`
	Environment Environment `json:"environment"`
}

// StartRequest is the fixed role configuration handed to the bootstrap.
type StartRequest struct {
	Role      domain.Role `json:"role"`
	ModelID   string      `json:"model_id"`
	ModelPath string      `json:"model_path,omitempty"`
	ExtraArgs []string    `json:"extra_args,omitempty"`
}

// StopRequest asks the bootstrap to stop SGLang and return an exit code.
type StopRequest struct {
	Force  bool   `json:"force"`
	Reason string `json:"reason,omitempty"`
}

// StopResult is the response of POST /bootstrap/stop.
type StopResult struct {
	Phase    string `json:"phase"`
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out"`
	Detail   string `json:"detail,omitempty"`
}

// Client is a stateless bootstrap client. The endpoint is supplied per call so
// one client serves every container; the shared secret is fixed for the whole
// deployment, because partner consoles create the containers and the value has
// to be provisioned on both sides by hand.
type Client struct {
	http  *http.Client
	token string
}

// New builds a client with a bounded timeout.
func New(timeout time.Duration, token string) (*Client, error) {
	if timeout <= 0 || timeout > 60*time.Second {
		return nil, errors.New("bootstrap timeout must be between zero and 60 seconds")
	}
	return NewWithHTTPClient(&http.Client{Timeout: timeout}, token)
}

// NewWithHTTPClient builds a client around a caller supplied HTTP client. It
// exists so tests and constrained deployments can route bootstrap calls through
// a custom transport.
func NewWithHTTPClient(client *http.Client, token string) (*Client, error) {
	if client == nil {
		return nil, errors.New("bootstrap client is required")
	}
	if strings.TrimSpace(token) == "" {
		// Fail closed: a client without a secret could only talk to an
		// unauthenticated bootstrap, which must never exist.
		return nil, errors.New("bootstrap shared secret is required")
	}
	return &Client{http: client, token: strings.TrimSpace(token)}, nil
}

// Health probes GET /bootstrap/health.
func (c *Client) Health(ctx context.Context, endpoint string) (Health, error) {
	var health Health
	if err := c.call(ctx, endpoint, http.MethodGet, PathHealth, nil, &health); err != nil {
		return Health{}, err
	}
	return health, nil
}

// Status reads GET /bootstrap/status.
func (c *Client) Status(ctx context.Context, endpoint string) (Status, error) {
	var status Status
	if err := c.call(ctx, endpoint, http.MethodGet, PathStatus, nil, &status); err != nil {
		return Status{}, err
	}
	return status, nil
}

// Start hands the role configuration to the bootstrap and starts SGLang.
func (c *Client) Start(ctx context.Context, endpoint string, request StartRequest) error {
	if !request.Role.Valid() {
		return errors.New("bootstrap start requires a prefill or decode role")
	}
	if strings.TrimSpace(request.ModelID) == "" || len(request.ModelID) > domain.MaxModelIDLength {
		return errors.New("bootstrap start requires a valid model id")
	}
	if len(request.ExtraArgs) > 128 {
		return errors.New("bootstrap start accepts at most 128 extra arguments")
	}
	for _, argument := range request.ExtraArgs {
		if strings.ContainsAny(argument, "\x00\r\n") {
			return errors.New("bootstrap extra argument contains control characters")
		}
	}
	return c.call(ctx, endpoint, http.MethodPost, PathStart, request, nil)
}

// Stop asks the bootstrap to stop SGLang and report the final exit code.
func (c *Client) Stop(ctx context.Context, endpoint string, request StopRequest) (StopResult, error) {
	var result StopResult
	if err := c.call(ctx, endpoint, http.MethodPost, PathStop, request, &result); err != nil {
		return StopResult{}, err
	}
	return result, nil
}

func (c *Client) call(ctx context.Context, endpoint, method, path string, body any, destination any) error {
	base := domain.NormalizeEndpoint(endpoint)
	if base == "" {
		return errors.New("bootstrap endpoint is required")
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set(HeaderToken, c.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(payload) > maxResponseBytes {
		return fmt.Errorf("%s %s: bootstrap response is too large", method, path)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s %s returned HTTP %d: %s", method, path, response.StatusCode, strings.TrimSpace(string(payload)))
	}
	if destination == nil || len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, destination); err != nil {
		return fmt.Errorf("%s %s: decode bootstrap response: %w", method, path, err)
	}
	return nil
}
