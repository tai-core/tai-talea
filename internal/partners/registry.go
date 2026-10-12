// Package partners builds the PartnerAdapter set from configuration.
package partners

import (
	"fmt"

	"github.com/tai-core/tai-talea/internal/config"
	"github.com/tai-core/tai-talea/internal/partner"
	"github.com/tai-core/tai-talea/internal/partners/alayanew"
	"github.com/tai-core/tai-talea/internal/partners/kubernetes"
)

// Registry resolves a partner id to its adapter.
type Registry struct {
	order    []string
	adapters map[string]partner.PartnerAdapter
}

// Build constructs one adapter per configured partner. Each partner is fully
// isolated: its credentials, its adapter kind and its capacity view never leak
// into another partner's reconciliation.
//
// resolveAdvertiseHost answers "which address did the control plane verify for
// this container?" for platforms that publish an address peers cannot use for
// every port. It may be nil, in which case only the static configuration is
// consulted.
func Build(cfg config.Config, resolveAdvertiseHost func(name string) string) (*Registry, error) {
	registry := &Registry{adapters: make(map[string]partner.PartnerAdapter, len(cfg.Partners))}
	for _, partnerConfig := range cfg.Partners {
		adapter, err := buildAdapter(cfg, partnerConfig, resolveAdvertiseHost)
		if err != nil {
			return nil, fmt.Errorf("partner %s: %w", partnerConfig.ID, err)
		}
		registry.adapters[partnerConfig.ID] = adapter
		registry.order = append(registry.order, partnerConfig.ID)
	}
	return registry, nil
}

func buildAdapter(global config.Config, partnerConfig config.PartnerConfig, resolveAdvertiseHost func(name string) string) (partner.PartnerAdapter, error) {
	switch partnerConfig.Adapter {
	case "static":
		instances := make([]partner.CapacityInstance, 0, len(partnerConfig.Instances))
		for _, item := range partnerConfig.Instances {
			// One mapping lives in the config package; reusing it here is what
			// keeps validation and wiring from drifting apart.
			event := item.ToEventInstance()
			instances = append(instances, partner.CapacityInstance{
				ID:              event.ID,
				Endpoint:        event.Endpoint,
				ServiceEndpoint: event.ServiceEndpoint,
				LeaseID:         event.LeaseID,
				Spec:            *event.Spec,
			})
		}
		return partner.NewStaticAdapter(partnerConfig.ID, instances)
	case "http":
		return partner.NewHTTPAdapter(partner.HTTPAdapterConfig{
			PartnerID:           partnerConfig.ID,
			BaseURL:             partnerConfig.HTTP.BaseURL,
			Token:               partnerConfig.HTTP.Token,
			AvailablePath:       partnerConfig.HTTP.AvailablePath,
			LeasesPath:          partnerConfig.HTTP.LeasesPath,
			ReleasePathTemplate: partnerConfig.HTTP.ReleasePathTemplate,
			Timeout:             partnerConfig.HTTP.Timeout.Duration(),
		})
	case "alayanew":
		alaya := partnerConfig.AlayaNeW
		if alaya == nil {
			return nil, fmt.Errorf("alayanew adapter requires alayanew configuration")
		}
		return alayanew.New(partnerConfig.ID, alayanew.Config{
			AccessKey:         alaya.AccessKey,
			SecretKey:         alaya.SecretKey,
			AIDCId:            alaya.AIDCId,
			ModelSupport:      alaya.ModelSupport,
			BootstrapPort:     alaya.BootstrapPort,
			ServicePort:       alaya.ServicePort,
			ProductCode:       alaya.ProductCode,
			Image:             alaya.Image,
			ComputeResource:   alaya.ComputeResource,
			Priority:          alaya.Priority,
			NASStorageID:      alaya.NASStorageID,
			NASFileDirectory:  alaya.NASFileDirectory,
			NASMountPath:      alaya.NASMountPath,
			NASReadOnly:       alaya.NASReadOnly,
			Env:               alaya.Env,
			AdvertiseHosts:    alaya.AdvertiseHosts,
			AdvertiseResolver: resolveAdvertiseHost,
			Timeout:           alaya.Timeout.Duration(),
			WaitTimeout:       alaya.WaitTimeout.Duration(),
		})
	case "kubernetes":
		cluster := partnerConfig.Kubernetes
		if cluster == nil {
			return nil, fmt.Errorf("kubernetes adapter requires kubernetes configuration")
		}
		return kubernetes.New(partnerConfig.ID, kubernetes.Config{
			Kubeconfig:      cluster.Kubeconfig,
			Context:         cluster.Context,
			Namespace:       cluster.Namespace,
			Image:           cluster.Image,
			ImagePullPolicy: cluster.ImagePullPolicy,
			Entrypoint:      cluster.Entrypoint,
			// The container bootstrap refuses to serve without the shared
			// secret, so it is injected here from the same control plane
			// setting the SSH route provisions on its workers.
			BootstrapToken: global.Controller.BootstrapToken,
			BootstrapPort:  cluster.BootstrapPort,
			ServicePort:    cluster.ServicePort,
			ProfilePath:    cluster.ProfilePath,
			GPUResource:    cluster.GPUResource,
			GPUCount:       cluster.GPUCount,
			NodeSelector:   cluster.NodeSelector,
			Nodes:          kubernetesNodes(cluster.Nodes),
			Volumes:        kubernetesVolumes(cluster.Volumes),
			ShmSize:        cluster.ShmSize,
			HostNetwork:    cluster.HostNetwork,
			HostIPC:        cluster.HostIPC,
			Privileged:     cluster.Privileged,
			ServiceAccount: cluster.ServiceAccount,
			RunAsUser:      cluster.RunAsUser,
			RunAsGroup:     cluster.RunAsGroup,
			FSGroup:        cluster.FSGroup,
			Env:            cluster.Env,
			ModelSupport:   cluster.ModelSupport,
			Timeout:        cluster.Timeout.Duration(),
			WaitTimeout:    cluster.WaitTimeout.Duration(),
		})
	default:
		return nil, fmt.Errorf("unsupported adapter %q", partnerConfig.Adapter)
	}
}

func kubernetesVolumes(volumes []config.KubernetesVolumeConfig) []kubernetes.VolumeMountConfig {
	out := make([]kubernetes.VolumeMountConfig, 0, len(volumes))
	for _, volume := range volumes {
		out = append(out, kubernetes.VolumeMountConfig{
			Name:      volume.Name,
			HostPath:  volume.HostPath,
			MountPath: volume.MountPath,
			ReadOnly:  volume.ReadOnly,
		})
	}
	return out
}

func kubernetesNodes(nodes []config.KubernetesNodeConfig) []kubernetes.NodeProfile {
	out := make([]kubernetes.NodeProfile, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, kubernetes.NodeProfile{
			Host:            node.Host,
			Env:             node.Env,
			Selector:        node.NodeSelector,
			ExtraSGLangArgs: node.ExtraSGLangArgs,
		})
	}
	return out
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
