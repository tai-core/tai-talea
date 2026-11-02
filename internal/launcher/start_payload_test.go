package launcher

import (
	"encoding/json"
	"strings"
	"testing"
)

// The environment contract travels in the start payload, and the two sides of
// it must never drift: a wheelhouse deployment omits the field (the bootstrap
// installs), a prebuilt deployment sends an explicit false (the bootstrap must
// trust the virtualenv it finds and must not go looking for a wheelhouse that
// is not there).
func TestStartRequestCarriesTheEnvironmentContract(t *testing.T) {
	cases := []struct {
		name     string
		request  StartRequest
		contains string
		absent   string
	}{
		{
			name:     "nil keeps the bootstrap default and stays off the wire",
			request:  StartRequest{Role: "prefill", ModelID: "m"},
			contains: `"role"`,
			absent:   "prepare_environment",
		},
		{
			name:     "prebuilt sends an explicit false",
			request:  StartRequest{Role: "prefill", ModelID: "m", PrepareEnvironment: ptr(false)},
			contains: `"prepare_environment":false`,
		},
		{
			name:     "an explicit true survives the round trip",
			request:  StartRequest{Role: "prefill", ModelID: "m", PrepareEnvironment: ptr(true)},
			contains: `"prepare_environment":true`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.request)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			body := string(encoded)
			if !strings.Contains(body, tc.contains) {
				t.Fatalf("payload %s must contain %s", body, tc.contains)
			}
			if tc.absent != "" && strings.Contains(body, tc.absent) {
				t.Fatalf("payload %s must not contain %s", body, tc.absent)
			}
		})
	}
}

func ptr(b bool) *bool { return &b }
