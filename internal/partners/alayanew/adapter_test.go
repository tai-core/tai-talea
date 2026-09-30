package alayanew

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/partner"
)

// fakePlatform mimics the CCI OpenAPI surface used by the adapter.
type fakePlatform struct {
	t          *testing.T
	secret     string
	instances  []map[string]any
	ports      map[string]map[int]string
	results    map[string]map[string]string // key -> result/instanceId
	signedOK   int
	lastCreate map[string]any
}

func (f *fakePlatform) handler(w http.ResponseWriter, r *http.Request) {
	// Verify the HMAC signature: recompute over ts|METHOD|path.
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "alayanew-HMAC-SHA256 ") {
		f.t.Errorf("missing authorization header")
		w.WriteHeader(401)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(auth, "alayanew-HMAC-SHA256 "), ":", 3)
	if len(parts) != 3 {
		f.t.Errorf("malformed authorization: %s", auth)
		w.WriteHeader(401)
		return
	}
	// Authorization = "alayanew-HMAC-SHA256 {ak}:{ts}:{sig}"; STS = "{ts}|{METHOD}|{path}".
	sts := parts[1] + "|" + r.Method + "|" + r.URL.Path
	mac := hmac.New(sha256.New, []byte(f.secret))
	mac.Write([]byte(sts))
	if parts[2] != hex.EncodeToString(mac.Sum(nil)) {
		f.t.Errorf("signature mismatch for %s %s: sts=%s", r.Method, r.URL.Path, sts)
		w.WriteHeader(401)
		return
	}
	f.signedOK++

	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/api/osm/v1/cci/instance/list":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": 200, "message": "OK",
			"data": map[string]any{"records": f.instances, "totalCount": len(f.instances)},
		})
	case strings.HasSuffix(r.URL.Path, "/open-port/list"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/osm/v1/cci/instance/"), "/open-port/list")
		mapped := f.ports[id]
		list := make([]map[string]any, 0, len(mapped))
		for internal, external := range mapped {
			var externalPort int
			_, _ = fmt.Sscanf(external, "%d", &externalPort)
			list = append(list, map[string]any{"port": internal, "externalHost": "203.0.113.9", "externalPort": externalPort})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": 200, "message": "OK",
			"data": map[string]any{"ports": list, "customPorts": []any{}},
		})
	case r.URL.Path == "/api/osm/v1/subscription/CCI/create":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.lastCreate = body
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": 200, "message": "OK", "data": "CCI:SUBSCRIBE:x:1",
		})
	case strings.HasPrefix(r.URL.Path, "/api/osm/v1/subscription/CCI/result/"):
		key := strings.TrimPrefix(r.URL.Path, "/api/osm/v1/subscription/CCI/result/")
		state := f.results[key]
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": 200, "message": "OK",
			"data": map[string]any{"result": state["result"], "instanceId": state["instanceId"], "message": state["message"]},
		})
	default:
		w.WriteHeader(404)
	}
}

