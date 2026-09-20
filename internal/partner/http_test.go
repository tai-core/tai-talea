package partner_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/partner"
)

func TestHTTPAdapterRequiresCompleteExplicitArray(t *testing.T) {
	for _, payload := range []string{`{}`, `null`, `{"instances":null}`, `{"instances":[]}{}`, `{"instances":[],"complete":false}`, `{"instances":[],"next_cursor":"page2"}`} {
		t.Run(payload, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(payload)) }))
			defer server.Close()
			adapter, err := partner.NewHTTPAdapter(partner.HTTPAdapterConfig{PartnerID: "test", BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.ListActiveLeases(context.Background()); !errors.Is(err, partner.ErrSnapshotIncomplete) {
				t.Fatalf("incomplete response accepted: %v", err)
			}
		})
	}
	for _, payload := range []string{`{"instances":[]}`, `{"instances":[],"complete":true}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(payload)) }))
		adapter, err := partner.NewHTTPAdapter(partner.HTTPAdapterConfig{PartnerID: "test", BaseURL: server.URL})
		if err != nil {
			t.Fatal(err)
		}
		instances, err := adapter.ListActiveLeases(context.Background())
		server.Close()
		if err != nil || len(instances) != 0 {
			t.Fatalf("explicit empty snapshot rejected: %v", err)
		}
	}
}

func TestHTTPReleaseConflictIsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusConflict) }))
	defer server.Close()
	adapter, err := partner.NewHTTPAdapter(partner.HTTPAdapterConfig{PartnerID: "test", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.ReleaseInstance(context.Background(), "busy-node"); err == nil {
		t.Fatal("HTTP 409 must not mark a busy container released")
	}
}

type splitViews struct{ available, leases []partner.CapacityInstance }

func (s splitViews) PartnerID() string { return "partner-a" }
func (s splitViews) ListAvailableInstances(context.Context) ([]partner.CapacityInstance, error) {
	return s.available, nil
}
func (s splitViews) ListActiveLeases(context.Context) ([]partner.CapacityInstance, error) {
	return s.leases, nil
}
func (s splitViews) ReleaseInstance(context.Context, string) error { return nil }

func TestSnapshotRetainsOccupiedLeasesAndRejectsConflict(t *testing.T) {
	a, b := instance("a", "lease-a"), instance("b", "lease-b")
	snapshot, err := partner.CollectSnapshot(context.Background(), splitViews{available: []partner.CapacityInstance{a}, leases: []partner.CapacityInstance{a, b}})
	if err != nil || len(snapshot.Instances) != 2 {
		t.Fatalf("occupied lease omitted: %+v %v", snapshot, err)
	}
	conflict := a
	conflict.LeaseID = "different-lease"
	for _, available := range [][]partner.CapacityInstance{{a, a}, {conflict}, {b}} {
		_, err := partner.CollectSnapshot(context.Background(), splitViews{available: available, leases: []partner.CapacityInstance{a}})
		if !errors.Is(err, partner.ErrSnapshotIncomplete) {
			t.Fatalf("conflicting view accepted: %v", err)
		}
	}
}

func TestPullServiceEndpointOnlyChange(t *testing.T) {
	puller, _ := newPuller(t)
	a := instance("a", "lease-a")
	a.ServiceEndpoint = "http://a:9002"
	adapter, err := partner.NewStaticAdapter("partner-a", []partner.CapacityInstance{a})
	if err != nil {
		t.Fatal(err)
	}
	first, err := puller.Plan(context.Background(), adapter)
	if err != nil {
		t.Fatal(err)
	}
	if err := puller.Commit(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	a.ServiceEndpoint = "http://a:9003"
	if err := adapter.Replace([]partner.CapacityInstance{a}); err != nil {
		t.Fatal(err)
	}
	second, err := puller.Plan(context.Background(), adapter)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Updated) != 1 || first.Snapshot.Version == second.Snapshot.Version {
		t.Fatal("service endpoint change lost from snapshot diff/version")
	}
}

type timedViews struct {
	splitViews
	fetchedAt time.Time
}

func (s *timedViews) ListAvailableInstances(context.Context) ([]partner.CapacityInstance, error) {
	s.fetchedAt = time.Now().UTC()
	return s.available, nil
}
func TestSnapshotObservationPrecedesNetworkFetch(t *testing.T) {
	a := instance("a", "lease-a")
	views := &timedViews{splitViews: splitViews{available: []partner.CapacityInstance{a}, leases: []partner.CapacityInstance{a}}}
	snapshot, err := partner.CollectSnapshot(context.Background(), views)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Observed.After(views.fetchedAt) {
		t.Fatal("snapshot can overwrite a newer push received during fetch")
	}
}
