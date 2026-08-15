package outbox

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

type Dispatcher struct {
	db     *gorm.DB
	client *asynq.Client
	logger *slog.Logger
	config config.Outbox
}

func NewDispatcher(db *gorm.DB, client *asynq.Client, logger *slog.Logger, cfg config.Outbox) *Dispatcher {
	return &Dispatcher{db: db, client: client, logger: logger, config: cfg}
}

func (d *Dispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(d.config.PollInterval)
	defer ticker.Stop()
	for {
		if err := d.dispatchBatch(ctx); err != nil {
			d.logger.ErrorContext(ctx, "dispatch outbox", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *Dispatcher) dispatchBatch(ctx context.Context) error {
	return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var events []Event
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = 'pending' AND available_at <= ?", time.Now().UTC()).
			Order("created_at ASC").Limit(d.config.BatchSize).Find(&events).Error; err != nil {
			return err
		}
		for _, event := range events {
			options := []asynq.Option{asynq.TaskID(event.ID.String()), asynq.MaxRetry(d.config.MaxRetry), asynq.Timeout(d.config.TaskTimeout)}
			switch event.EventType {
			case EventObjectGCRequested:
				options = append(options, asynq.ProcessAt(time.Now().UTC().Add(d.config.GCDelay)), asynq.Queue("maintenance"))
			case EventObjectVerifyRequested:
				options = append(options, asynq.Queue("object"))
			case EventObjectReady:
				options = append(options, asynq.Queue("media"))
			case EventAIReprocessRequested:
				options = append(options, asynq.Queue("ai"))
			default:
				options = append(options, asynq.Queue("maintenance"))
			}
			_, err := d.client.EnqueueContext(ctx, asynq.NewTask(event.EventType, event.Payload), options...)
			if err != nil && !errors.Is(err, asynq.ErrTaskIDConflict) {
				return err
			}
			now := time.Now().UTC()
			if err := tx.Model(&Event{}).Where("id = ?", event.ID).
				Updates(map[string]any{"status": "published", "published_at": now, "attempt": gorm.Expr("attempt + 1")}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
