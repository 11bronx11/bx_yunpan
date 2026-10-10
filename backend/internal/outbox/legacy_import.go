package outbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// StageLegacyTask transfers a stopped legacy queue's outstanding task into the
// durable relay. The caller owns the transaction and must keep old workers off.
// Published history is otherwise never automatically replayed by the new relay.
func StageLegacyTask(tx *gorm.DB, task taskqueue.Task, due time.Time, gcDelay time.Duration, apply bool) error {
	eventID, err := uuid.Parse(strings.SplitN(task.ID, ":", 2)[0])
	if err != nil {
		return fmt.Errorf("legacy task is not an Outbox task: %s", task.ID)
	}
	var event Event
	if err := tx.Where("id = ?", eventID).First(&event).Error; err != nil {
		return fmt.Errorf("legacy task %s has no matching event: %w", task.ID, err)
	}
	kind, q := canonicalTaskType(task.Kind, task.Queue)
	dest := kind + "@" + q
	if _, ok := targetAllowedForEventWithSearch(event.EventType, true, true, Delivery{Transport: "rabbitmq", Destination: dest}); !ok {
		return fmt.Errorf("legacy task %s route does not match event", task.ID)
	}
	var rows []Delivery
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("event_id = ? AND transport IN ?", eventID, []string{"asynq", "rabbitmq"}).Find(&rows).Error; err != nil {
		return err
	}
	var owned *Delivery
	for _, r := range rows {
		parsed, ok := targetFromDelivery(Delivery{Transport: "rabbitmq", Destination: r.Destination})
		if ok && parsed.Destination == dest {
			if owned != nil {
				return fmt.Errorf("legacy task %s has ambiguous delivery ownership", task.ID)
			}
			copy := r
			owned = &copy
		}
	}
	if owned == nil {
		return fmt.Errorf("legacy task %s has no delivery owner", task.ID)
	}
	if !apply {
		return nil
	}
	digest := sha256.Sum256(task.Body)
	result := tx.Exec(`INSERT INTO legacy_task_imports(task_id,event_id,destination,payload_sha256,retry_count,max_retry,available_at)
  VALUES(?,?,?,?,?,?,?) ON CONFLICT(task_id) DO NOTHING`, task.ID, eventID, dest, hex.EncodeToString(digest[:]), task.RetryCount, task.MaxRetry, due)
	if result.Error != nil || result.RowsAffected == 0 {
		return result.Error
	}
	notBefore := due
	if kind == TaskObjectGCV1 && event.CreatedAt.Add(gcDelay).After(notBefore) {
		notBefore = event.CreatedAt.Add(gcDelay)
	}
	updates := map[string]any{"transport": "rabbitmq", "status": "pending", "published_at": nil, "lease_until": nil, "lease_token": nil, "last_error": nil, "available_at": due, "not_before": notBefore, "task_retry_count": task.RetryCount, "task_max_retry": task.MaxRetry}
	if task.Timeout > 0 {
		updates["task_timeout_ms"] = task.Timeout.Milliseconds()
	}
	if err := tx.Model(&Delivery{}).Where("event_id = ? AND transport = ? AND destination = ?", owned.EventID, owned.Transport, owned.Destination).Updates(updates).Error; err != nil {
		return err
	}
	return tx.Model(&Event{}).Where("id = ?", eventID).Updates(map[string]any{"status": "pending", "published_at": nil}).Error
}
