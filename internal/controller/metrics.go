package controller

import (
	"context"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/obs"
)

// allInstanceStates is the closed set published for capacity_instances_total.
// Publishing the full cross-product keeps the metric free of stale series.
var allInstanceStates = []domain.InstanceState{
	domain.InstanceAllocating,
	domain.InstancePreparing,
	domain.InstanceIdle,
	domain.InstanceReleasing,
	domain.InstanceReleased,
	domain.InstanceLost,
}

// allServiceStates is the closed set published for service_instances_total.
var allServiceStates = []domain.ServiceState{
	domain.ServiceNone,
	domain.ServiceStarting,
	domain.ServiceHealthy,
	domain.ServiceRegistering,
	domain.ServiceServing,
	domain.ServiceDraining,
	domain.ServiceFailed,
}

// allRoles is the closed set of PD roles, including "unassigned".
var allRoles = []domain.Role{domain.RoleNone, domain.RolePrefill, domain.RoleDecode}

// PublishGauges recomputes the capacity and service gauges from the database.
func (c *Controller) PublishGauges(ctx context.Context) error {
	if c.metrics == nil {
		return nil
	}
	instanceCounts, err := c.store.CountInstancesByState(ctx)
	if err != nil {
		return err
	}
	serviceCounts, err := c.store.CountServicesByRoleState(ctx)
	if err != nil {
		return err
	}

	instanceIndex := make(map[string]int, len(instanceCounts))
	for _, count := range instanceCounts {
		instanceIndex[count.Partner+"\x00"+count.State] = count.Count
	}
	serviceIndex := make(map[string]int, len(serviceCounts))
	for _, count := range serviceCounts {
		serviceIndex[count.Role+"\x00"+count.State] = count.Count
	}

	for _, partnerID := range c.cfg.PartnerIDs() {
		for _, state := range allInstanceStates {
			c.metrics.AddInstanceStateCount(partnerID, string(state),
				instanceIndex[partnerID+"\x00"+string(state)])
		}
	}
	for _, role := range allRoles {
		for _, state := range allServiceStates {
			c.metrics.AddServiceStateCount(roleLabel(role), string(state),
				serviceIndex[string(role)+"\x00"+string(state)])
		}
	}

	operations, err := c.store.ListOpenOperations(ctx, 1000)
	if err == nil {
		c.metrics.SetGauge("capacity_open_operations",
			"Lifecycle operations that are not in a terminal state.", nil, float64(len(operations)))
	}
	return nil
}

func roleLabel(role domain.Role) string {
	if role == domain.RoleNone {
		return "none"
	}
	return string(role)
}

// MetricsHandlerPayload is a convenience hook for the HTTP layer.
func (c *Controller) MetricsRegistry() *obs.Registry { return c.metrics }
