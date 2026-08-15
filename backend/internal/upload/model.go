package upload

import (
	"time"

	"github.com/google/uuid"
)

const (
	StatusCreated   = "created"
	StatusUploading = "uploading"
	StatusVerifying = "verifying"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusAborted   = "aborted"
	StatusExpired   = "expired"
)

type Session struct {
	ID                 uuid.UUID `gorm:"type:uuid;primaryKey"`
	UserID             uuid.UUID `gorm:"type:uuid"`
	FolderID           uuid.UUID `gorm:"type:uuid"`
	Filename           string
	FilenameNormalized string
	DeclaredSHA256     string
	MimeType           string
	SizeBytes          int64
	ReservedBytes      int64
	Bucket             string
	ObjectKey          string
	StorageUploadID    string
	PartSize           int64
	PartCount          int
	Status             string
	IdempotencyKey     string
	CompletedObjectID  *uuid.UUID `gorm:"type:uuid"`
	CompletedEntryID   *uuid.UUID `gorm:"type:uuid"`
	ErrorCode          *string
	ExpiresAt          time.Time
	Version            int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
	ConfirmedParts     []Part `gorm:"foreignKey:SessionID;references:ID"`
}

func (Session) TableName() string { return "upload_sessions" }

type Part struct {
	SessionID   uuid.UUID `gorm:"type:uuid;primaryKey"`
	PartNumber  int       `gorm:"primaryKey"`
	ETag        string    `gorm:"column:etag"`
	SizeBytes   int64
	Checksum    *string
	CompletedAt time.Time
}

func (Part) TableName() string { return "upload_parts" }
