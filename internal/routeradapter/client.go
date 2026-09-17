// Package routeradapter talks to the sglang-router control plane.
//
// Contract used here is the one documented in sglang-router/README.md and
// exercised by dago/internal/routerlegacy:
//
//	POST   /workers                     -> 202 {worker_id, url}
//	GET    /workers                     -> 200 {workers:[{id,url,worker_type,is_healthy,load}]}
//	GET    /workers/{worker_id}         -> 200 worker info
//	GET    /state/workers               -> 200 [WorkerReadinessRecord]
//	GET    /state/workers/{worker_id}   -> 200 WorkerReadinessRecord
//	PUT    /state/workers/{worker_id}   -> 200 {previous_state, record, changed}
//	GET    /get_loads                   -> 200 {loads:[{worker,worker_type,load}]}
//
// Draining is expressed exclusively through readiness. DELETE /workers is
// deliberately not exposed: development document §8 forbids it as a drain
// primitive.
package routeradapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
)

const (
	// ModeReadinessV1 is the only supported Router control mode.
	ModeReadinessV1 = "readiness-v1"

	readinessReady       = "ready"
	readinessUnavailable = "unavailable"

	maxResponseBytes = 4 << 20
)

// Errors returned by the adapter.
var (
	// ErrWorkerNotFound means the Router does not know the worker id.
	ErrWorkerNotFound = errors.New("router worker not found")
	// ErrAlreadyRegistered means the Router already tracks the worker URL.
	ErrAlreadyRegistered = errors.New("router worker url already registered")
	// ErrReadinessNotVisible means the readiness record is not projected yet.
	ErrReadinessNotVisible = errors.New("router readiness record is not visible yet")
)

// StatusError carries a non-2xx Router response.
type StatusError struct {
	Path       string
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s returned HTTP %d", e.Path, e.StatusCode)
}

// Config describes one Router managed by the control plane. Milestone 1
// supports exactly one Router, but the type keeps the M2 multi-Router path open.
type Config struct {
	Name    string        `json:"name" yaml:"name"`
	URL     string        `json:"url" yaml:"url"`
	Mode    string        `json:"mode,omitempty" yaml:"mode,omitempty"`
	Timeout time.Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

// EffectiveMode returns the mode with the safe default applied.
func (c Config) EffectiveMode() string {
	if c.Mode == "" {
		return ModeReadinessV1
	}
	return c.Mode
}

// Validate rejects Router configurations the adapter cannot honour.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Name) == "" || len(c.Name) > 63 {
		return errors.New("router name is required and must be at most 63 characters")
	}
	parsed, err := url.Parse(c.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return errors.New("router url must be an http(s) origin without credentials, path, query or fragment")
	}
	if c.EffectiveMode() != ModeReadinessV1 {
		return fmt.Errorf("router mode must be %s", ModeReadinessV1)
	}
	if c.Timeout < 0 || c.Timeout > 30*time.Second {
		return errors.New("router timeout must be between zero and 30 seconds")
	}
	return nil
}

// RegisterRequest is one dynamic worker registration.
type RegisterRequest struct {
	WorkerURL  string
	WorkerType domain.Role
	ModelID    string
	APIKey     string
}

// Registration is the accepted worker identity.
type Registration struct {
	WorkerID  string `json:"worker_id"`
	WorkerURL string `json:"url"`
}

