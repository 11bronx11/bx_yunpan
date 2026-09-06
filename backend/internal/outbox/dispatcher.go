package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	redis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dblock"
)

type Dispatcher struct {
	db            *gorm.DB
	client        taskEnqueuer
	logger        *slog.Logger
	config        config.Outbox
	locking       config.Locking
	aiEnabled     bool
	searchEnabled bool
	metrics       *Metrics
	// redis 为 nil 时退化为单副本直接轮询，不做选主。
	redis         redis.UniversalClient
	markPublished func(*gorm.DB, Delivery, time.Time) error
}

type taskEnqueuer interface {
	EnqueueContext(context.Context, *asynq.Task, ...asynq.Option) (*asynq.TaskInfo, error)
}

func NewDispatcher(db *gorm.DB, client *asynq.Client, logger *slog.Logger, cfg config.Outbox) *Dispatcher {
	return newDispatcher(db, client, logger, cfg)
}

func newDispatcher(db *gorm.DB, client taskEnqueuer, logger *slog.Logger, cfg config.Outbox) *Dispatcher {
	return &Dispatcher{db: db, client: client, logger: logger, config: cfg, aiEnabled: true, markPublished: markDeliveryPublished}
}

// WithMetrics attaches the worker-owned registry used for outbox backlog
// health. A nil registerer is accepted for callers that do not expose metrics.
func (d *Dispatcher) WithMetrics(metrics *Metrics) *Dispatcher {
	d.metrics = metrics
	return d
}

// WithAIEnabled controls whether object events fan out to the AI queue.
func (d *Dispatcher) WithAIEnabled(enabled bool) *Dispatcher {
	d.aiEnabled = enabled
	return d
}

func (d *Dispatcher) WithSearchEnabled(enabled bool) *Dispatcher {
	d.searchEnabled = enabled
	return d
}

// WithLeaderLock 打开多副本选主：只有持锁的副本投递。
//
// 为什么用 Redis 锁而不是 advisory lock：dispatcher 的一轮投递本身是一个
// 事务，但选主要跨越连续多轮、跨越事务边界，没有一个能覆盖全程的事务
// 可以挂 advisory lock。
//
// 锁失效的后果是两个副本同时投递，而 Asynq 的 TaskID 去重与任务侧幂等
// 会吸收重复，所以不需要更强的互斥保证。
func (d *Dispatcher) WithLeaderLock(client redis.UniversalClient, locking config.Locking) *Dispatcher {
	d.redis = client
	d.locking = locking
	return d
}

func (d *Dispatcher) Run(ctx context.Context) {
	if d.redis != nil && d.locking.DispatcherLeaderEnabled {
		d.runAsFollower(ctx)
		return
	}
	d.poll(ctx, nil)
}

