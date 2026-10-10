package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"
	redis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dblock"
)

type Dispatcher struct {
	db            *gorm.DB
	client        taskqueue.Publisher
	logger        *slog.Logger
	config        config.Outbox
	locking       config.Locking
	aiEnabled     bool
	searchEnabled bool
	metrics       *Metrics
	// redis 为 nil 时退化为单副本直接轮询，不做选主。
	redis          redis.UniversalClient
	markPublished  func(*gorm.DB, Delivery, time.Time) error
	publishTimeout time.Duration
}

func NewDispatcher(db *gorm.DB, client taskqueue.Publisher, logger *slog.Logger, cfg config.Outbox) *Dispatcher {
	return newDispatcher(db, client, logger, cfg)
}

func newDispatcher(db *gorm.DB, client taskqueue.Publisher, logger *slog.Logger, cfg config.Outbox) *Dispatcher {
	return &Dispatcher{db: db, client: client, logger: logger, config: cfg, aiEnabled: true, markPublished: markDeliveryPublished, publishTimeout: 5 * time.Second}
}

func (d *Dispatcher) WithPublishTimeout(timeout time.Duration) *Dispatcher {
	if timeout > 0 {
		d.publishTimeout = timeout
	}
	return d
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
// 锁失效的后果是两个副本同时投递，而 任务侧幂等与数据库执行记录
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
		processed, err := d.dispatchBatchCount(ctx)
		if err != nil {
			d.logger.ErrorContext(ctx, "dispatch outbox", "error", err)
		}
		// Drain a backlog without imposing an artificial batch/interval ceiling.
		// Check cancellation and leadership between every bounded batch.
		if err == nil && processed >= d.config.BatchSize && processed > 0 {
			select {
			case <-ctx.Done():
				return
			case <-lost:
				return
			default:
				continue
			}
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
	_, err := d.dispatchBatchCount(ctx)
	return err
}

func (d *Dispatcher) dispatchBatchCount(ctx context.Context) (int, error) {
	if err := d.prepareDeliveries(ctx); err != nil {
		return 0, err
	}
	claimed, err := d.claimDeliveries(ctx)
	if err != nil {
		return 0, err
	}
	var firstErr error
	for _, item := range claimed {
		if err := d.sendDelivery(ctx, item); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if d.logger != nil {
				d.logger.WarnContext(ctx, "dispatch outbox delivery", "event_id", item.event.ID, "error", err)
			}
		}
	}
	return len(claimed), firstErr
}

type claimedDelivery struct {
	delivery Delivery
	event    Event
}

func (d *Dispatcher) prepareDeliveries(ctx context.Context) error {
	return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := ensureLegacyDeliveries(tx, d.aiEnabled); err != nil {
			return err
		}
		if err := ensureSearchDeliveries(tx, d.searchEnabled); err != nil {
			return err
		}
		var now time.Time
		if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return err
		}
		if _, err := RecoverPublishedBatch(tx, now, 15*time.Minute, d.config.BatchSize, d.aiEnabled, d.searchEnabled); err != nil {
			return err
		}
		// Pin GC eligibility once. Retries, restarts and config changes never
		// move a persisted deadline earlier or start a fresh delay.
		if err := tx.Exec(`UPDATE outbox_deliveries d SET not_before =
          CASE WHEN d.destination IN ('object.gc.v1@maintenance', 'object.gc_requested.v1@maintenance')
          THEN GREATEST(e.available_at, e.created_at + (? * interval '1 millisecond'))
          ELSE e.available_at END
          FROM outbox_events e WHERE d.event_id=e.id AND d.transport='rabbitmq' AND d.not_before IS NULL`, d.config.GCDelay.Milliseconds()).Error; err != nil {
			return err
		}
		return reportUnroutedEvents(ctx, tx, d.logger, d.aiEnabled, d.metrics)
	})
}

// claimDeliveries 只持有数据库锁，不在事务中访问 Redis。
func (d *Dispatcher) claimDeliveries(ctx context.Context) ([]claimedDelivery, error) {
	items := make([]claimedDelivery, 0, d.config.BatchSize)
	recovered := 0
	err := d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		var deliveries []Delivery
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("transport = 'rabbitmq' AND not_before <= ? AND ((status IN ? AND available_at <= ?) OR (status = 'sending' AND lease_until <= ?))", now, []string{"pending", "failed"}, now, now).
			Order("available_at ASC, event_id ASC").Limit(d.config.BatchSize).Find(&deliveries).Error; err != nil {
			return err
		}
		if len(deliveries) == 0 {
			return nil
		}
		eventIDs := make([]uuid.UUID, 0, len(deliveries))
		for _, delivery := range deliveries {
			eventIDs = append(eventIDs, delivery.EventID)
		}
		var events []Event
		if err := tx.Where("id IN ?", eventIDs).Find(&events).Error; err != nil {
			return err
		}
		eventByID := make(map[uuid.UUID]Event, len(events))
		for _, event := range events {
			eventByID[event.ID] = event
		}
		for index := range deliveries {
			if deliveries[index].Status == "sending" {
				recovered++
			}
			event, ok := eventByID[deliveries[index].EventID]
			if !ok {
				return fmt.Errorf("outbox event %s not found", deliveries[index].EventID)
			}
			token := uuid.Must(uuid.NewV7())
			leaseUntil := now.Add(time.Duration(d.config.BatchSize)*(d.publishTimeout+time.Second) + time.Minute)
			if leaseUntil.Before(now.Add(time.Minute)) {
				leaseUntil = now.Add(time.Minute)
			}
			if err := tx.Model(&Delivery{}).Where("event_id = ? AND transport = ? AND destination = ?", deliveries[index].EventID, deliveries[index].Transport, deliveries[index].Destination).
				Updates(map[string]any{"status": "sending", "attempt": gorm.Expr("attempt + 1"), "lease_until": leaseUntil, "lease_token": token, "last_error": nil}).Error; err != nil {
				return err
			}
			deliveries[index].Status = "sending"
			deliveries[index].Attempt++
			deliveries[index].LeaseUntil = &leaseUntil
			deliveries[index].LeaseToken = &token
			items = append(items, claimedDelivery{delivery: deliveries[index], event: event})
		}
		return nil
	})
	if err == nil && d.metrics != nil {
		d.metrics.LeaseRecoveries.Add(float64(recovered))
	}
	return items, err
}

