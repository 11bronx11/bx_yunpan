package ai

import (
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"gorm.io/gorm"
)

type Document struct {
	ID              uuid.UUID  `gorm:"type:uuid;primaryKey"`
	ObjectID        uuid.UUID  `gorm:"type:uuid"`
	ActiveVersionID *uuid.UUID `gorm:"type:uuid"`
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

type DocumentVersion struct {
	ID                 uuid.UUID `gorm:"type:uuid;primaryKey"`
	DocumentID         uuid.UUID `gorm:"type:uuid"`
	ObjectID           uuid.UUID `gorm:"type:uuid"`
	PipelineVersion    string
	ModelVersion       string
	ParserID           string
	ParserVersion      string
	VisionModelVersion string
	ChunkerVersion     string
	TokenizerID        string
	ParserWarnings     pq.StringArray `gorm:"type:text[]"`
	Generation         int64
	Status             string
	Summary            *string
	Tags               pq.StringArray `gorm:"type:text[]"`
	Language           *string
	ErrorCode          *string
	LeaseToken         *uuid.UUID `gorm:"type:uuid"`
	LeaseUntil         *time.Time
	LastError          *string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	IndexedAt          *time.Time
	RetiredAt          *time.Time
}

func (DocumentVersion) TableName() string { return "ai_document_versions" }

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
	Version      int64
	ErrorCode    *string
	ErrorMessage *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (Task) TableName() string { return "async_tasks" }

func (t *Task) BeforeCreate(_ *gorm.DB) error {
	if t.Version == 0 {
		t.Version = 1
	}
	return nil
}

type SearchInput struct {
	Query             string
	Mode              string
	FolderID          *uuid.UUID
	IncludeSubfolders bool
	MimeTypes         []string
	// FileIDs is an internal Ask scope. Empty means all files authorized by owner/folder.
	FileIDs []uuid.UUID
	Limit   int
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
	// Candidates stays inside aisvc. It carries full chunks for Ask/reranking;
	// gRPC/HTTP conversion only exposes the public citation excerpt.
	candidates []searchCandidate
}
