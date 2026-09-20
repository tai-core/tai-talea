package domain

import "testing"

func TestCapacityEndpointsAreOrigins(t *testing.T) {
	for _, endpoint := range []string{"http://", "http:///path", "http://user:secret@node:9001", "http://node/api", "http://node?token=x", "http://node#fragment", " http://node"} {
		if err := (EventInstance{ID: "node", Endpoint: endpoint}).Validate(); err == nil {
			t.Fatalf("invalid origin accepted: %q", endpoint)
		}
	}
	for _, endpoint := range []string{"http://node:9001", "https://node/", "http://[::1]:9001"} {
		if err := (EventInstance{ID: "node", Endpoint: endpoint}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
}