func (d *Dispatcher) sendDelivery(ctx context.Context, item claimedDelivery) error {
	delivery, event := item.delivery, item.event
	target, ok := targetFromDelivery(delivery)
	if !ok {
		return d.failDelivery(delivery, "unsupported delivery transport or destination")
	}
	if target.Queue == "ai" && !d.aiEnabled {
		return d.ackDelivery(delivery, event)
	}
	if target, ok = targetAllowedForEventWithSearch(event.EventType, d.aiEnabled, d.searchEnabled, delivery); !ok {
		return d.failDelivery(delivery, "unsupported delivery transport or destination")
	}
	payload := payloadWithTrace(event, d.logger)
	maxRetry, timeout := d.config.MaxRetry, d.config.TaskTimeout
	if delivery.TaskMaxRetry != nil {
		maxRetry = *delivery.TaskMaxRetry
	}
	if delivery.TaskTimeoutMS != nil {
		timeout = time.Duration(*delivery.TaskTimeoutMS) * time.Millisecond
	}
	err := d.client.Publish(ctx, taskqueue.Task{ID: event.ID.String() + ":" + target.Destination, Kind: target.TaskType, Queue: target.Queue, Body: payload, RetryCount: delivery.TaskRetryCount, MaxRetry: maxRetry, Timeout: timeout})
	if err != nil {
		return d.failDelivery(delivery, err.Error())
	}
	return d.ackDelivery(delivery, event)
}

func (d *Dispatcher) ackDelivery(delivery Delivery, event Event) error {
	err := d.db.Transaction(func(tx *gorm.DB) error {
		if err := d.markPublished(tx, delivery, time.Now().UTC()); err != nil {
			return err
		}
		return refreshLegacyEventStatus(tx, event.ID)
	})
	if err == nil {
		return nil
	}
	// ACK 失败后释放 claim，下一轮可用稳定 TaskID 重试。
	_ = d.db.Model(&Delivery{}).Where("event_id = ? AND transport = ? AND destination = ? AND status = 'sending' AND lease_token = ?", delivery.EventID, delivery.Transport, delivery.Destination, delivery.LeaseToken).
		Updates(map[string]any{"status": "pending", "available_at": time.Now().UTC(), "lease_until": nil, "lease_token": nil}).Error
	return err
}

func (d *Dispatcher) failDelivery(delivery Delivery, message string) error {
	err := d.db.Model(&Delivery{}).
		Where("event_id = ? AND transport = ? AND destination = ? AND status = 'sending' AND lease_token = ?", delivery.EventID, delivery.Transport, delivery.Destination, delivery.LeaseToken).
		Updates(map[string]any{"status": "failed", "available_at": time.Now().UTC().Add(backoff(delivery.Attempt)), "lease_until": nil, "lease_token": nil, "last_error": message}).Error
	return errors.Join(errors.New(message), err)
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
		if err := addDeliveriesWithSearch(tx, event, false, true); err != nil {
			return err
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
		// Reconcile each currently allowed destination independently. During a
		// rolling upgrade an older relay may have created only a subset of the
		// fan-out (for example media without AI); counting existing rows would
		// permanently lose the newly enabled destination.
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
	query := tx.Model(&Delivery{}).
		Where("event_id = ? AND transport = ? AND destination = ?", delivery.EventID, delivery.Transport, delivery.Destination)
	if delivery.LeaseToken != nil {
		query = query.Where("status = 'sending' AND lease_token = ?", *delivery.LeaseToken)
	}
	updates := map[string]any{"status": "published", "published_at": now, "lease_until": nil, "lease_token": nil, "last_error": nil}
	if delivery.LeaseToken == nil {
		updates["attempt"] = gorm.Expr("attempt + 1")
	}
	result := query.Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		return nil
	}
	return errors.New("outbox delivery lease lost")
}

func markDeliveryFailed(tx *gorm.DB, delivery Delivery, message string) error {
	next := time.Now().UTC().Add(time.Second)
	query := tx.Model(&Delivery{}).
		Where("event_id = ? AND transport = ? AND destination = ?", delivery.EventID, delivery.Transport, delivery.Destination)
	if delivery.LeaseToken != nil {
		query = query.Where("status = 'sending' AND lease_token = ?", *delivery.LeaseToken)
	}
	return query.Updates(map[string]any{"status": "failed", "available_at": next, "lease_until": nil, "lease_token": nil, "last_error": message}).Error
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

// TracePayloadKey 是 RabbitMQ payload 里承载 trace context 的字段名。
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
	if _, exists := fields["task_id"]; !exists {
		fields["task_id"] = fields["event_id"]
	}
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

// dispatchTarget 是一条 outbox 行要投递到的一个 RabbitMQ 任务。
type dispatchTarget struct {
	taskType string
	queue    string
}

// fanout 把一个事件类型映射到一到多个 RabbitMQ 任务。
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

func backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<attempt) * time.Second
}
