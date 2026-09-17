package partner

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// StaticAdapter is the milestone 1 PartnerAdapter. Partners that do not expose
// a live capacity API yet are represented by configuration; the same adapter
// backs the local development harness, where capacity changes are injected with
// Add / Revoke / Replace.
//
// It is safe for concurrent use.
type StaticAdapter struct {
	id string

	mu        sync.RWMutex
	available map[string]CapacityInstance
	leases    map[string]CapacityInstance
	released  map[string]time.Time

	// Failure injection for tests and for rehearsing partner outage handling.
	listError    error
	releaseError error

	releaseCalls []string
}

// StaticAdapterOption customizes a StaticAdapter.
type StaticAdapterOption func(*StaticAdapter)

// WithListError makes every pull fail with the given error.
func WithListError(err error) StaticAdapterOption {
	return func(a *StaticAdapter) { a.listError = err }
}

// WithReleaseError makes every release fail with the given error.
func WithReleaseError(err error) StaticAdapterOption {
	return func(a *StaticAdapter) { a.releaseError = err }
}

// NewStaticAdapter builds an adapter from a fixed capacity list.
func NewStaticAdapter(id string, instances []CapacityInstance, options ...StaticAdapterOption) (*StaticAdapter, error) {
	if id == "" {
		return nil, errors.New("static adapter requires a partner id")
	}
	adapter := &StaticAdapter{
		id:        id,
		available: map[string]CapacityInstance{},
		leases:    map[string]CapacityInstance{},
		released:  map[string]time.Time{},
	}
	for _, option := range options {
		option(adapter)
	}
	for _, instance := range instances {
		if err := validateInstance(instance); err != nil {
			return nil, err
		}
		if _, duplicate := adapter.leases[instance.ID]; duplicate {
			return nil, errors.New("static adapter received duplicate instance " + instance.ID)
		}
		adapter.available[instance.ID] = instance
		adapter.leases[instance.ID] = instance
	}
	return adapter, nil
}

// PartnerID implements PartnerAdapter.
func (a *StaticAdapter) PartnerID() string { return a.id }

// ListAvailableInstances implements PartnerAdapter.
func (a *StaticAdapter) ListAvailableInstances(_ context.Context) ([]CapacityInstance, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.listError != nil {
		return nil, a.listError
	}
	return sortedInstances(a.available), nil
}

// ListActiveLeases implements PartnerAdapter.
func (a *StaticAdapter) ListActiveLeases(_ context.Context) ([]CapacityInstance, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.listError != nil {
		return nil, a.listError
	}
	return sortedInstances(a.leases), nil
}

// ReleaseInstance implements PartnerAdapter. The container leaves both views,
// mirroring a partner that reclaimed it.
func (a *StaticAdapter) ReleaseInstance(_ context.Context, instanceID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.releaseError != nil {
		return a.releaseError
	}
	a.releaseCalls = append(a.releaseCalls, instanceID)
	if _, ok := a.leases[instanceID]; !ok {
		// Idempotent success: an already gone instance is not an error.
		return nil
	}
	delete(a.available, instanceID)
	delete(a.leases, instanceID)
	a.released[instanceID] = time.Now().UTC()
	return nil
}

// Add publishes a new container. Real partners push capacity; this is the
// harness equivalent.
func (a *StaticAdapter) Add(instance CapacityInstance) error {
	if err := validateInstance(instance); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.leases[instance.ID]; exists {
		a.available[instance.ID] = instance
		a.leases[instance.ID] = instance
		return nil
	}
	a.available[instance.ID] = instance
	a.leases[instance.ID] = instance
	return nil
}

// Revoke removes a container from both views.
func (a *StaticAdapter) Revoke(instanceID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.available, instanceID)
	delete(a.leases, instanceID)
}

// Replace swaps the whole capacity view, simulating a partner-wide change.
func (a *StaticAdapter) Replace(instances []CapacityInstance) error {
	next := make(map[string]CapacityInstance, len(instances))
	for _, instance := range instances {
		if err := validateInstance(instance); err != nil {
			return err
		}
		next[instance.ID] = instance
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.available = next
	a.leases = make(map[string]CapacityInstance, len(next))
	for id, instance := range next {
		a.leases[id] = instance
	}
	return nil
}

// ReleaseCalls returns the released instance ids in call order.
func (a *StaticAdapter) ReleaseCalls() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]string(nil), a.releaseCalls...)
}

// Released reports whether the instance was handed back to the partner.
func (a *StaticAdapter) Released(instanceID string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.released[instanceID]
	return ok
}

func sortedInstances(source map[string]CapacityInstance) []CapacityInstance {
	instances := make([]CapacityInstance, 0, len(source))
	for _, instance := range source {
		instances = append(instances, instance)
	}
	sort.Slice(instances, func(i, j int) bool { return instances[i].ID < instances[j].ID })
	return instances
}
