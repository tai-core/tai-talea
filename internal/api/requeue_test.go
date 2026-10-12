package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestRequeueRouteIsAuthenticatedAndNarrow covers the surface rather than the
// semantics: the state machine rules live in the controller's own tests. What
// matters here is that the verb is reachable with the admin token, that a
// missing instance is a 404 and not a conflict, and that an instance which is
// not parked is refused - the endpoint must not quietly become a second drain.
func TestRequeueRouteIsAuthenticatedAndNarrow(t *testing.T) {
	server, persistence, endpoint := newServer(t)

	// Unauthenticated, both on the admin route and on the console route.
	for _, path := range []string{
		"/v1/capacity/instances/container-1/requeue",
		"/v1/capacity/console/instances/container-1/requeue",
	} {
		request, _ := http.NewRequest("POST", server.URL+path, nil)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 401 {
			t.Fatalf("unauthenticated %s: %d", path, response.StatusCode)
		}
	}

	call := func(path string) (int, string) {
		t.Helper()
		request, _ := http.NewRequest("POST", server.URL+path, nil)
		request.Header.Set("Authorization", "Bearer "+adminToken)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(data)
	}

	if code, _ := call("/v1/capacity/instances/missing/requeue"); code != 404 {
		t.Fatalf("missing instance: %d, want 404", code)
	}

	// A live, healthy instance is not parked: the reconciler owns it and the
	// requeue has no budget to restore.
	body, _ := json.Marshal(map[string]any{
		"event_id": "requeue-add", "partner_id": "partner-a", "type": "CAPACITY_ADDED",
		"occurred_at": time.Now().UTC(),
		"instance":    map[string]any{"id": "container-1", "endpoint": endpoint, "lease_id": "lease-1"},
	})
	if code, _ := push(t, server.URL, body); code != 202 {
		t.Fatalf("add: %d", code)
	}
	code, text := call("/v1/capacity/instances/container-1/requeue")
	if code != 409 || !strings.Contains(text, "IDLE/FAILED") {
		t.Fatalf("requeue of a healthy instance = %d %s, want 409", code, text)
	}
	instance, err := persistence.GetInstance(t.Context(), "container-1")
	if err != nil {
		t.Fatal(err)
	}
	if instance.ServiceState == "FAILED" {
		t.Fatalf("the refused requeue changed the row: %+v", instance)
	}
}
