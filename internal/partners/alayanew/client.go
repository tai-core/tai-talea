// Package alayanew implements a PartnerAdapter for 九章智算云 (Alaya NeW Cloud)
// CCI instances. It talks to the OpenAPI at https://api.alayanew.com with the
// platform's HMAC-SHA256 request signing and adds on-demand instance creation,
// which the generic read-mostly adapter contract cannot express.
//
// Wire notes verified against the live platform (2026-09-29/30, 2026-10-13):
//   - Every path is prefixed with /api/osm/v1. Omitting the version segment
//     does not return a 404 about the path: it returns HTTP 200 carrying
//     {"message":"Invalid AccessKey"}, which reads like a dead credential and
//     sends you hunting for a key that is perfectly valid. A bare /api/osm
//     prefix returns HTML instead. Check the prefix before the key.
//   - StringToSign is "{ts_ms}|{METHOD}|{path}" with no query string or host.
//   - The subscription result key contains colons; it must be appended to the
//     path verbatim (no percent-encoding) or the signature mismatches.
package alayanew

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.alayanew.com"

// Client is a signed HTTP client for the CCI OpenAPI.
type Client struct {
	baseURL  string
	accessKA string
	secretKA string
	http     *http.Client
}

// NewClient builds a client. timeout bounds a single request.
func NewClient(accessKey, secretKey string, timeout time.Duration) *Client {
	return &Client{
		baseURL:  defaultBaseURL,
		accessKA: accessKey,
		secretKA: secretKey,
		http:     &http.Client{Timeout: timeout},
	}
}

func (c *Client) sign(method, path string, ts int64) string {
	sts := fmt.Sprintf("%d|%s|%s", ts, strings.ToUpper(method), path)
	mac := hmac.New(sha256.New, []byte(c.secretKA))
	mac.Write([]byte(sts))
	return fmt.Sprintf("alayanew-HMAC-SHA256 %s:%d:%s", c.accessKA, ts, hex.EncodeToString(mac.Sum(nil)))
}

// apiError is a platform error carrying the business status code.
type apiError struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
}

func (e *apiError) Error() string {
	return fmt.Sprintf("alayanew api status %d: %s", e.Status, e.Message)
}

// statusNameInUse is the business status for "this enterprise already has an
// instance with that name" (message: 本企业内有重复实例名称，请更换).
//
// It is not a validation error the caller can avoid by checking first: a
// released container keeps its name reserved on the platform and is no longer
// listed by any endpoint, so the name looks free right up until the create
// call refuses it. Names are therefore single-use, and the only way through a
// collision is a different name.
const statusNameInUse = 212035

