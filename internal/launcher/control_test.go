package launcher

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
)

func TestBootstrapNeverFollowsRedirectWithSharedSecret(t *testing.T) {
	var reached atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Store(true) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client, _ := New(time.Second, "deployment-shared-secret")
	if _, err := client.Health(t.Context(), source.URL); err == nil {
		t.Fatal("redirect must be rejected")
	}
	if reached.Load() {
		t.Fatal("authenticated bootstrap request followed a redirect")
	}
}

func TestAlreadyRunningRequiresMatchingRoleAndModel(t *testing.T) {
	for _, tc := range []struct {
		role, model string
		adopt       bool
	}{{"prefill", "model-a", true}, {"decode", "model-a", false}, {"prefill", "model-b", false}, {"", "", false}} {
		t.Run(tc.role+tc.model, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == PathStart {
					w.WriteHeader(http.StatusUnprocessableEntity)
					return
				}
				fmt.Fprintf(w, `{"phase":"RUNNING","role":%q,"model_id":%q}`, tc.role, tc.model)
			}))
			defer server.Close()
			client, _ := New(time.Second, "token")
			err := client.Start(t.Context(), server.URL, StartRequest{Role: domain.RolePrefill, ModelID: "model-a"})
			if err == nil || errors.Is(err, ErrAlreadyRunning) != tc.adopt {
				t.Fatalf("unexpected adoption result: %v", err)
			}
		})
	}
}
