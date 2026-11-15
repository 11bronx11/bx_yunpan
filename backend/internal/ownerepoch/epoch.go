// Package ownerepoch combines independent API/background content counters in
// one committed SQL snapshot. Authorization and quota always use business facts.
package ownerepoch

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

var ErrInvalidOwner = errors.New("owner epoch requires a non-zero owner")

type Epoch struct {
	OwnerID                uuid.UUID `gorm:"type:uuid;primaryKey"`
	ContentEpoch, ACLEpoch int64
	UpdatedAt              time.Time
}

func (Epoch) TableName() string { return "owner_content_epochs" }

type (
	writerKey struct{}
	batchKey  struct{}
)

func Background(ctx context.Context) context.Context {
	return context.WithValue(ctx, writerKey{}, true)
}

// Batch tracks successful bumps inside a shared owner transaction. Callers
// discard the context after rollback so no aborted bump is cached.
func Batch(ctx context.Context) context.Context {
	return context.WithValue(ctx, batchKey{}, map[uuid.UUID]bool{})
}

func Ensure(tx *gorm.DB, id uuid.UUID) error {
	if tx == nil || id == uuid.Nil {
		return ErrInvalidOwner
	}
	if err := tx.Exec(`INSERT INTO owner_content_epochs(owner_id,content_epoch,acl_epoch,updated_at) VALUES(?,1,1,clock_timestamp()) ON CONFLICT DO NOTHING`, id).Error; err != nil {
		return err
	}
	return tx.Exec(`INSERT INTO owner_async_epochs(owner_id) VALUES(?) ON CONFLICT DO NOTHING`, id).Error
}

func Current(tx *gorm.DB, id uuid.UUID) (Epoch, error) {
	if tx == nil || id == uuid.Nil {
		return Epoch{}, ErrInvalidOwner
	}
	var e Epoch
	q := tx.Model(&Epoch{}).
		Select(`owner_content_epochs.owner_id,owner_content_epochs.content_epoch+b.content_epoch AS content_epoch,owner_content_epochs.acl_epoch,GREATEST(owner_content_epochs.updated_at,b.updated_at) AS updated_at`).
		Joins(`JOIN owner_async_epochs b USING(owner_id)`).
		Where(`owner_content_epochs.owner_id=?`, id).Take(&e)
	if q.Error != nil {
		return e, q.Error
	}
	if q.RowsAffected != 1 {
		return e, gorm.ErrRecordNotFound
	}
	return e, nil
}

func BumpContent(tx *gorm.DB, id uuid.UUID) (Epoch, error) {
	if tx == nil || id == uuid.Nil {
		return Epoch{}, ErrInvalidOwner
	}
	bumps, _ := tx.Statement.Context.Value(batchKey{}).(map[uuid.UUID]bool)
	if bumps != nil && bumps[id] {
		return Current(tx, id)
	}
	table := "owner_content_epochs"
	if bg, _ := tx.Statement.Context.Value(writerKey{}).(bool); bg {
		table = "owner_async_epochs"
	}
	q := tx.Exec("UPDATE "+table+" SET content_epoch=content_epoch+1,updated_at=clock_timestamp() WHERE owner_id=?", id)
	if q.Error != nil {
		return Epoch{}, q.Error
	}
	if q.RowsAffected != 1 {
		return Epoch{}, gorm.ErrRecordNotFound
	}
	if bumps != nil {
		bumps[id] = true
	}
	return Current(tx, id)
}

func BumpACL(tx *gorm.DB, id uuid.UUID) (Epoch, error) {
	if tx == nil || id == uuid.Nil {
		return Epoch{}, ErrInvalidOwner
	}
	q := tx.Exec("UPDATE owner_content_epochs SET acl_epoch=acl_epoch+1,updated_at=clock_timestamp() WHERE owner_id=?", id)
	if q.Error != nil {
		return Epoch{}, q.Error
	}
	if q.RowsAffected != 1 {
		return Epoch{}, gorm.ErrRecordNotFound
	}
	return Current(tx, id)
}