// call performs one signed request and decodes the JSON envelope into out.
// path must start with /api/osm; query is appended verbatim.
func (c *Client) call(ctx context.Context, method, path, query string, body any, out any) error {
	url := c.baseURL + path
	if query != "" {
		url += "?" + query
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("Authorization", c.sign(method, path, time.Now().UnixMilli()))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	var envelope struct {
		Status  int             `json:"status"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode response (http %d): %w", resp.StatusCode, err)
	}
	// The platform reports business failures with HTTP 200 plus a status code.
	if envelope.Status != 200 {
		return &apiError{Status: envelope.Status, Message: envelope.Message}
	}
	if out != nil && len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("decode data: %w", err)
		}
	}
	return nil
}

// FlexInt accepts both JSON numbers and numeric strings: the platform API
// returns gpuCount as a string on some endpoints and as a number on others.
type FlexInt int

func (f *FlexInt) UnmarshalJSON(data []byte) error {
	text := strings.Trim(string(data), "\"")
	if text == "" || text == "null" {
		*f = 0
		return nil
	}
	value, err := strconv.Atoi(text)
	if err != nil {
		return fmt.Errorf("not a number: %q", text)
	}
	*f = FlexInt(value)
	return nil
}

// InstanceRecord is one entry of the instance list.
type InstanceRecord struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	AIDCId   int    `json:"aidcId"`
	Image    string `json:"image"`
	Resource struct {
		GPUName  string  `json:"gpuName"`
		GPUCount FlexInt `json:"gpuCount"`
		CPUCores string  `json:"cpuCores"`
		MemoryGB string  `json:"memoryGB"`
	} `json:"resource"`
}

// listInstances returns the pages of instances with the given status filter.
func (c *Client) listInstances(ctx context.Context, aidcID int, status string) ([]InstanceRecord, error) {
	var records []InstanceRecord
	for page := 1; page <= 50; page++ {
		query := fmt.Sprintf("pageNo=%d&pageSize=100", page)
		if aidcID > 0 {
			query += fmt.Sprintf("&aidcId=%d", aidcID)
		}
		if status != "" {
			query += "&status=" + status
		}
		var out struct {
			Records    []InstanceRecord `json:"records"`
			TotalCount int              `json:"totalCount"`
		}
		if err := c.call(ctx, "GET", "/api/osm/v1/cci/instance/list", query, nil, &out); err != nil {
			return nil, err
		}
		records = append(records, out.Records...)
		if len(out.Records) < 100 || (out.TotalCount > 0 && len(records) >= out.TotalCount) {
			break
		}
	}
	return records, nil
}

// portRecord is one port mapping entry.
type portRecord struct {
	Port         int    `json:"port"`
	ExternalHost string `json:"externalHost"`
	ExternalPort int    `json:"externalPort"`
}

// portMappings returns the published port map of one instance.
func (c *Client) portMappings(ctx context.Context, instanceID string) (map[int]string, error) {
	var out struct {
		Ports       []portRecord `json:"ports"`
		CustomPorts []portRecord `json:"customPorts"`
	}
	path := "/api/osm/v1/cci/instance/" + instanceID + "/open-port/list"
	if err := c.call(ctx, "GET", path, "", nil, &out); err != nil {
		return nil, err
	}
	mapped := make(map[int]string)
	for _, record := range append(append([]portRecord{}, out.Ports...), out.CustomPorts...) {
		if record.ExternalHost != "" && record.ExternalPort > 0 {
			mapped[record.Port] = fmt.Sprintf("http://%s:%d", record.ExternalHost, record.ExternalPort)
		}
	}
	return mapped, nil
}

// createInstance submits a subscription and returns the result key.
func (c *Client) createInstance(ctx context.Context, productCode string, cciParams map[string]any) (string, error) {
	var key string
	body := map[string]any{"productCode": productCode, "cciParams": cciParams}
	if err := c.call(ctx, "POST", "/api/osm/v1/subscription/CCI/create", "", body, &key); err != nil {
		return "", err
	}
	return key, nil
}

// subscriptionResource is one allocated resource inside a subscription result.
// The instance id lives here, not at the top level: the platform renamed
// instanceId to resourceInstanceId when it moved the endpoint.
type subscriptionResource struct {
	Status             string `json:"status"`
	SubscriptionNo     string `json:"subscriptionNo"`
	ResourceInstanceID string `json:"resourceInstanceId"`
	// ErrorCode is null on success and a business code on rejection (40010 is
	// the quota refusal). A plain int decodes null as 0, which is why the
	// success case needs no pointer.
	ErrorCode    int    `json:"errorCode"`
	ErrorMessage string `json:"errorMessage"`
}

// subscriptionResult is one poll of a subscription.
//
// The platform decommissioned GET /api/osm/v1/subscription/CCI/result/{key}
// (it answers status 212147, "该接口已停用") in favour of
// GET /api/osm/v1/subscription/sku/cci/result/{key}, and changed the payload
// with it. The old body was {result, message, instanceId}; the new one nests
// everything under data.resources[]. Verified live on 2026-10-27 against a CPU
// subscription, one second after the create call:
//
//	{"status":200,"message":"OK","data":{
//	  "status":"SUCCESS","startAt":"2026-10-27 14:48:10",
//	  "resources":[{"status":"SUCCESS","subscriptionNo":"ORD...",
//	    "resourceInstanceId":"647e8613-...","resourceId":"647e8613-...",
//	    "errorCode":null,"errorMessage":null}]}}
//
// Read what SUCCESS means here: the container had not started yet when that
// body was captured. It says the subscription was fulfilled and an id was
// minted, not that the container is RUNNING - that stays AwaitSSHAccess's
// question, and it already holds its budget while the platform is queueing.
type subscriptionResult struct {
	Status    string                 `json:"status"`
	Resources []subscriptionResource `json:"resources"`
}

// instanceID returns the first container id the subscription minted, if any.
func (r subscriptionResult) instanceID() string {
	for _, resource := range r.Resources {
		if resource.ResourceInstanceID != "" {
			return resource.ResourceInstanceID
		}
	}
	return ""
}

// rejection returns the platform's own refusal text, or "" when the
// subscription has not been refused. Reporting the raw message matters: a
// quota refusal carries "您的剩余配额为：0，已达上限：3" and the code 40010, which
// tells an operator what to do; "creation failed" tells them nothing.
func (r subscriptionResult) rejection() string {
	for _, resource := range r.Resources {
		if resource.ErrorCode != 0 || resource.ErrorMessage != "" {
			return fmt.Sprintf("status %d: %s", resource.ErrorCode, resource.ErrorMessage)
		}
	}
	if r.Status == "FAILED" || r.Status == "ERROR" {
		return "the subscription did not produce a container"
	}
	return ""
}

func (c *Client) subscriptionResultOnce(ctx context.Context, key string) (subscriptionResult, error) {
	var out subscriptionResult
	// The key contains colons; keep it verbatim in the path or the signature
	// will mismatch (verified against the live platform).
	err := c.call(ctx, "GET", "/api/osm/v1/subscription/sku/cci/result/"+key, "", nil, &out)
	return out, err
}

// releaseInstance reclaims one container immediately.
func (c *Client) releaseInstance(ctx context.Context, instanceID string) error {
	path := "/api/osm/v1/cci/instance/" + instanceID + "/release"
	return c.call(ctx, "POST", path, "", nil, nil)
}

// instanceStatus returns the raw lifecycle status of one instance.
func (c *Client) instanceStatus(ctx context.Context, instanceID string) (string, error) {
	var out struct {
		Status string `json:"status"`
	}
	path := "/api/osm/v1/cci/instance/" + instanceID
	if err := c.call(ctx, "GET", path, "", nil, &out); err != nil {
		return "", err
	}
	return out.Status, nil
}

// sshInfo is the get-ssh response payload.
type sshInfo struct {
	URL      string `json:"url"`
	Password string `json:"password"`
}

// getSSH returns the SSH connection details of one instance.
func (c *Client) getSSH(ctx context.Context, instanceID string) (sshInfo, error) {
	var out sshInfo
	path := "/api/osm/v1/cci/instance/" + instanceID + "/get-ssh"
	if err := c.call(ctx, "POST", path, "", nil, &out); err != nil {
		return sshInfo{}, err
	}
	return out, nil
}
