package drive

import (
	"time"

	"github.com/google/uuid"
)

type Folder struct {
	ID             uuid.UUID  `gorm:"type:uuid;primaryKey"`
	OwnerID        uuid.UUID  `gorm:"type:uuid"`
	ParentID       *uuid.UUID `gorm:"type:uuid"`
	Name           string
	NameNormalized string
	DeletedAt      *time.Time
	Version        int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (Folder) TableName() string { return "folders" }

type FileEntry struct {
	ID             uuid.UUID `gorm:"type:uuid;primaryKey"`
	OwnerID        uuid.UUID `gorm:"type:uuid"`
	FolderID       uuid.UUID `gorm:"type:uuid"`
	ObjectID       uuid.UUID `gorm:"type:uuid"`
	Name           string
	NameNormalized string
	DeletedAt      *time.Time
	Version        int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (FileEntry) TableName() string { return "file_entries" }

type FileView struct {
	FileEntry
	SizeBytes int64
	MimeType  string
	SHA256    string
	AIStatus  *string
}
