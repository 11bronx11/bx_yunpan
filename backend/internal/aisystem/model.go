package aisystem

import (
	"encoding/json"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/google/uuid"
	"gorm.io/datatypes"
)

type Object struct {
	ObjectID                                                                      uuid.UUID
	ContentGeneration                                                             int64
	ContentHash                                                                   string
	Size                                                                          int64
	MediaType                                                                     string
	LifecycleRevision                                                             int64
	TombstonedAt                                                                  *time.Time
	DesiredSourceSeq, DesiredTextSeq, DesiredVectorSeq, DesiredSummarySeq         int64
	ProfileID, ProfileRevision                                                    string
	ActiveSourceID, ActiveContentVersionID, ActiveEmbeddingSetID, ActiveSummaryID *uuid.UUID
}

func (Object) TableName() string { return "ai_objects" }

type Artifact struct {
	ObjectID                        uuid.UUID
	ContentGeneration               int64
	ArtifactID, TaskID              uuid.UUID
	Generation                      int64
	Kind, InputDigest, ArtifactHash string
	TypedOutputRef                  uuid.UUID
	RowCount, ByteCount             int64
	Complete                        bool
	Data                            datatypes.JSON
	CreatedAt                       time.Time
}

func (Artifact) TableName() string { return "ai_artifact_manifests" }

type Receipt struct {
	ResultID, TaskID                                             uuid.UUID
	Generation                                                   int64
	InputDigest, ResultDigest, ArtifactHash, EffectKind, Outcome string
	PublishedRefs                                                datatypes.JSON
	Revision                                                     int64
	CommittedAt                                                  time.Time
}

func (Receipt) TableName() string { return "ai_commit_receipts" }

type sourceData struct {
	Parsed   ai.ParseResult
	Coverage string
}
type textData struct{ Chunks rag.ParentChild }
type vectorItem struct {
	ChildID  uuid.UUID
	Vector   []float32
	TextHash string
}
type vectorData struct{ Items []vectorItem }
type summaryData struct {
	Insight                       ai.Insight
	Coverage                      string
	SourceBlocks, ProcessedBlocks int
	Parts                         []string
}

func decode[T any](b []byte) (T, error) { var v T; err := json.Unmarshal(b, &v); return v, err }
