package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/store"
)

// EnrollPrepared takes over an SSH-provisioned node. Job ID is the new lease ID,
// so a restart after committing the row can safely repeat this call.
func (c *Controller) EnrollPrepared(ctx context.Context, instance domain.Instance) error {
	c.capacityAdmissions.Lock()
	defer c.capacityAdmissions.Unlock()
	lock := c.instanceOperation(instance.ID)
	lock.Lock()
	defer lock.Unlock()
	if _, ok := c.partners.Adapter(instance.PartnerID); !ok {
		return errors.New("unknown partner")
	}
	rows, err := c.store.ListInstances(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.ID != instance.ID && row.Active() && reservesEndpoint(row, instance.Endpoint, instance.ServiceURL()) {
			return fmt.Errorf("endpoint already belongs to another active node")
		}
	}
	current, err := c.store.GetInstance(ctx, instance.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err == nil && current.PartnerID != instance.PartnerID {
		return store.ErrConflict
	}
	if err == nil && current.LeaseID == instance.LeaseID && current.InstanceState != domain.InstanceReleased {
		if current.InstanceState != domain.InstanceAllocating {
			// The poll may have already created the row from the address the
			// platform publishes, which on CCI is a public ingress that carries
			// only the container's HTTP ports. The installation measured the
			// container's own address from the inside and verified the control
			// node can reach it, and that is the address PD disaggregation
			// needs, so the measurement replaces the published one. A node that
			// already serves keeps its identity: changing it is what the
			// pending-update path is for.
			if current.HasService() || domain.NormalizeEndpoint(current.Endpoint) == domain.NormalizeEndpoint(instance.Endpoint) {
				return nil
			}
			return c.adoptVerifiedEndpoint(ctx, current, instance)
		}
		instance = current
	} else {
		if err == nil && current.InstanceState != domain.InstanceReleased {
			return store.ErrConflict
		}
		instance.InstanceState, instance.ServiceState = domain.InstanceAllocating, domain.ServiceNone
		instance.LeaseUpdatedAt, instance.LastSeenAt = c.now(), c.now()
		if err := c.transition(ctx, store.Transition{InstanceID: instance.ID, Next: instance,
			CreateOnly: errors.Is(err, store.ErrNotFound), Reoffer: err == nil,
			Audit: store.AuditEntry{Action: "ssh_node_enrolled", InstanceID: instance.ID, PartnerID: instance.PartnerID,
				Details: map[string]string{"lease_id": instance.LeaseID, "endpoint": instance.Endpoint}}}); err != nil {
			return err
		}
	}
	instance.InstanceState = domain.InstancePreparing
	return c.transition(ctx, store.Transition{InstanceID: instance.ID, Next: instance, ExpectInstanceState: domain.InstanceAllocating,
		Operation: &store.Operation{OperationID: lifecycleOperationID(instance, store.OpPrepare), InstanceID: instance.ID, Type: store.OpPrepare, Status: store.OpPending, CreatedAt: c.now()},
		Audit:     store.AuditEntry{Action: "ssh_preparation_queued", InstanceID: instance.ID, PartnerID: instance.PartnerID}})
}

// adoptVerifiedEndpoint re-points a row that was created before the container
// was installed at the address the pipeline verified from inside it, and puts
// the node back through preparation so the new identity is validated before
// anything is scheduled onto it.
func (c *Controller) adoptVerifiedEndpoint(ctx context.Context, current, verified domain.Instance) error {
	next := current
	next.Endpoint = domain.NormalizeEndpoint(verified.Endpoint)
	next.ServiceEndpoint = domain.NormalizeEndpoint(verified.ServiceEndpoint)
	next.InstanceState = domain.InstancePreparing
	next.ServiceState = domain.ServiceNone
	next.PrepareAttempts, next.StartAttempts, next.StartRearms = 0, 0, 0
	next.StartParkedAt = time.Time{}
	next.RouterWorkerID, next.ReadinessGeneration = "", 0
	next.LastError = ""
	next.LeaseUpdatedAt, next.LastSeenAt = c.now(), c.now()
	return c.transition(ctx, store.Transition{
		InstanceID: current.ID,
		Next:       next,
		Audit: store.AuditEntry{Action: "ssh_endpoint_adopted", InstanceID: current.ID, PartnerID: current.PartnerID,
			Details: map[string]string{"endpoint": next.Endpoint, "previous_endpoint": current.Endpoint}},
	})
}

// Pending addresses are reserved until the old service has drained. Another
// admission must not take them while an identity update is in progress.
func reservesEndpoint(row domain.Instance, endpoint, serviceURL string) bool {
	endpoint, serviceURL = domain.NormalizeEndpoint(endpoint), domain.NormalizeEndpoint(serviceURL)
	if domain.NormalizeEndpoint(row.Endpoint) == endpoint || domain.NormalizeEndpoint(row.ServiceURL()) == serviceURL {
		return true
	}
	if row.PendingUpdate == nil {
		return false
	}
	pendingService := row.PendingUpdate.ServiceEndpoint
	if pendingService == "" {
		pendingService = row.PendingUpdate.Endpoint
	}
	return domain.NormalizeEndpoint(row.PendingUpdate.Endpoint) == endpoint || domain.NormalizeEndpoint(pendingService) == serviceURL
}
