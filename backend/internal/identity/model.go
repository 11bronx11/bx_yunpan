package identity

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	UserStatusActive   int16 = 1
	UserStatusDisabled int16 = 2
)

type User struct {
	ID                 uuid.UUID `gorm:"type:uuid;primaryKey"`
	Username           string
	UsernameNormalized string
	EmailNormalized    *string
	PasswordHash       string
	Status             int16
	QuotaBytes         int64
	UsedLogicalBytes   int64
	ReservedBytes      int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (User) TableName() string { return "users" }

type RefreshSession struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey"`
	UserID    uuid.UUID `gorm:"type:uuid"`
	TokenHash []byte
	FamilyID  uuid.UUID `gorm:"type:uuid"`
	ExpiresAt time.Time
	RevokedAt *time.Time
	CreatedAt time.Time
}

func (RefreshSession) TableName() string { return "refresh_sessions" }

// Provision acceleration and quota rows in the user's creation transaction.
func (u *User) AfterCreate(tx *gorm.DB) error {
	if err := tx.Exec("INSERT INTO user_quota_usage(owner_id,occupied_bytes) VALUES(?,?)", u.ID, u.UsedLogicalBytes+u.ReservedBytes).Error; err != nil {
		return err
	}
	return tx.Exec("INSERT INTO owner_async_epochs(owner_id) VALUES(?)", u.ID).Error
}