// runAsFollower 反复尝试抢锁：抢到就投递并由 watchdog 续期，没抢到就
// 等一轮再试。丢锁后立刻停止投递并回到抢锁状态。
func (d *Dispatcher) runAsFollower(ctx context.Context) {
	for ctx.Err() == nil {
		lock := dblock.NewRedisLock(d.redis, d.locking.LeaderKey, d.locking.LeaderTTL)
		err := lock.Acquire(ctx)
		switch {
		case err == nil:
			d.logger.InfoContext(ctx, "outbox dispatcher acquired leadership", "key", d.locking.LeaderKey)
			d.lead(ctx, lock)
		case errors.Is(err, dblock.ErrNotAcquired):
			// 正常状态：别的副本在投递，本副本待命。
		case ctx.Err() != nil:
			return
		default:
			d.logger.WarnContext(ctx, "acquire dispatcher leadership", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(d.config.PollInterval):
		}
	}
}

// lead 在持锁期间投递。watchdog 报告丢锁时 poll 立刻返回，随后释放锁
// 并回到抢锁循环。
func (d *Dispatcher) lead(ctx context.Context, lock *dblock.RedisLock) {
	leaseCtx, stopWatchdog := context.WithCancel(ctx)
	defer stopWatchdog()
	lost := lock.Watchdog(leaseCtx, d.logger)

	d.poll(ctx, lost)

	stopWatchdog()
	// 用独立 context 释放：ctx 可能已因停机取消，复用它连 DEL 都发不出去。
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := lock.Release(releaseCtx); err != nil {
		d.logger.WarnContext(ctx, "release dispatcher leadership", "error", err)
	}
}

// poll 是投递主循环。lost 非 nil 时，它一旦关闭就退出——表示不再持有
// 选主锁，必须立即停手。
func (d *Dispatcher) poll(ctx context.Context, lost <-chan struct{}) {
	ticker := time.NewTicker(d.config.PollInterval)
	defer ticker.Stop()
	for {
		if err := d.dispatchBatch(ctx); err != nil {
			d.logger.ErrorContext(ctx, "dispatch outbox", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-lost:
			d.logger.WarnContext(ctx, "outbox dispatcher lost leadership", "key", d.locking.LeaderKey)
			return
		case <-ticker.C:
		}
	}
}

func (d *Dispatcher) dispatchBatch(ctx context.Context) error {
	return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := ensureLegacyDeliveries(tx, d.aiEnabled); err != nil {
			return err
		}
		if err := ensureSearchDeliveries(tx, d.searchEnabled); err != nil {
			return err
		}
		if err := reportUnroutedEvents(ctx, tx, d.logger, d.aiEnabled, d.metrics); err != nil {
			return err
		}
		var deliveries []Delivery
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status IN ? AND available_at <= ?", []string{"pending", "failed"}, time.Now().UTC()).
			Order("available_at ASC, event_id ASC").Limit(d.config.BatchSize).Find(&deliveries).Error; err != nil {
			return err
		}
		for _, delivery := range deliveries {
			var event Event
			if err := tx.First(&event, "id = ?", delivery.EventID).Error; err != nil {
				return err
			}
			target, ok := targetAllowedForEventWithSearch(event.EventType, d.aiEnabled, d.searchEnabled, delivery)
			if !ok {
				message := "unsupported delivery transport or destination"
				if err := markDeliveryFailed(tx, delivery, message); err != nil {
					return err
				}
				d.logger.ErrorContext(ctx, "outbox delivery unroutable", "event_id", event.ID, "transport", delivery.Transport, "destination", delivery.Destination)
				continue
			}
			// A delivery created before AI was disabled is intentionally marked
			// published without enqueueing: no consumer is enabled for that target.
			if target.Queue == "ai" && !d.aiEnabled {
				if err := markDeliveryPublished(tx, delivery, time.Now().UTC()); err != nil {
					return err
				}
				if err := refreshLegacyEventStatus(tx, event.ID); err != nil {
					return err
				}
				continue
			}
			payload := payloadWithTrace(event, d.logger)
			options := []asynq.Option{
				// TaskID is stable per event and destination. A relay restart
				// after Redis accepted a task is therefore harmless.
				asynq.TaskID(event.ID.String() + ":" + delivery.Destination),
				asynq.MaxRetry(d.config.MaxRetry),
				asynq.Timeout(d.config.TaskTimeout),
				asynq.Queue(target.Queue),
			}
			if target.TaskType == EventObjectGCRequested {
				options = append(options, asynq.ProcessAt(time.Now().UTC().Add(d.config.GCDelay)))
			}
			_, err := d.client.EnqueueContext(ctx, asynq.NewTask(target.TaskType, payload), options...)
			if err != nil && !errors.Is(err, asynq.ErrTaskIDConflict) {
				if markErr := markDeliveryFailed(tx, delivery, err.Error()); markErr != nil {
					return markErr
				}
				continue
			}
			if err := d.markPublished(tx, delivery, time.Now().UTC()); err != nil {
				return err
			}
			if err := refreshLegacyEventStatus(tx, event.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

func ensureSearchDeliveries(tx *gorm.DB, enabled bool) error {
	if !enabled {
		return nil
	}
	var events []Event
	if err := tx.Where("status IN ?", []string{"pending", "failed"}).Order("created_at ASC").Limit(100).Find(&events).Error; err != nil {
		return err
	}
	for _, event := range events {
		for _, target := range SearchRoutes(event.EventType, true) {
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&Delivery{EventID: event.ID, Transport: target.Transport, Destination: target.Destination, Status: "pending", AvailableAt: time.Now().UTC()}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureLegacyDeliveries is the compatibility bridge for rows written by a
// pre-000009 writer. It only creates explicit routes for known event types;
// unknown rows stay pending and are visible as unrouted rather than silently
// being sent to maintenance.
func ensureLegacyDeliveries(tx *gorm.DB, aiEnabled bool) error {
	var events []Event
	if err := tx.Where("status IN ?", []string{"pending", "failed"}).
		Order("created_at ASC").Limit(100).Find(&events).Error; err != nil {
		return err
	}
	for _, event := range events {
		var count int64
		if err := tx.Model(&Delivery{}).Where("event_id = ?", event.ID).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		if err := addDeliveries(tx, event, aiEnabled); err != nil {
			return err
		}
	}
	return nil
}

func reportUnroutedEvents(ctx context.Context, tx *gorm.DB, logger *slog.Logger, aiEnabled bool, metrics *Metrics) error {
	type unroutedRow struct {
		EventType string
		Count     int64
		Oldest    time.Time
	}
	var rows []unroutedRow
	if err := tx.Model(&Event{}).
		Select("event_type, COUNT(*) AS count, MIN(created_at) AS oldest").
		Where("status IN ?", []string{"pending", "failed"}).
		Where("NOT EXISTS (SELECT 1 FROM outbox_deliveries d WHERE d.event_id = outbox_events.id)").
		Group("event_type").
		Find(&rows).Error; err != nil {
		return err
	}
	counts := make([]UnroutedCount, 0, len(rows))
	for _, row := range rows {
		if len(Routes(row.EventType, aiEnabled)) != 0 {
			continue
		}
		counts = append(counts, UnroutedCount{EventType: row.EventType, Count: row.Count, Oldest: row.Oldest})
		if logger != nil {
			logger.ErrorContext(ctx, "outbox event has no enabled delivery route",
				"event_type", row.EventType, "count", row.Count, "oldest", row.Oldest)
		}
	}
	if metrics != nil {
		metrics.ObserveUnroutedCounts(counts, time.Now().UTC())
	}
	return nil
}

func markDeliveryPublished(tx *gorm.DB, delivery Delivery, now time.Time) error {
	return tx.Model(&Delivery{}).
		Where("event_id = ? AND transport = ? AND destination = ?", delivery.EventID, delivery.Transport, delivery.Destination).
		Updates(map[string]any{"status": "published", "published_at": now, "attempt": gorm.Expr("attempt + 1"), "last_error": nil}).Error
}

func markDeliveryFailed(tx *gorm.DB, delivery Delivery, message string) error {
	next := time.Now().UTC().Add(time.Second)
	return tx.Model(&Delivery{}).
		Where("event_id = ? AND transport = ? AND destination = ?", delivery.EventID, delivery.Transport, delivery.Destination).
		Updates(map[string]any{"status": "failed", "attempt": gorm.Expr("attempt + 1"), "available_at": next, "last_error": message}).Error
}

func refreshLegacyEventStatus(tx *gorm.DB, eventID uuid.UUID) error {
	var pending int64
	if err := tx.Model(&Delivery{}).Where("event_id = ? AND status <> 'published'", eventID).Count(&pending).Error; err != nil {
		return err
	}
	if pending != 0 {
		return nil
	}
	now := time.Now().UTC()
	return tx.Model(&Event{}).Where("id = ?", eventID).
		Updates(map[string]any{"status": "published", "published_at": now, "attempt": gorm.Expr("attempt + 1")}).Error
}

// TracePayloadKey 是 Asynq payload 里承载 trace context 的字段名。
// 业务 handler 用具名 struct 反序列化，多出这个字段不会影响它们。
const TracePayloadKey = "_trace"

// payloadWithTrace 把 outbox 行上的 trace context 合并进任务 payload，
// 让 Worker / aisvc 侧能 Extract 出来当 parent span。
func payloadWithTrace(event Event, logger *slog.Logger) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(event.Payload, &fields); err != nil {
		// payload 不是 JSON 对象时原样投递，不因为 trace 丢失影响业务投递。
		return event.Payload
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	fields["event_id"] = json.RawMessage(fmt.Sprintf("%q", event.ID.String()))
	fields["task_id"] = fields["event_id"]
	fields["event_type"] = json.RawMessage(fmt.Sprintf("%q", event.EventType))
	fields["event_version"] = json.RawMessage(strconv.Itoa(event.EventVersion))
	if event.AggregateVersion != nil {
		fields["aggregate_version"] = json.RawMessage(strconv.FormatInt(*event.AggregateVersion, 10))
	}
	if len(event.TraceContext) == 0 {
		merged, err := json.Marshal(fields)
		if err != nil {
			return event.Payload
		}
		return merged
	}
	fields[TracePayloadKey] = json.RawMessage(event.TraceContext)
	merged, err := json.Marshal(fields)
	if err != nil {
		logger.Warn("merge trace context into task payload", "event_id", event.ID, "error", err)
		return event.Payload
	}
	return merged
}

// dispatchTarget 是一条 outbox 行要投递到的一个 Asynq 任务。
type dispatchTarget struct {
	taskType string
	queue    string
}

// fanout 把一个事件类型映射到一到多个 Asynq 任务。
//
// object.ready 是唯一需要扇出的事件：缩略图留在 worker 的 media 队列；
// AI 开启时再把索引任务投到 aisvc 消费的 ai 队列。两个任务共用同一份
// payload，各自独立重试，任一失败不会拖累另一个。
func fanout(eventType string, aiEnabled bool) []dispatchTarget {
	targets := Routes(eventType, aiEnabled)
	if len(targets) == 0 {
		return nil
	}
	result := make([]dispatchTarget, 0, len(targets))
	for _, item := range targets {
		result = append(result, dispatchTarget{taskType: item.TaskType, queue: item.Queue})
	}
	return result
}
