package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tai-core/tai-talea/internal/domain"
)

// ResumePendingEvents recovers events claimed before a crash or a transient
// write/network failure. Live delivery owns the same lock, so an event that is
// still executing is skipped instead of being abandoned based on elapsed time.
func (c *Controller) ResumePendingEvents(ctx context.Context) (int, error) {
	if !c.eventRecovery.TryLock() {
		return 0, nil
	}
	defer c.eventRecovery.Unlock()
	parent := ctx
	budget := c.cfg.Controller.ReconcileInterval.Duration()
	if budget < time.Second {
		budget = time.Second
	}
	if budget > 5*time.Second {
		budget = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	records, err := c.store.ListPendingEventsAfter(ctx, 32, c.eventRecoveryTime, c.eventRecoveryAfter)
	if err != nil {
		return 0, err
	}
	if len(records) == 0 && c.eventRecoveryAfter != "" {
		c.eventRecoveryAfter = ""
		c.eventRecoveryTime = time.Time{}
		records, err = c.store.ListPendingEventsAfter(ctx, 32, time.Time{}, "")
		if err != nil {
			return 0, err
		}
	}
	resumed := 0
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return resumed, parent.Err()
		}
		c.eventRecoveryAfter = record.EventID
		c.eventRecoveryTime = record.OccurredAt
		lock := c.eventOperation(record.EventID)
		if !lock.TryLock() {
			continue
		}
		err := func() error {
			defer lock.Unlock()
			latest, err := c.store.GetEvent(ctx, record.EventID)
			if err != nil || latest.Result != domain.ResultPending {
				return err
			}
			var event domain.CapacityEvent
			if err := json.Unmarshal([]byte(latest.PayloadJSON), &event); err != nil {
				return c.store.CompleteEvent(ctx, latest.EventID, domain.ResultRejected, "stored event payload cannot be decoded")
			}
			event.EventID, event.PartnerID, event.Type = latest.EventID, latest.PartnerID, latest.EventType
			event.Source, event.ReceivedAt, event.OccurredAt = latest.Source, latest.ReceivedAt, latest.OccurredAt
			if err := event.Validate(); err != nil {
				return c.store.CompleteEvent(ctx, event.EventID, domain.ResultRejected, fmt.Sprintf("stored event invalid: %s", err))
			}
			resumed++
			_, err = c.applyCapacityEvent(ctx, event)
			return err
		}()
		if err != nil {
			c.log().Warn("pending event replay deferred", "event_id", record.EventID, "error", err.Error())
		}
	}
	return resumed, nil
}
