package outbox

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const (
	EventFileCreated           = "file.created.v1"
	EventShareImported         = "share.imported.v1"
	EventObjectVerifyRequested = "object.verify_requested.v1"
	EventObjectReady           = "object.ready.v1"
	EventObjectGCRequested     = "object.gc_requested.v1"
	EventAIReprocessRequested  = "ai.reprocess_requested.v1"
	// EventAIIndexRequested 不由业务事务写入 outbox，而是 dispatcher 投递
	// EventObjectReady 时额外扇出的任务类型：对象就绪要同时触发缩略图
	// （worker，media 队列）与 AI 索引（aisvc，ai 队列），而 Asynq 一个任务
	// 只会被一个消费者处理，所以在投递侧扇出成两个任务。
	EventAIIndexRequested = "ai.index_requested.v1"
)

func EventTypes() []string {
	return []string{
		EventFileCreated,
		EventShareImported,
		EventObjectVerifyRequested,
		EventObjectReady,
		EventObjectGCRequested,
		EventAIReprocessRequested,
		EventAIIndexRequested,
	}
}

type Event struct {
	ID            uuid.UUID `gorm:"type:uuid;primaryKey"`
	AggregateType string
	AggregateID   uuid.UUID `gorm:"type:uuid"`
	EventType     string
	EventVersion  int
	Payload       json.RawMessage
	Status        string
	AvailableAt   time.Time
	PublishedAt   *time.Time
	Attempt       int
	CreatedAt     time.Time
	// TraceContext 是写入事务时注入的 W3C trace context，供异步消费侧续接
	// 同一条 trace。与业务数据同一事务提交，不引入新的一致性问题。
	TraceContext datatypes.JSON `gorm:"type:jsonb"`
}

func (Event) TableName() string { return "outbox_events" }

// TraceCarrier 是写入 outbox 行与 Asynq payload 的 trace context 载体，
// 形如 {"traceparent": "...", "tracestate": "..."}。
type TraceCarrier map[string]string

func (c TraceCarrier) Get(key string) string { return c[key] }

func (c TraceCarrier) Set(key, value string) { c[key] = value }

func (c TraceCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for key := range c {
		keys = append(keys, key)
	}
	return keys
}

func Add(tx *gorm.DB, aggregateType string, aggregateID uuid.UUID, eventType string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	event := Event{
		ID:            uuid.Must(uuid.NewV7()),
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		EventType:     eventType,
		EventVersion:  1,
		Payload:       encoded,
		Status:        "pending",
		AvailableAt:   time.Now().UTC(),
		TraceContext:  injectTraceContext(tx.Statement.Context),
	}
	return tx.Create(&event).Error
}

// injectTraceContext 把当前 span context 序列化。无有效 span 时返回 nil，
// 让列保持 NULL 而不是写入空对象。
func injectTraceContext(ctx context.Context) datatypes.JSON {
	if ctx == nil || !trace.SpanContextFromContext(ctx).IsValid() {
		return nil
	}
	carrier := TraceCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) == 0 {
		return nil
	}
	encoded, err := json.Marshal(carrier)
	if err != nil {
		return nil
	}
	return encoded
}
