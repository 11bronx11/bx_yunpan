package identity

import (
	"time"

	"github.com/google/uuid"
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
