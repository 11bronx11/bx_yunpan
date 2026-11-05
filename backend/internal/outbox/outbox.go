package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
)

const (
	EventFileCreated           = eventcontract.EventFileCreatedV1
	EventShareImported         = eventcontract.EventShareImportedV1
	EventObjectVerifyRequested = eventcontract.EventObjectVerifyRequestedV1
	EventObjectReady           = eventcontract.EventObjectReadyV1
	EventObjectGCRequested     = eventcontract.EventObjectGCRequestedV1
	EventAIReprocessRequested  = eventcontract.EventAIReprocessRequestedV1
	// EventAIIndexRequested 不由业务事务写入 outbox，而是 dispatcher 投递
	// EventObjectReady 时额外扇出的任务类型：对象就绪要同时触发缩略图
	// （worker，media 队列）与 AI 索引（aisvc，ai 队列），而 RabbitMQ 一个任务
	// 只会被一个消费者处理，所以在投递侧扇出成两个任务。
	EventAIIndexRequested        = eventcontract.EventAIIndexRequestedV1
	EventStorageOperationRecover = eventcontract.EventStorageOperationRecoverV1
	EventUploadOperationRecover  = eventcontract.EventUploadOperationRecoverV1
	EventMediaThumbnailRequested = eventcontract.EventMediaThumbnailRequestedV1
	TaskSearchProjectV1          = "search.project.v1"
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
		EventStorageOperationRecover,
		EventUploadOperationRecover,
		EventMediaThumbnailRequested,
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
	// These fields are nullable during the expand/contract window. Legacy rows
	// retain a nil aggregate version because the historical value is unknown.
	MessageKind      *eventcontract.MessageKind `gorm:"type:varchar(32)"`
	AggregateVersion *int64                     `gorm:"type:bigint"`
	Producer         *string                    `gorm:"type:varchar(128)"`
	OccurredAt       *time.Time                 `gorm:"type:timestamptz"`
	// TraceContext 是写入事务时注入的 W3C trace context，供异步消费侧续接
	// 同一条 trace。与业务数据同一事务提交，不引入新的一致性问题。
	TraceContext datatypes.JSON `gorm:"type:jsonb"`
}

func (Event) TableName() string { return "outbox_events" }

// Delivery is the per-transport state for one outbox event. The composite key
// makes fan-out destinations independent and makes retries safe to repeat.
type Delivery struct {
	EventID        uuid.UUID `gorm:"type:uuid;primaryKey"`
	Transport      string    `gorm:"primaryKey"`
	Destination    string    `gorm:"primaryKey"`
	Status         string
	Attempt        int
	AvailableAt    time.Time
	NotBefore      *time.Time
	TaskRetryCount int
	TaskMaxRetry   *int
	TaskTimeoutMS  *int64 `gorm:"column:task_timeout_ms"`
	LeaseUntil     *time.Time
	LeaseToken     *uuid.UUID `gorm:"type:uuid"`
	PublishedAt    *time.Time
	LastError      *string
}

func (Delivery) TableName() string { return "outbox_deliveries" }

type Recovery struct {
	RecoveryKey   string    `gorm:"primaryKey"`
	EventID       uuid.UUID `gorm:"type:uuid"`
	Reason        string
	ResourceType  string
	ResourceID    uuid.UUID `gorm:"type:uuid"`
	TargetVersion *int64    `gorm:"type:bigint"`
	CreatedAt     time.Time
	UpdatedAt     time.Time
	StoppedAt     *time.Time
}

func (Recovery) TableName() string { return "outbox_recoveries" }

// EnsureRecovery writes one deterministic recovery event and its audit row in
// the caller's transaction. Re-running a scan is therefore a no-op for the
// same resource/version/work type while preserving the original event ID.
func EnsureRecovery(tx *gorm.DB, recoveryKey, reason, resourceType string, resourceID uuid.UUID, targetVersion int64, input eventcontract.EnvelopeInput) (bool, error) {
	if tx == nil || recoveryKey == "" || reason == "" || resourceType == "" || resourceID == uuid.Nil {
		return false, fmt.Errorf("outbox: invalid recovery metadata")
	}
	eventID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("bx-yunpan/recovery/"+recoveryKey))
	input.EventID = eventID
	envelope, err := eventcontract.New(input)
	if err != nil {
		return false, err
	}
	var existing Recovery
	err = tx.Where("recovery_key = ?", recoveryKey).First(&existing).Error
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, err
	}
	now := time.Now().UTC()
	event := Event{
		ID: envelope.EventID, AggregateType: envelope.AggregateType, AggregateID: envelope.AggregateID,
		EventType: envelope.EventType, EventVersion: envelope.EventVersion, Payload: envelope.Payload,
		Status: "pending", AvailableAt: now, MessageKind: ptr(envelope.MessageKind),
		AggregateVersion: envelope.AggregateVersion, Producer: ptr(envelope.Producer), OccurredAt: ptr(envelope.OccurredAt),
	}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&event).Error; err != nil {
		return false, err
	}
	if err := addDeliveries(tx, event, true); err != nil {
		return false, err
	}
	version := targetVersion
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&Recovery{RecoveryKey: recoveryKey, EventID: eventID, Reason: reason, ResourceType: resourceType, ResourceID: resourceID, TargetVersion: &version, CreatedAt: now, UpdatedAt: now})
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		return false, nil
	}
	return true, nil
}

