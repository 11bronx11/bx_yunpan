package outbox

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrInvalidConsumer = errors.New("invalid event consumer")

// Consumption records the completion of one consumer's handling of an event.
// event_consumptions intentionally has no foreign key to outbox_events: long
// lived/replayed events may outlive local outbox retention.
type Consumption struct {
	EventID      uuid.UUID `gorm:"type:uuid;primaryKey"`
	ConsumerName string    `gorm:"primaryKey"`
	ProcessedAt  time.Time
}

func (Consumption) TableName() string { return "event_consumptions" }

// ConsumeOnce runs effect at most once for the (event, consumer) pair in the
// transaction represented by tx.  Callers must pass a transaction when the
// effect writes PostgreSQL state: the marker and the side effect then commit
// or roll back together.  A conflict returns handled=false and never invokes
// effect, allowing a repeated delivery to ACK safely.
func ConsumeOnce(tx *gorm.DB, eventID uuid.UUID, consumerName string, effect func(*gorm.DB) error) (handled bool, err error) {
	if tx == nil {
		return false, errors.New("consume once requires a database handle")
	}
	consumerName = strings.TrimSpace(consumerName)
	if eventID == uuid.Nil || consumerName == "" {
		return false, ErrInvalidConsumer
	}
	result := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "event_id"}, {Name: "consumer_name"}},
		DoNothing: true,
	}).Create(&Consumption{EventID: eventID, ConsumerName: consumerName, ProcessedAt: time.Now().UTC()})
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		return false, nil
	}
	if effect == nil {
		return true, nil
	}
	if err := effect(tx); err != nil {
		// The caller's transaction must be rolled back.  Returning the error
		// without deleting the marker is important: deleting it here would
		// make a non-transactional caller observe a false success window.
		return false, err
	}
	return true, nil
}
