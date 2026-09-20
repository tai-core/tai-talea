package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/api"
	"github.com/tai-core/tai-talea/internal/config"
	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/onboarding"
)

func TestConsolePartnerScopeAndAsyncOnboarding(t *testing.T) {
	const tokenA = "console-partner-a-012345"
	const tokenB = "console-partner-b-012345"
	var jobs *onboarding.Manager
	server, _, endpoint := newServer(t, func(o *api.Options) {
		o.Config.Partners[0].ConsoleToken = tokenA
		o.Config.Partners = append(o.Config.Partners, config.PartnerConfig{ID: "partner-b", Adapter: "static", PushToken: "partner-b-token", PushSecret: "partner-b-secret-012345", ConsoleToken: tokenB, HMACTolerance: config.Duration(time.Minute)})
		var err error
		jobs, err = onboarding.New(filepath.Join(t.TempDir(), "jobs"), o.Store, func(context.Context, onboarding.Request, func(string) error) (domain.Instance, error) {
			t.Fatal("HTTP submit must not run installation")
			return domain.Instance{}, nil
		}, o.Controller.EnrollPrepared)
		if err != nil {
			t.Fatal(err)
		}
		o.Onboarding = jobs
	})
	body, _ := json.Marshal(domain.CapacityEvent{EventID: "scope-add", PartnerID: "partner-a", Type: domain.EventCapacityAdded, OccurredAt: time.Now(), Instance: &domain.EventInstance{ID: "container-1", Endpoint: endpoint, LeaseID: "l1"}})
	if code, _ := push(t, server.URL, body); code != 202 {
		t.Fatal(code)
	}
	call := func(token, method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}
	for _, path := range []string{"/v1/capacity/console/instances/container-1", "/v1/capacity/console/instances/container-1/audit"} {
		if code, _ := call(tokenB, "GET", path, ""); code != 404 {
			t.Fatalf("cross-owner %s: %d", path, code)
		}
	}
	if code, _ := call(tokenB, "POST", "/v1/capacity/console/instances/container-1/reclaim", "{}"); code != 404 {
		t.Fatal(code)
	}
	if _, data := call(tokenB, "GET", "/v1/capacity/console/instances", ""); strings.Contains(data, "container-1") {
		t.Fatal("foreign node leaked")
	}
	if code, _ := call(tokenA, "GET", "/v1/capacity/events", ""); code != 401 {
		t.Fatal("partner read administrator events")
	}
	if code, _ := call(partnerToken, "GET", "/v1/capacity/console", ""); code != 401 {
		t.Fatal("push token bypasses HMAC through console")
	}
	if _, data := call(tokenA, "GET", "/v1/capacity/console", ""); strings.Contains(data, tokenA) || strings.Contains(data, "partner-b") {
		t.Fatal("metadata leaks")
	}
	payload := `{"request_id":"online1","id":"container-2","host":"10.0.0.2","port":22,"user":"root","password":"ssh-secret"}`
	for i := 0; i < 2; i++ {
		code, data := call(tokenA, "POST", "/v1/capacity/console/onboarding", payload)
		if code != 202 || strings.Contains(data, "ssh-secret") {
			t.Fatalf("submission: %d %s", code, data)
		}
	}
	if len(jobs.List("partner-a")) != 1 || len(jobs.List("partner-b")) != 0 {
		t.Fatal("jobs not scoped/deduplicated")
	}
	if code, _ := call(tokenB, "POST", "/v1/capacity/console/onboarding", payload); code != 409 {
		t.Fatal("cross-partner request reuse")
	}
	if code, _ := call(tokenA, "POST", "/v1/capacity/console/onboarding", strings.Replace(payload, `"id":"container-2"`, `"id":"container-2","partner_id":"partner-b"`, 1)); code != 403 {
		t.Fatal("forged partner accepted")
	}
	if code, _ := call("", "POST", "/v1/capacity/console/onboarding", payload); code != 401 {
		t.Fatal("missing authentication")
	}
}
