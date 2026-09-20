package launcher

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFailedProcessIsReachableButUnhealthy(t *testing.T) {
	for _, body := range []string{`{"status":"failed","phase":"FAILED","version":"tai-talea-bootstrap/1","detail":"process exited"}`, `{"error":"upstream unavailable"}`, `not json`} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503); _, _ = w.Write([]byte(body)) }))
		client, _ := New(time.Second, "token")
		health, err := client.Health(t.Context(), s.URL)
		s.Close()
		if body[2:8] == "status" {
			if err != nil || health.Healthy() || health.Phase != PhaseFailed {
				t.Fatalf("%+v %v", health, err)
			}
		} else if err == nil {
			t.Fatal("unstructured 503 must remain an error")
		}
	}
}
