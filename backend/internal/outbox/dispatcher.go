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
			for _, target := range fanout(event.EventType) {
				options := []asynq.Option{
					// TaskID 带上任务类型：同一 outbox 行扇出的两个任务要有不同
					// 的 ID，否则第二个会被判为重复而丢弃。
					asynq.TaskID(event.ID.String() + ":" + target.taskType),
					asynq.MaxRetry(d.config.MaxRetry),
					asynq.Timeout(d.config.TaskTimeout),
					asynq.Queue(target.queue),
				}
				if target.taskType == EventObjectGCRequested {
					options = append(options, asynq.ProcessAt(time.Now().UTC().Add(d.config.GCDelay)))
				}
				_, err := d.client.EnqueueContext(ctx, asynq.NewTask(target.taskType, event.Payload), options...)
				if err != nil && !errors.Is(err, asynq.ErrTaskIDConflict) {
					return err
				}
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

// dispatchTarget 是一条 outbox 行要投递到的一个 Asynq 任务。
type dispatchTarget struct {
	taskType string
	queue    string
}

// fanout 把一个事件类型映射到一到多个 Asynq 任务。
//
// object.ready 是唯一需要扇出的事件：缩略图留在 worker 的 media 队列，
// AI 索引进 ai 队列由 aisvc 消费。两个任务共用同一份 payload，各自独立
// 重试，任一失败不会拖累另一个。
func fanout(eventType string) []dispatchTarget {
	switch eventType {
	case EventObjectReady:
		return []dispatchTarget{
			{taskType: EventObjectReady, queue: "media"},
			{taskType: EventAIIndexRequested, queue: "ai"},
		}
	case EventObjectGCRequested:
		return []dispatchTarget{{taskType: EventObjectGCRequested, queue: "maintenance"}}
	case EventObjectVerifyRequested:
		return []dispatchTarget{{taskType: EventObjectVerifyRequested, queue: "object"}}
	case EventAIReprocessRequested:
		return []dispatchTarget{{taskType: EventAIReprocessRequested, queue: "ai"}}
	default:
		return []dispatchTarget{{taskType: eventType, queue: "maintenance"}}
	}
}