// HasActiveDelivery reports whether a retryable delivery already exists for
// the same event type/aggregate and destination. A recovery scan uses this to
// reuse pending/failed work instead of creating a second command event.
func HasActiveDelivery(tx *gorm.DB, eventType string, aggregateID uuid.UUID) (bool, error) {
	if tx == nil || eventType == "" || aggregateID == uuid.Nil {
		return false, fmt.Errorf("outbox: invalid delivery lookup")
	}
	var rows []struct {
		EventID     uuid.UUID
		Destination string
		Transport   string
		Status      string
	}
	if err := tx.Table("outbox_deliveries d").Select("d.event_id,d.destination,d.transport,d.status").
		Joins("JOIN outbox_events e ON e.id=d.event_id").Where("e.aggregate_id = ?", aggregateID).Scan(&rows).Error; err != nil {
		return false, err
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
		if row.Status != "published" {
			return true, nil
		}
		if row.Transport == "rabbitmq" || row.Transport == "asynq" {
			var succeeded int64
			if err := tx.Table("task_results").Where("task_id = ? AND status='succeeded'", row.EventID.String()+":"+parsed.Destination).Count(&succeeded).Error; err != nil {
				return false, err
			}
			if succeeded == 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

// TraceCarrier 是写入 outbox 行与 RabbitMQ payload 的 trace context 载体，
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

// Add accepts the new complete envelope form, while retaining the historical
// five-argument form during the expand/contract compatibility window:
// Add(tx, envelope) or Add(tx, aggregateType, aggregateID, eventType, payload).
func Add(tx *gorm.DB, first any, legacy ...any) error {
	if tx == nil {
		return fmt.Errorf("outbox: nil transaction")
	}
	if envelope, ok := first.(eventcontract.Envelope); ok && len(legacy) == 0 {
		return AddEnvelope(tx, envelope)
	}
	if len(legacy) != 3 {
		return fmt.Errorf("outbox: expected envelope or aggregateType, aggregateID, eventType, payload")
	}
	aggregateType, ok := first.(string)
	if !ok {
		return fmt.Errorf("outbox: aggregate type must be a string")
	}
	aggregateID, ok := legacy[0].(uuid.UUID)
	if !ok {
		return fmt.Errorf("outbox: aggregate id must be uuid.UUID")
	}
	eventType, ok := legacy[1].(string)
	if !ok {
		return fmt.Errorf("outbox: event type must be a string")
	}
	payload := legacy[2]
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	var messageKind *eventcontract.MessageKind
	if definition, ok := eventcontract.Lookup(eventType); ok {
		kind := definition.MessageKind
		messageKind = &kind
	}
	now := time.Now().UTC()
	producer := "core-api"
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
		MessageKind:   messageKind,
		Producer:      &producer,
		OccurredAt:    &now,
	}
	if err := tx.Create(&event).Error; err != nil {
		return err
	}
	// Add is the compatibility writer used by existing services. It dual-writes
	// delivery rows while preserving the legacy event row for old relays.
	return addDeliveries(tx, event, true)
}

// AddEnvelope writes a validated typed contract and its delivery rows in the
// caller's transaction. Event creation and delivery creation therefore commit
// or roll back together.
func AddEnvelope(tx *gorm.DB, envelope eventcontract.Envelope) error {
	return addEnvelopeWithSearch(tx, envelope, false)
}

func addEnvelopeWithSearch(tx *gorm.DB, envelope eventcontract.Envelope, searchEnabled bool) error {

	if tx == nil {
		return fmt.Errorf("outbox: nil transaction")
	}
	if err := envelope.Validate(); err != nil {
		return err
	}
	encodedTrace, err := json.Marshal(envelope.Trace)
	if err != nil {
		return fmt.Errorf("marshal trace context: %w", err)
	}
	var traceContext datatypes.JSON
	if envelope.Trace != nil {
		traceContext = encodedTrace
	}
	event := Event{
		ID:               envelope.EventID,
		AggregateType:    envelope.AggregateType,
		AggregateID:      envelope.AggregateID,
		EventType:        envelope.EventType,
		EventVersion:     envelope.EventVersion,
		Payload:          envelope.Payload,
		Status:           "pending",
		AvailableAt:      time.Now().UTC(),
		TraceContext:     traceContext,
		MessageKind:      ptr(envelope.MessageKind),
		AggregateVersion: envelope.AggregateVersion,
		Producer:         ptr(envelope.Producer),
		OccurredAt:       ptr(envelope.OccurredAt),
	}
	if err := tx.Create(&event).Error; err != nil {
		return err
	}
	return addDeliveriesWithSearch(tx, event, true, searchEnabled)
}

func ptr[T any](value T) *T { return &value }

func addDeliveries(tx *gorm.DB, event Event, aiEnabled bool) error {
	return addDeliveriesWithSearch(tx, event, aiEnabled, false)
}

func addDeliveriesWithSearch(tx *gorm.DB, event Event, aiEnabled, searchEnabled bool) error {

	now := time.Now().UTC()
	targets := append(Routes(event.EventType, aiEnabled), SearchRoutes(event.EventType, searchEnabled)...)
	for _, target := range targets {
		var existing []Delivery
		if err := tx.Where("event_id = ? AND transport IN ?", event.ID, []string{"asynq", "rabbitmq"}).Find(&existing).Error; err != nil {
			return err
		}
		owned := false
		for _, row := range existing {
			legacy := row
			legacy.Transport = "rabbitmq"
			parsed, ok := targetFromDelivery(legacy)
			if ok && parsed.Destination == target.Destination {
				owned = true
				break
			}
		}
		if owned {
			continue
		}
		delivery := Delivery{
			EventID: event.ID, Transport: target.Transport, Destination: target.Destination,
			Status: "pending", Attempt: 0, AvailableAt: now,
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&delivery).Error; err != nil {
			return err
		}
	}
	return nil
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
