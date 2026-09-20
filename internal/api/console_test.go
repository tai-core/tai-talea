package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestConsoleAndAuthenticatedReclaim(t *testing.T) {
	server, persistence, endpoint := newServer(t)
	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	html, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(html), "节点管理") || strings.Contains(string(html), adminToken) {
		t.Fatal("console shell must be available without exposing credentials")
	}
	if !strings.Contains(response.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("missing console CSP")
	}
	for _, path := range []string{"/v1/capacity/console", "/v1/capacity/console/telemetry", "/v1/capacity/instances/container-1/reclaim"} {
		method := "GET"
		if strings.HasSuffix(path, "reclaim") {
			method = "POST"
		}
		req, _ := http.NewRequest(method, server.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("unauthenticated %s: %d", path, resp.StatusCode)
		}
	}
	body, _ := json.Marshal(map[string]any{"event_id": "console-add", "partner_id": "partner-a", "type": "CAPACITY_ADDED", "occurred_at": time.Now().UTC(), "instance": map[string]any{"id": "container-1", "endpoint": endpoint, "lease_id": "lease-1"}})
	if code, _ := push(t, server.URL, body); code != 202 {
		t.Fatalf("add: %d", code)
	}
	call := func(method, path, body string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, data
	}
	for _, bad := range []string{`{"grace_seconds":-1}`, `{"grace_seconds":86401}`, `{} {}`, `{"unknown":1}`} {
		if code, _ := call("POST", "/v1/capacity/instances/container-1/reclaim", bad); code != 400 {
			t.Fatalf("invalid grace/payload accepted: %s", bad)
		}
	}
	for i := 0; i < 2; i++ {
		if code, _ := call("POST", "/v1/capacity/instances/container-1/reclaim", `{"grace_seconds":60}`); code != 202 {
			t.Fatalf("reclaim: %d", code)
		}
	}
	instance, err := persistence.GetInstance(t.Context(), "container-1")
	if err != nil || !instance.PendingRelease {
		t.Fatalf("intent not persisted: %+v %v", instance, err)
	}
	if code, _ := call("POST", "/v1/capacity/reconcile", `{}`); code != 200 {
		t.Fatalf("reconcile: %d", code)
	}
	instance, _ = persistence.GetInstance(t.Context(), "container-1")
	if instance.InstanceState != "RELEASED" {
		t.Fatalf("expected completed reclaim: %+v", instance)
	}
	if code, _ := call("POST", "/v1/capacity/instances/missing/reclaim", `{}`); code != 404 {
		t.Fatalf("missing node: %d", code)
	}
	code, data := call("GET", "/v1/capacity/console", "")
	if code != 200 || strings.Contains(string(data), adminToken) || strings.Contains(string(data), partnerSecret) {
		t.Fatal("console metadata leaks credentials")
	}
}
