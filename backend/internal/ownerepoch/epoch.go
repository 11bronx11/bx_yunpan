// Package ownerepoch stores coarse-grained invalidation epochs in PostgreSQL.
// Epochs are acceleration metadata only: permission and quota decisions still
// read their authoritative rows from the business tables.
package ownerepoch

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

var ErrInvalidOwner = errors.New("owner epoch requires a non-zero owner")

type Epoch struct {
	OwnerID      uuid.UUID `gorm:"type:uuid;primaryKey"`
	ContentEpoch int64
	ACLEpoch     int64
	UpdatedAt    time.Time
}

func (Epoch) TableName() string { return "owner_content_epochs" }

// Ensure creates the initial row for a user. It is safe to call from the user
// provisioning transaction and from compatibility paths during rollout.
func Ensure(tx *gorm.DB, ownerID uuid.UUID) error {
	if tx == nil || ownerID == uuid.Nil {
		return ErrInvalidOwner
	}
	return tx.Exec(`
		INSERT INTO owner_content_epochs (owner_id, content_epoch, acl_epoch, updated_at)
		VALUES (?, 1, 1, ?)
		ON CONFLICT (owner_id) DO NOTHING`, ownerID, time.Now().UTC()).Error
}

func Current(tx *gorm.DB, ownerID uuid.UUID) (Epoch, error) {
	if tx == nil || ownerID == uuid.Nil {
		return Epoch{}, ErrInvalidOwner
	}
	if err := Ensure(tx, ownerID); err != nil {
		return Epoch{}, err
	}
	var epoch Epoch
	if err := tx.Where("owner_id = ?", ownerID).First(&epoch).Error; err != nil {
		return Epoch{}, err
	}
	return epoch, nil
}

func BumpContent(tx *gorm.DB, ownerID uuid.UUID) (Epoch, error) {
	if tx == nil || ownerID == uuid.Nil {
		return Epoch{}, ErrInvalidOwner
	}
	if err := Ensure(tx, ownerID); err != nil {
		return Epoch{}, err
	}
	var epoch Epoch
	err := tx.Raw(`
		UPDATE owner_content_epochs
		SET content_epoch = content_epoch + 1, updated_at = ?
		WHERE owner_id = ?
		RETURNING owner_id, content_epoch, acl_epoch, updated_at`, time.Now().UTC(), ownerID).Scan(&epoch).Error
	return epoch, err
}

func BumpACL(tx *gorm.DB, ownerID uuid.UUID) (Epoch, error) {
	if tx == nil || ownerID == uuid.Nil {
		return Epoch{}, ErrInvalidOwner
	}
	if err := Ensure(tx, ownerID); err != nil {
		return Epoch{}, err
	}
	var epoch Epoch
	err := tx.Raw(`
		UPDATE owner_content_epochs
		SET acl_epoch = acl_epoch + 1, updated_at = ?
		WHERE owner_id = ?
		RETURNING owner_id, content_epoch, acl_epoch, updated_at`, time.Now().UTC(), ownerID).Scan(&epoch).Error
	return epoch, err
}
