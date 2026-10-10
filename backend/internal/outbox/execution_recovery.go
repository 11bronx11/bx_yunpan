package outbox

import (
	"errors"
	"strings"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func deliveryTaskID(row Delivery) string {
	target, ok := targetFromDelivery(row)
	if !ok {
		return row.EventID.String() + ":" + row.Destination
	}
	return row.EventID.String() + ":" + target.Destination
}

// RecoverDelivery reuses the original publication instead of creating a new
// task identity. Callers supply a transaction, so replay and ownership checks
// commit together. Live, retrying, and terminal tasks are kept intact.
func RecoverDelivery(tx *gorm.DB, eventType string, aggregateID uuid.UUID, now time.Time, staleAfter time.Duration) (active, repaired bool, err error) {
	if tx == nil || eventType == "" || aggregateID == uuid.Nil || staleAfter <= 0 {
		return false, false, errors.New("outbox: invalid recovery lookup")
	}
	var rows []Delivery
	err = tx.Clauses(clause.Locking{Strength: "UPDATE", Table: clause.Table{Name: "d"}}).Table("outbox_deliveries AS d").Select("d.*").Joins("JOIN outbox_events e ON e.id=d.event_id").Where("e.aggregate_id=?", aggregateID).Order("d.event_id,d.transport,d.destination").Find(&rows).Error
	if err != nil {
		return
	}
	wanted := Routes(eventType, true)
	for _, row := range rows {
		parsed, ok := targetFromDelivery(Delivery{Transport: "rabbitmq", Destination: row.Destination})
		if !ok {
			continue
		}
		matched := false
		for _, route := range wanted {
			if parsed.Destination == route.Destination || (parsed.Queue == "media" && route.Queue == "media") {
				matched = true
			}
		}
		if !matched {
			continue
		}
		if row.Status != "published" || row.Transport != "rabbitmq" {
			active = true
			continue
		}
		live, replay, e := recoverPublished(tx, row, now, staleAfter)
		if e != nil {
			return false, false, e
		}
		active = active || live
		repaired = repaired || replay
	}
	return
}

func recoverPublished(tx *gorm.DB, row Delivery, now time.Time, staleAfter time.Duration) (active, repaired bool, err error) {
	id := deliveryTaskID(row)
	if err = tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 741823))", id).Error; err != nil {
		return
	}
	var result taskqueue.Result
	q := tx.Where("task_id=?", id).Find(&result)
	if q.Error != nil {
		return false, false, q.Error
	}
	if q.RowsAffected > 0 {
		return result.Status == "dead", false, nil
	}
	var execution taskqueue.Execution
	q = tx.Where("task_id=?", id).Find(&execution)
	if q.Error != nil {
		return false, false, q.Error
	}
	due := row.PublishedAt != nil && !row.PublishedAt.Add(staleAfter).After(now)
	updates := map[string]any{"status": "pending", "available_at": now, "lease_until": nil, "lease_token": nil, "last_error": "execution receipt missing; replay original task"}
	if q.RowsAffected > 0 {
		switch execution.Status {
		case "running":
			due = due && execution.LeaseUntil != nil && !execution.LeaseUntil.After(now)
		case "retry":
			due = due && !execution.AvailableAt.Add(staleAfter).After(now)
		default:
			return false, false, errors.New("outbox: finished execution without terminal receipt")
		}
		updates["task_retry_count"] = execution.RetryCount
		updates["task_max_retry"] = execution.MaxRetry
		updates["task_timeout_ms"] = execution.TimeoutMS
	}
	if !due {
		return true, false, nil
	}
	q = tx.Model(&Delivery{}).Where("event_id=? AND transport=? AND destination=? AND status='published'", row.EventID, row.Transport, row.Destination).Updates(updates)
	if q.Error != nil {
		return false, false, q.Error
	}
	if q.RowsAffected != 1 {
		return true, false, nil
	}
	if err = tx.Model(&Event{}).Where("id=?", row.EventID).Updates(map[string]any{"status": "pending", "available_at": now}).Error; err != nil {
		return
	}
	return true, true, nil
}

// RecoverPublishedBatch covers every enabled queue, including projections
// whose business state has no dedicated recovery scanner. The query excludes
// live/retrying/terminal tasks before LIMIT so old successes cannot starve work.
func RecoverPublishedBatch(tx *gorm.DB, now time.Time, staleAfter time.Duration, limit int, aiEnabled, searchEnabled bool) (int, error) {
	if limit <= 0 || staleAfter <= 0 {
		return 0, errors.New("outbox: invalid recovery batch")
	}
	var rows []Delivery
	// Filter supported event/destination pairs before LIMIT, using the same
	// registry and compatibility mapping as publishing. Receipt joins must use
	// the canonical TaskID even when the stored destination is a legacy alias.
	var values []string
	var args []any
	for _, eventType := range eventcontract.EventTypes() {
		allowed := append(Routes(eventType, aiEnabled), SearchRoutes(eventType, searchEnabled)...)
		for _, route := range allowed {
			values = append(values, "(?::text,?::text,?::text)")
			args = append(args, eventType, route.Destination, route.Destination)
			for _, legacyType := range eventcontract.EventTypes() {
				kind, queue := canonicalTaskType(legacyType, route.Queue)
				if legacyType != kind && kind == route.TaskType && queue == route.Queue {
					values = append(values, "(?::text,?::text,?::text)")
					args = append(args, eventType, legacyType+"@"+queue, route.Destination)
				}
			}
		}
	}
	if len(values) == 0 {
		return 0, nil
	}
	query := tx.Table("outbox_deliveries AS d").Select("d.*").
		Joins("JOIN outbox_events e ON e.id=d.event_id").
		Joins("JOIN (VALUES "+strings.Join(values, ",")+") AS route(event_type,destination,canonical_destination) ON route.event_type=e.event_type AND route.destination=d.destination", args...).
		Joins("LEFT JOIN task_results r ON r.task_id=d.event_id::text || ':' || route.canonical_destination").
		Joins("LEFT JOIN task_executions x ON x.task_id=d.event_id::text || ':' || route.canonical_destination").
		Where("d.transport='rabbitmq' AND d.status='published' AND d.published_at<=? AND r.task_id IS NULL", now.Add(-staleAfter)).
		Where("x.task_id IS NULL OR (x.status='running' AND x.lease_until<=?) OR (x.status='retry' AND x.available_at<=?)", now, now.Add(-staleAfter))
	if err := query.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED", Table: clause.Table{Name: "d"}}).Order("d.published_at,d.event_id,d.destination").Limit(limit).Find(&rows).Error; err != nil {
		return 0, err
	}
	count := 0
	for _, row := range rows {
		var event Event
		if err := tx.Where("id=?", row.EventID).Take(&event).Error; err != nil {
			return count, err
		}
		if _, ok := targetAllowedForEventWithSearch(event.EventType, aiEnabled, searchEnabled, row); !ok {
			continue
		}
		_, replayed, err := recoverPublished(tx, row, now, staleAfter)
		if err != nil {
			return count, err
		}
		if replayed {
			count++
		}
	}
	return count, nil
}
