package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/store"
)

func (c *Controller) reconcileReclaim(ctx context.Context, instance domain.Instance, report *ReconcileReport) error {
	lock := c.instanceOperation(instance.ID)
	if !lock.TryLock() {
		// A start owns the node. Its health loop observes the durable reclaim
		// flag and exits; never stop/release underneath an in-flight Start call.
		return nil
	}
	defer lock.Unlock()
	var err error
	instance, err = c.store.GetInstance(ctx, instance.ID)
	if err != nil {
		return err
	}
	if instance.InstanceState == domain.InstanceReleased {
		return nil
	}
	if instance.ServiceState == domain.ServiceNone {
		if due, err := c.operationRetryDue(ctx, instance.ID, store.OpRelease); err != nil {
			return err
		} else if !due {
			return nil
		}
		if err := c.BeginRelease(ctx, instance.ID, "requested reclaim"); err != nil {
			return err
		}
		report.Released = append(report.Released, instance.ID)
		return nil
	}
	if due, err := c.operationRetryDue(ctx, instance.ID, store.OpDrain); err != nil {
		return err
	} else if !due {
		return nil
	}
	if instance.ServiceState != domain.ServiceDraining {
		// Old versions could leave a failed stop in FAILED. It must return to
		// draining, never to the normal service restart path.
		if err := c.beginDrain(ctx, instance.ID, "requested reclaim", 0, true); err != nil {
			c.recordReclaimError(ctx, instance.ID, err)
			return err
		}
		var err error
		instance, err = c.store.GetInstance(ctx, instance.ID)
		if err != nil {
			return err
		}
	}
	if err := c.finishDrain(ctx, instance); err != nil {
		return err
	}
	return nil
}

func (c *Controller) recordReclaimError(ctx context.Context, id string, cause error) {
	// Preserve the intent and expose the error without rewriting a stale row.
	current, err := c.store.GetInstance(ctx, id)
	if err != nil || !current.PendingRelease {
		return
	}
	current.LastError = cause.Error()
	// FAILED plus pending-release prevents normal restart and allows the next
	// round to retry closing readiness before attempting stop.
	if current.ServiceState != domain.ServiceNone && current.ServiceState != domain.ServiceDraining {
		current.ServiceState = domain.ServiceFailed
	}
	_ = c.transition(ctx, store.Transition{InstanceID: id, Next: current,
		Operation: &store.Operation{OperationID: drainOperationID(current), InstanceID: id,
			Type: store.OpDrain, Status: store.OpPending, LastError: cause.Error(), CreatedAt: c.now(),
			NextRetryAt: c.now().Add(c.cfg.Controller.OperationRetry.Duration())}})
}

// DefaultDrainGrace supplies the same policy to all operator entry points.
func (c *Controller) DefaultDrainGrace() time.Duration { return c.cfg.Controller.DrainGrace.Duration() }

func drainOperationID(instance domain.Instance) string {
	return fmt.Sprintf("drain:%d:%s:%s", len(instance.ID), instance.ID, instance.LeaseID)
}
