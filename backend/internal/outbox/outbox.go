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
)

func EventTypes() []string {
	return []string{
		EventFileCreated,
		EventShareImported,
		EventObjectVerifyRequested,
		EventObjectReady,
		EventObjectGCRequested,
		EventAIReprocessRequested,
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
