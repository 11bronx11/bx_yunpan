package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
	redis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dblock"
)

type Dispatcher struct {
	db        *gorm.DB
	client    *asynq.Client
	logger    *slog.Logger
	config    config.Outbox
	locking   config.Locking
	aiEnabled bool
	// redis 为 nil 时退化为单副本直接轮询，不做选主。
	redis redis.UniversalClient
}

func NewDispatcher(db *gorm.DB, client *asynq.Client, logger *slog.Logger, cfg config.Outbox) *Dispatcher {
	return &Dispatcher{db: db, client: client, logger: logger, config: cfg, aiEnabled: true}
}

// WithAIEnabled controls whether object events fan out to the AI queue.
func (d *Dispatcher) WithAIEnabled(enabled bool) *Dispatcher {
	d.aiEnabled = enabled
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
		var events []Event
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = 'pending' AND available_at <= ?", time.Now().UTC()).
			Order("created_at ASC").Limit(d.config.BatchSize).Find(&events).Error; err != nil {
			return err
		}
		for _, event := range events {
			payload := payloadWithTrace(event, d.logger)
			for _, target := range fanout(event.EventType, d.aiEnabled) {
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
				_, err := d.client.EnqueueContext(ctx, asynq.NewTask(target.taskType, payload), options...)
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

// TracePayloadKey 是 Asynq payload 里承载 trace context 的字段名。
// 业务 handler 用具名 struct 反序列化，多出这个字段不会影响它们。
const TracePayloadKey = "_trace"

// payloadWithTrace 把 outbox 行上的 trace context 合并进任务 payload，
// 让 Worker / aisvc 侧能 Extract 出来当 parent span。
func payloadWithTrace(event Event, logger *slog.Logger) []byte {
	if len(event.TraceContext) == 0 {
		return event.Payload
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(event.Payload, &fields); err != nil {
		// payload 不是 JSON 对象时原样投递，不因为 trace 丢失影响业务投递。
		return event.Payload
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
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
	switch eventType {
	case EventObjectReady:
		targets := []dispatchTarget{{taskType: EventObjectReady, queue: "media"}}
		if aiEnabled {
			targets = append(targets, dispatchTarget{taskType: EventAIIndexRequested, queue: "ai"})
		}
		return targets
	case EventObjectGCRequested:
		return []dispatchTarget{{taskType: EventObjectGCRequested, queue: "maintenance"}}
	case EventObjectVerifyRequested:
		return []dispatchTarget{{taskType: EventObjectVerifyRequested, queue: "object"}}
	case EventAIReprocessRequested:
		if !aiEnabled {
			return nil
		}
		return []dispatchTarget{{taskType: EventAIReprocessRequested, queue: "ai"}}
	default:
		return []dispatchTarget{{taskType: eventType, queue: "maintenance"}}
	}
}
