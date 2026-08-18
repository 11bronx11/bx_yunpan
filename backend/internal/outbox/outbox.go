package outbox

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
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
}

func (Event) TableName() string { return "outbox_events" }

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
	}
	return tx.Create(&event).Error
}
