package outbox

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventstream"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// KafkaDispatcher owns only kafka deliveries. It deliberately does not touch
// Asynq rows, so a Kafka outage cannot block short-lived task execution.
type KafkaDispatcher struct {
	db       *gorm.DB
	producer eventstream.Producer
	logger   *slog.Logger
}

func NewKafkaDispatcher(db *gorm.DB, producer eventstream.Producer, logger *slog.Logger) *KafkaDispatcher {
	return &KafkaDispatcher{db: db, producer: producer, logger: logger}
}

// DispatchBatch publishes at most limit records and marks a delivery only
// after the producer acknowledges it. A failed publish remains retryable with
// backoff; the business event itself is never marked failed by this path.
func (d *KafkaDispatcher) DispatchBatch(ctx context.Context, limit int) error {
	if d == nil || d.db == nil || d.producer == nil {
		return errors.New("outbox: Kafka dispatcher is not configured")
	}
	if limit <= 0 {
		limit = 100
	}
	return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Events may have been written by an HTTP request context that does not
		// carry process-level feature flags. The Kafka relay therefore performs
		// the same compatibility backfill as the Asynq relay before polling.
		if err := ensureKafkaDeliveries(tx, true); err != nil {
			return err
		}
		var deliveries []Delivery
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("transport = 'kafka' AND status IN ? AND available_at <= ?", []string{"pending", "failed"}, time.Now().UTC()).
			Order("available_at ASC, event_id ASC").Limit(limit).Find(&deliveries).Error; err != nil {
			return err
		}
		for _, delivery := range deliveries {
			var event Event
			if err := tx.Where("id = ?", delivery.EventID).First(&event).Error; err != nil {
				return err
			}
			if err := PublishKafkaRecord(ctx, d.producer, event); err != nil {
				message := err.Error()
				next := time.Now().UTC().Add(backoff(delivery.Attempt))
				if updateErr := tx.Model(&Delivery{}).Where("event_id = ? AND transport = ? AND destination = ?", delivery.EventID, delivery.Transport, delivery.Destination).Updates(map[string]any{"status": "failed", "attempt": gorm.Expr("attempt + 1"), "available_at": next, "last_error": message}).Error; updateErr != nil {
					return updateErr
				}
				if d.logger != nil {
					d.logger.WarnContext(ctx, "publish Kafka outbox delivery", "event_id", event.ID, "topic", delivery.Destination, "error", err)
				}
				continue
			}
			if err := markDeliveryPublished(tx, delivery, time.Now().UTC()); err != nil {
				return err
			}
			if err := refreshLegacyEventStatus(tx, event.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

func ensureKafkaDeliveries(tx *gorm.DB, enabled bool) error {
	if !enabled {
		return nil
	}
	var events []Event
	if err := tx.Where("status IN ?", []string{"pending", "failed"}).Order("created_at ASC").Limit(100).Find(&events).Error; err != nil {
		return err
	}
	for _, event := range events {
		for _, target := range KafkaRoutes(event.EventType, true) {
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&Delivery{EventID: event.ID, Transport: target.Transport, Destination: target.Destination, Status: "pending", AvailableAt: time.Now().UTC()}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<attempt) * time.Second
}
