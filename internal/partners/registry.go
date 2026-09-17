// Package partners builds the PartnerAdapter set from configuration.
package partners

import (
	"fmt"

	"github.com/tai-core/tai-talea/internal/config"
	"github.com/tai-core/tai-talea/internal/partner"
)

// Registry resolves a partner id to its adapter.
type Registry struct {
	order    []string
	adapters map[string]partner.PartnerAdapter
}

// Build constructs one adapter per configured partner. Each partner is fully
// isolated: its credentials, its adapter kind and its capacity view never leak
// into another partner's reconciliation.
func Build(cfg config.Config) (*Registry, error) {
	registry := &Registry{adapters: make(map[string]partner.PartnerAdapter, len(cfg.Partners))}
	for _, partnerConfig := range cfg.Partners {
		adapter, err := buildAdapter(partnerConfig)
		if err != nil {
			return nil, fmt.Errorf("partner %s: %w", partnerConfig.ID, err)
		}
		registry.adapters[partnerConfig.ID] = adapter
		registry.order = append(registry.order, partnerConfig.ID)
	}
	return registry, nil
}

func buildAdapter(cfg config.PartnerConfig) (partner.PartnerAdapter, error) {
	switch cfg.Adapter {
	case "static":
		instances := make([]partner.CapacityInstance, 0, len(cfg.Instances))
		for _, item := range cfg.Instances {
			instances = append(instances, partner.CapacityInstance{
				ID:              item.ID,
				Endpoint:        item.Endpoint,
				ServiceEndpoint: item.ServiceEndpoint,
				LeaseID:         item.LeaseID,
				Spec:            item.Spec,
			})
		}
		return partner.NewStaticAdapter(cfg.ID, instances)
	case "http":
		return partner.NewHTTPAdapter(partner.HTTPAdapterConfig{
			PartnerID:           cfg.ID,
			BaseURL:             cfg.HTTP.BaseURL,
			Token:               cfg.HTTP.Token,
			AvailablePath:       cfg.HTTP.AvailablePath,
			LeasesPath:          cfg.HTTP.LeasesPath,
			ReleasePathTemplate: cfg.HTTP.ReleasePathTemplate,
			Timeout:             cfg.HTTP.Timeout.Duration(),
		})
	default:
		return nil, fmt.Errorf("unsupported adapter %q", cfg.Adapter)
	}
}

// Adapter resolves one partner adapter.
func (r *Registry) Adapter(partnerID string) (partner.PartnerAdapter, bool) {
	adapter, ok := r.adapters[partnerID]
	return adapter, ok
}

// IDs lists the configured partner ids in configuration order.
func (r *Registry) IDs() []string {
	return append([]string(nil), r.order...)
}

// All returns every adapter in configuration order.
func (r *Registry) All() []partner.PartnerAdapter {
	adapters := make([]partner.PartnerAdapter, 0, len(r.order))
	for _, id := range r.order {
		adapters = append(adapters, r.adapters[id])
	}
	return adapters
}

// Static returns the static adapter of one partner, which is what the local
// development harness manipulates to simulate partner capacity changes.
func (r *Registry) Static(partnerID string) (*partner.StaticAdapter, bool) {
	adapter, ok := r.adapters[partnerID]
	if !ok {
		return nil, false
	}
	static, ok := adapter.(*partner.StaticAdapter)
	return static, ok
}