func newTestAdapter(t *testing.T, fake *fakePlatform) *Adapter {
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(server.Close)
	adapter, err := New("alayanew", Config{
		AccessKey: "ak-test", SecretKey: fake.secret, Timeout: 5 * time.Second,
		ModelSupport: []string{"zai-org/GLM-5.3-Flash"},
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter.client.baseURL = server.URL
	return adapter
}

func TestSnapshotConvertsRunningInstancesWithPortMapping(t *testing.T) {
	fake := &fakePlatform{
		t:      t,
		secret: "sk-test",
		instances: []map[string]any{
			{"id": "inst-a", "name": "node-a", "status": "RUNNING", "aidcId": 2,
				"resource": map[string]any{"gpuName": "NVIDIA-H100-80G", "gpuCount": 8}},
		},
		ports: map[string]map[int]string{
			"inst-a": {9001: "30086", 9002: "30093"},
		},
	}
	adapter := newTestAdapter(t, fake)

	snapshot, err := partner.CollectSnapshot(context.Background(), adapter)
	if err != nil {
		t.Fatalf("collect snapshot: %v", err)
	}
	if fake.signedOK < 2 {
		t.Fatalf("expected signed requests, got %d", fake.signedOK)
	}
	if len(snapshot.Instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(snapshot.Instances))
	}
	got := snapshot.Instances[0]
	// The operator-chosen name is the managed identity; the platform UUID
	// would look like a replacement container on every pull.
	if got.ID != "node-a" || got.LeaseID != "cci-node-a" {
		t.Errorf("unexpected identity: %+v", got)
	}
	if got.Endpoint != "http://203.0.113.9:30086" || got.ServiceEndpoint != "http://203.0.113.9:30093" {
		t.Errorf("unexpected endpoints: %+v", got)
	}
	if got.Spec.GPU != "NVIDIA-H100-80G" || got.Spec.GPUCount != 8 {
		t.Errorf("unexpected spec: %+v", got.Spec)
	}
	if !got.Spec.SupportsModel("zai-org/GLM-5.3-Flash") {
		t.Errorf("model support not advertised")
	}
}

func TestSnapshotSkipsInstanceWithoutBootstrapPort(t *testing.T) {
	fake := &fakePlatform{
		t: t, secret: "sk-test",
		instances: []map[string]any{
			{"id": "inst-good", "status": "RUNNING", "resource": map[string]any{"gpuName": "H100", "gpuCount": 8}},
			{"id": "inst-bad", "status": "RUNNING", "resource": map[string]any{"gpuName": "H100", "gpuCount": 8}},
		},
		ports: map[string]map[int]string{
			"inst-good": {9001: "30001", 9002: "30002"},
			"inst-bad":  {9999: "39999"}, // no bootstrap port published
		},
	}
	adapter := newTestAdapter(t, fake)
	snapshot, err := partner.CollectSnapshot(context.Background(), adapter)
	if err != nil {
		t.Fatalf("collect snapshot: %v", err)
	}
	if len(snapshot.Instances) != 1 || snapshot.Instances[0].ID != "inst-good" {
		t.Fatalf("expected only inst-good, got %+v", snapshot.Instances)
	}
}

func TestCreateAndWait(t *testing.T) {
	fake := &fakePlatform{
		t: t, secret: "sk-test",
		results: map[string]map[string]string{
			"CCI:SUBSCRIBE:x:1": {"result": "SUCCESS", "instanceId": "inst-new"},
		},
	}
	adapter := newTestAdapter(t, fake)
	adapter.cfg.ProductCode = "PRD-CCI-5-208"
	adapter.cfg.Image = "registry.test/ubuntu:24.04"
	adapter.cfg.ComputeResource = "EXCLUSIVE"
	adapter.cfg.NASStorageID = "nas-1"
	adapter.cfg.NASFileDirectory = "glm53-weights"
	adapter.cfg.NASMountPath = "/root/gongji"
	adapter.cfg.Env = map[string]string{"TAI_TALEA_BOOTSTRAP_TOKEN": "token-1234567890abcdef"}

	key, err := adapter.CreateInstance(context.Background(), "glm53-prefill")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id, err := adapter.WaitCreation(context.Background(), key)
	if err != nil || id != "inst-new" {
		t.Fatalf("wait: id=%s err=%v", id, err)
	}

	create, ok := fake.lastCreate["cciParams"].(map[string]any)
	if !ok {
		t.Fatalf("missing cciParams: %+v", fake.lastCreate)
	}
	if create["computeResourceType"] != "EXCLUSIVE" {
		t.Errorf("computeResourceType not forwarded: %+v", create)
	}
	if create["env"].(map[string]any)["TAI_TALEA_BOOTSTRAP_TOKEN"] == "" {
		t.Errorf("env not forwarded")
	}
	storage := create["storageConfigs"].([]any)[0].(map[string]any)
	if storage["storageId"] != "nas-1" || storage["fileDirectory"] != "glm53-weights" {
		t.Errorf("nas config wrong: %+v", storage)
	}
	if fake.lastCreate["productCode"] != "PRD-CCI-5-208" {
		t.Errorf("productCode wrong: %+v", fake.lastCreate)
	}
}

func TestWaitCreationReportsFailure(t *testing.T) {
	fake := &fakePlatform{
		t: t, secret: "sk-test",
		results: map[string]map[string]string{
			"k": {"result": "FAILED", "instanceId": "", "message": "insufficient quota"},
		},
	}
	adapter := newTestAdapter(t, fake)
	_, err := adapter.WaitCreation(context.Background(), "k")
	if err == nil || !strings.Contains(err.Error(), "insufficient quota") {
		t.Fatalf("expected failure reason, got %v", err)
	}
}

// Compile-time interface check against the real adapter contract.
var _ partner.PartnerAdapter = (*Adapter)(nil)