// ReadinessRecord mirrors the Router's WorkerReadinessRecord projection.
type ReadinessRecord struct {
	WorkerID          string `json:"worker_id"`
	WorkerURL         string `json:"worker_url"`
	State             string `json:"state"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
	Generation        uint64 `json:"generation"`
	UpdatedAtUnixMS   uint64 `json:"updated_at_unix_ms"`
}

// Ready reports whether the Router will route new requests to this worker.
func (r ReadinessRecord) Ready() bool { return r.State == readinessReady }

// ReadinessTransition is the result of an atomic readiness change.
type ReadinessTransition struct {
	PreviousState string          `json:"previous_state"`
	Record        ReadinessRecord `json:"record"`
	Changed       bool            `json:"changed"`
}

// Worker is a Router membership entry.
type Worker struct {
	ID         string `json:"id"`
	URL        string `json:"url"`
	WorkerType string `json:"worker_type"`
	Healthy    bool   `json:"is_healthy"`
	Load       int64  `json:"load"`
}

// WorkerLoad is one entry of GET /get_loads. A negative load means the Router
// could not read the load from the worker.
type WorkerLoad struct {
	Worker     string `json:"worker"`
	WorkerType string `json:"worker_type"`
	Load       int64  `json:"load"`
}

// Client is a RouterAdapter bound to one Router.
type Client struct {
	config Config
	http   *http.Client
}

// New validates the configuration and builds a client.
func New(config Config) (*Client, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.Timeout == 0 {
		config.Timeout = 10 * time.Second
	}
	return &Client{config: config, http: &http.Client{Timeout: config.Timeout}}, nil
}

// Name returns the configured Router name.
func (c *Client) Name() string { return c.config.Name }

// BaseURL returns the Router origin.
func (c *Client) BaseURL() string { return strings.TrimSuffix(c.config.URL, "/") }

// RegisterWorker dynamically registers a Prefill or Decode worker. The Router
// starts with an empty pool, so this is the only way a worker becomes routable.
func (c *Client) RegisterWorker(ctx context.Context, request RegisterRequest) (Registration, error) {
	if !request.WorkerType.Valid() {
		return Registration{}, errors.New("worker type must be prefill or decode")
	}
	workerURL, err := url.Parse(request.WorkerURL)
	if err != nil || (workerURL.Scheme != "http" && workerURL.Scheme != "https") || workerURL.Host == "" ||
		workerURL.User != nil || workerURL.RawQuery != "" || workerURL.Fragment != "" || workerURL.Path != "" {
		return Registration{}, errors.New("worker url must be an http(s) origin")
	}
	if strings.TrimSpace(request.ModelID) != request.ModelID || request.ModelID == "" ||
		len(request.ModelID) > domain.MaxModelIDLength || strings.ContainsAny(request.ModelID, "\x00\r\n") {
		return Registration{}, errors.New("model id is invalid")
	}

	body := map[string]string{
		"url":         request.WorkerURL,
		"worker_type": string(request.WorkerType),
		"model_id":    request.ModelID,
	}
	if request.APIKey != "" {
		body["api_key"] = request.APIKey
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return Registration{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL()+"/workers", bytes.NewReader(encoded))
	if err != nil {
		return Registration{}, err
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")

	var response Registration
	if err := c.doJSON(httpRequest, http.StatusAccepted, &response); err != nil {
		var statusErr *StatusError
		if errors.As(err, &statusErr) && (statusErr.StatusCode == http.StatusConflict ||
			statusErr.StatusCode == http.StatusBadRequest) && looksLikeAlreadyRegistered(statusErr.Body) {
			return Registration{}, fmt.Errorf("%w: %s", ErrAlreadyRegistered, request.WorkerURL)
		}
		return Registration{}, err
	}
	if strings.TrimSpace(response.WorkerID) == "" || len(response.WorkerID) > 2048 ||
		strings.ContainsAny(response.WorkerID, "\x00\r\n") || response.WorkerURL != request.WorkerURL {
		return Registration{}, errors.New("router returned an invalid registration")
	}
	return response, nil
}

func looksLikeAlreadyRegistered(body string) bool {
	lowered := strings.ToLower(body)
	return strings.Contains(lowered, "exist") || strings.Contains(lowered, "duplicate") ||
		strings.Contains(lowered, "already")
}

// GetWorker reads one Router membership.
func (c *Client) GetWorker(ctx context.Context, workerID string) (Worker, error) {
	if err := c.validateWorkerID(workerID); err != nil {
		return Worker{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL()+"/workers/"+url.PathEscape(workerID), nil)
	if err != nil {
		return Worker{}, err
	}
	httpRequest.Header.Set("Accept", "application/json")
	var worker Worker
	if err := c.doJSON(httpRequest, http.StatusOK, &worker); err != nil {
		var statusErr *StatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
			return Worker{}, fmt.Errorf("%w: %s", ErrWorkerNotFound, workerID)
		}
		return Worker{}, err
	}
	if worker.ID == "" {
		worker.ID = workerID
	}
	if worker.URL == "" {
		return Worker{}, errors.New("router returned a worker without url")
	}
	return worker, nil
}

// FindWorkerByURL locates an existing membership by worker URL. It is used to
// recover when the Router already tracks a URL we are about to register.
func (c *Client) FindWorkerByURL(ctx context.Context, workerURL string) (Worker, bool, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL()+"/workers", nil)
	if err != nil {
		return Worker{}, false, err
	}
	httpRequest.Header.Set("Accept", "application/json")
	var response struct {
		Workers []Worker `json:"workers"`
	}
	if err := c.doJSON(httpRequest, http.StatusOK, &response); err != nil {
		return Worker{}, false, err
	}
	target := domain.NormalizeEndpoint(workerURL)
	for _, worker := range response.Workers {
		if domain.NormalizeEndpoint(worker.URL) == target {
			return worker, true, nil
		}
	}
	return Worker{}, false, nil
}

// GetReadiness reads the shared readiness record of one worker.
func (c *Client) GetReadiness(ctx context.Context, workerID string) (ReadinessRecord, error) {
	if err := c.validateWorkerID(workerID); err != nil {
		return ReadinessRecord{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL()+"/state/workers/"+url.PathEscape(workerID), nil)
	if err != nil {
		return ReadinessRecord{}, err
	}
	httpRequest.Header.Set("Accept", "application/json")
	var record ReadinessRecord
	if err := c.doJSON(httpRequest, http.StatusOK, &record); err != nil {
		var statusErr *StatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
			return ReadinessRecord{}, fmt.Errorf("%w: %s", ErrReadinessNotVisible, workerID)
		}
		return ReadinessRecord{}, err
	}
	return record, validateReadinessRecord(record, workerID)
}

// ListReadiness lists every shared readiness record.
func (c *Client) ListReadiness(ctx context.Context) ([]ReadinessRecord, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL()+"/state/workers", nil)
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Accept", "application/json")
	var records []ReadinessRecord
	if err := c.doJSON(httpRequest, http.StatusOK, &records); err != nil {
		return nil, err
	}
	return records, nil
}

// SetReadiness opens or closes routing for one worker. Closing readiness is the
// control plane's only drain primitive.
func (c *Client) SetReadiness(ctx context.Context, workerID string, ready bool) (ReadinessTransition, error) {
	if err := c.validateWorkerID(workerID); err != nil {
		return ReadinessTransition{}, err
	}
	state := readinessUnavailable
	if ready {
		state = readinessReady
	}
	body, err := json.Marshal(map[string]string{"state": state, "reason": "manual"})
	if err != nil {
		return ReadinessTransition{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.BaseURL()+"/state/workers/"+url.PathEscape(workerID), bytes.NewReader(body))
	if err != nil {
		return ReadinessTransition{}, err
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")

	var transition ReadinessTransition
	if err := c.doJSON(httpRequest, http.StatusOK, &transition); err != nil {
		var statusErr *StatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
			return ReadinessTransition{}, fmt.Errorf("%w: %s", ErrWorkerNotFound, workerID)
		}
		return ReadinessTransition{}, err
	}
	if err := validateReadinessRecord(transition.Record, workerID); err != nil {
		return ReadinessTransition{}, err
	}
	if transition.Record.State != state {
		return ReadinessTransition{}, fmt.Errorf("router did not enter requested readiness state %s", state)
	}
	return transition, nil
}

// GetLoads returns the per-worker load snapshot used by the PD planner input.
func (c *Client) GetLoads(ctx context.Context) ([]WorkerLoad, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL()+"/get_loads", nil)
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Accept", "application/json")
	var response struct {
		Loads []WorkerLoad `json:"loads"`
	}
	if err := c.doJSON(httpRequest, http.StatusOK, &response); err != nil {
		return nil, err
	}
	return response.Loads, nil
}

func (c *Client) validateWorkerID(workerID string) error {
	if strings.TrimSpace(workerID) == "" || len(workerID) > 2048 || strings.ContainsAny(workerID, "\x00\r\n") {
		return errors.New("router worker id is invalid")
	}
	return nil
}

func validateReadinessRecord(record ReadinessRecord, workerID string) error {
	if record.WorkerID != workerID || record.WorkerURL == "" || record.Generation == 0 {
		return errors.New("router returned an invalid readiness record")
	}
	if record.State != readinessReady && record.State != readinessUnavailable {
		return fmt.Errorf("router returned unsupported readiness state %q", record.State)
	}
	if record.State == readinessUnavailable && record.UnavailableReason == "" {
		return errors.New("router unavailable readiness has no reason")
	}
	return nil
}

func (c *Client) doJSON(request *http.Request, wantStatus int, destination any) error {
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if readErr != nil {
		return readErr
	}
	if len(payload) > maxResponseBytes {
		return errors.New("router response is too large")
	}
	if response.StatusCode != wantStatus {
		return &StatusError{Path: request.URL.Path, StatusCode: response.StatusCode, Body: string(payload)}
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode router response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("router response contains trailing json")
	}
	return nil
}
