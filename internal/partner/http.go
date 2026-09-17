package partner

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
)

// maxPartnerResponseBytes bounds what a partner API can return.
const maxPartnerResponseBytes = 8 << 20

// HTTPAdapterConfig configures the generic HTTP PartnerAdapter. Partner
// platforms that expose a plain JSON capacity API can be onboarded without
// writing code; anything else implements PartnerAdapter directly.
type HTTPAdapterConfig struct {
	// PartnerID is the stable partner identifier.
	PartnerID string `json:"partner_id" yaml:"partner_id"`
	// BaseURL is the partner API origin, for example https://partner-api:8443.
	BaseURL string `json:"base_url" yaml:"base_url"`
	// Token is the partner credential. It is stored per partner and never
	// logged.
	Token string `json:"token,omitempty" yaml:"token,omitempty"`
	// AvailablePath defaults to /instances/available.
	AvailablePath string `json:"available_path,omitempty" yaml:"available_path,omitempty"`
	// LeasesPath defaults to /instances/leases.
	LeasesPath string `json:"leases_path,omitempty" yaml:"leases_path,omitempty"`
	// ReleasePathTemplate defaults to /instances/{id}/release.
	ReleasePathTemplate string `json:"release_path_template,omitempty" yaml:"release_path_template,omitempty"`
	// Timeout bounds each partner call.
	Timeout time.Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

// Validate checks the adapter configuration.
func (c HTTPAdapterConfig) Validate() error {
	if strings.TrimSpace(c.PartnerID) == "" {
		return errors.New("partner id is required")
	}
	parsed, err := url.Parse(c.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return errors.New("partner base url must be an http(s) origin without credentials")
	}
	if c.Timeout < 0 || c.Timeout > 60*time.Second {
		return errors.New("partner timeout must be between zero and 60 seconds")
	}
	return nil
}

// HTTPAdapter implements PartnerAdapter over a JSON HTTP API.
type HTTPAdapter struct {
	config HTTPAdapterConfig
	http   *http.Client
}

// NewHTTPAdapter validates the configuration and builds the adapter.
func NewHTTPAdapter(config HTTPAdapterConfig) (*HTTPAdapter, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.AvailablePath == "" {
		config.AvailablePath = "/instances/available"
	}
	if config.LeasesPath == "" {
		config.LeasesPath = "/instances/leases"
	}
	if config.ReleasePathTemplate == "" {
		config.ReleasePathTemplate = "/instances/{id}/release"
	}
	if config.Timeout == 0 {
		config.Timeout = 15 * time.Second
	}
	return &HTTPAdapter{config: config, http: &http.Client{Timeout: config.Timeout}}, nil
}

// PartnerID implements PartnerAdapter.
func (a *HTTPAdapter) PartnerID() string { return a.config.PartnerID }

// ListAvailableInstances implements PartnerAdapter.
func (a *HTTPAdapter) ListAvailableInstances(ctx context.Context) ([]CapacityInstance, error) {
	return a.list(ctx, a.config.AvailablePath)
}

// ListActiveLeases implements PartnerAdapter.
func (a *HTTPAdapter) ListActiveLeases(ctx context.Context) ([]CapacityInstance, error) {
	return a.list(ctx, a.config.LeasesPath)
}

func (a *HTTPAdapter) list(ctx context.Context, path string) ([]CapacityInstance, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(a.config.BaseURL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	a.authorize(request)
	request.Header.Set("Accept", "application/json")

	response, err := a.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: GET %s: %v", ErrPartnerUnavailable, path, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxPartnerResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > maxPartnerResponseBytes {
		return nil, fmt.Errorf("%w: GET %s: response too large", ErrPartnerUnavailable, path)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: GET %s returned HTTP %d", ErrPartnerUnavailable, path, response.StatusCode)
	}
	var body struct {
		Instances []CapacityInstance `json:"instances"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: decode %s: %v", ErrSnapshotIncomplete, path, err)
	}
	return body.Instances, nil
}

// ReleaseInstance implements PartnerAdapter.
func (a *HTTPAdapter) ReleaseInstance(ctx context.Context, instanceID string) error {
	if strings.TrimSpace(instanceID) == "" || strings.ContainsAny(instanceID, "/\x00\r\n") {
		return errors.New("release requires a valid instance id")
	}
	path := strings.ReplaceAll(a.config.ReleasePathTemplate, "{id}", url.PathEscape(instanceID))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(a.config.BaseURL, "/")+path, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	a.authorize(request)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")

	response, err := a.http.Do(request)
	if err != nil {
		return fmt.Errorf("%w: release %s: %v", ErrPartnerUnavailable, instanceID, err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusConflict {
		// Already gone is an idempotent success for release.
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("release %s returned HTTP %d", instanceID, response.StatusCode)
	}
	return nil
}

func (a *HTTPAdapter) authorize(request *http.Request) {
	if a.config.Token != "" {
		request.Header.Set("Authorization", "Bearer "+a.config.Token)
	}
}
