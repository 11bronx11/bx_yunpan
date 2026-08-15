package ai

import (
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

type Document struct {
	ID              uuid.UUID `gorm:"type:uuid;primaryKey"`
	ObjectID        uuid.UUID `gorm:"type:uuid"`
	Status          string
	Summary         *string
	Tags            pq.StringArray `gorm:"type:text[]"`
	Language        *string
	PipelineVersion string
	ModelVersion    string
	ErrorCode       *string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (Document) TableName() string { return "ai_documents" }

type Task struct {
	ID           uuid.UUID `gorm:"type:uuid;primaryKey"`
	OwnerID      uuid.UUID `gorm:"type:uuid"`
	TaskType     string
	DedupeKey    string
	ResourceType string
	ResourceID   uuid.UUID `gorm:"type:uuid"`
	Status       string
	Progress     int
	Attempt      int
	ErrorCode    *string
	ErrorMessage *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (Task) TableName() string { return "async_tasks" }

type SearchInput struct {
	Query             string
	Mode              string
	FolderID          *uuid.UUID
	IncludeSubfolders bool
	MimeTypes         []string
	Limit             int
}

type Citation struct {
	ID         string    `json:"id"`
	FileID     uuid.UUID `json:"file_id"`
	FileName   string    `json:"file_name"`
	PageNumber *int      `json:"page_number"`
	Section    *string   `json:"section"`
	Excerpt    string    `json:"excerpt"`
}

type SearchHit struct {
	File      map[string]any `json:"file"`
	Score     float64        `json:"score"`
	MatchType string         `json:"match_type"`
	Citations []Citation     `json:"citations"`
}
