// Package aiprotocol contains bounded, versioned values shared across the core,
// control and AI boundaries. It has no database or provider dependency.
package aiprotocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

const IntentEvent = "ai.intent_requested.v2"
const TombstoneEvent = "ai.object_tombstoned.v2"
const JobFinishedEvent = "ai.job_finished.v2"

var ErrInvalid = errors.New("ai.invalid_contract")

type Seqs struct {
	Source  int64 `json:"source"`
	Text    int64 `json:"text"`
	Vector  int64 `json:"vector"`
	Summary int64 `json:"summary"`
}
type Profile struct {
	ID                string `json:"id"`
	Revision          string `json:"revision"`
	Parser            string `json:"parser"`
	Chunker           string `json:"chunker"`
	EmbeddingProvider string `json:"embedding_provider"`
	EmbeddingModel    string `json:"embedding_model"`
	EmbeddingRevision string `json:"embedding_revision"`
	SummaryProvider   string `json:"summary_provider"`
	SummaryModel      string `json:"summary_model"`
	SummaryRevision   string `json:"summary_revision"`
	Prompt            string `json:"prompt"`
	Dimension         int    `json:"dimension"`
	EnableVectors     bool   `json:"enable_vectors"`
	EnableSummary     bool   `json:"enable_summary"`
}

func (p Profile) Digest() string { return HashJSON(p) }

type LegacyBudget struct {
	TaskID                                    uuid.UUID `json:"task_id"`
	Attempts, Dispatches, Recoveries          int
	MaxAttempts, MaxDispatches, MaxRecoveries int
	Deadline                                  time.Time
}

type JobObserver struct {
	JobID   uuid.UUID `json:"job_id"`
	OwnerID uuid.UUID `json:"owner_id"`
}
type Intent struct {
	Observers         []JobObserver `json:"observers,omitempty"`
	Legacy            *LegacyBudget `json:"legacy,omitempty"`
	Version           int           `json:"version"`
	RequestedAt       int64         `json:"requested_at"`
	RequestID         uuid.UUID     `json:"request_id"`
	ObjectID          uuid.UUID     `json:"object_id"`
	ContentGeneration int64         `json:"content_generation"`
	ContentHash       string        `json:"content_hash"`
	Size              int64         `json:"size"`
	MediaType         string        `json:"media_type"`
	LifecycleRevision int64         `json:"lifecycle_revision"`
	Seqs              Seqs          `json:"seqs"`
	ProfileID         string        `json:"profile_id"`
	ProfileRevision   string        `json:"profile_revision"`
	Targets           []string      `json:"targets"`
	OwnerID           uuid.UUID     `json:"owner_id,omitempty"`
	JobID             uuid.UUID     `json:"job_id,omitempty"`
}

func (i Intent) Validate() error {
	if i.Version != 2 || i.RequestID == uuid.Nil || i.ObjectID == uuid.Nil || i.ContentGeneration < 1 || len(i.ContentHash) != 64 || i.Size < 0 || len(i.MediaType) > 255 || i.ProfileID == "" || i.ProfileRevision == "" {
		return ErrInvalid
	}
	if i.Seqs.Source < 0 || i.Seqs.Text < 0 || i.Seqs.Vector < 0 || i.Seqs.Summary < 0 {
		return ErrInvalid
	}
	return nil
}

type Tombstone struct {
	ObjectID          uuid.UUID `json:"object_id"`
	ContentGeneration int64     `json:"content_generation"`
	LifecycleRevision int64     `json:"lifecycle_revision"`
}

func (t Tombstone) Validate() error {
	if t.ObjectID == uuid.Nil || t.ContentGeneration < 1 || t.LifecycleRevision < 1 {
		return ErrInvalid
	}
	return nil
}

type Input struct {
	MaintenanceAt     int64     `json:"maintenance_at,omitempty"`
	Intent            Intent    `json:"intent"`
	Profile           Profile   `json:"profile"`
	ProcessingGrant   string    `json:"processing_grant,omitempty"`
	SourceID          uuid.UUID `json:"source_id,omitempty"`
	ContentVersionID  uuid.UUID `json:"content_version_id,omitempty"`
	EmbeddingSetID    uuid.UUID `json:"embedding_set_id,omitempty"`
	FailureCapability string    `json:"failure_capability,omitempty"`
	FailureSeq        int64     `json:"failure_seq,omitempty"`
	ErrorCode         string    `json:"error_code,omitempty"`
	TargetID          uuid.UUID `json:"target_id,omitempty"`
	TextTargetEpoch   int64     `json:"text_target_epoch,omitempty"`
	VectorTargetEpoch int64     `json:"vector_target_epoch,omitempty"`
	TargetNamespace   string    `json:"target_namespace,omitempty"`
	TargetEpoch       int64     `json:"target_epoch,omitempty"`
	Operation         string    `json:"operation,omitempty"`
}
type Output struct {
	Version           int       `json:"version"`
	TaskID            uuid.UUID `json:"task_id"`
	Generation        int64     `json:"generation"`
	InputDigest       string    `json:"input_digest"`
	ArtifactID        uuid.UUID `json:"artifact_id"`
	ArtifactHash      string    `json:"artifact_hash"`
	OutputID          uuid.UUID `json:"output_id"`
	SourceID          uuid.UUID `json:"source_id,omitempty"`
	ContentVersionID  uuid.UUID `json:"content_version_id,omitempty"`
	EmbeddingSetID    uuid.UUID `json:"embedding_set_id,omitempty"`
	ObjectID          uuid.UUID `json:"object_id"`
	ContentGeneration int64     `json:"content_generation"`
	RequestedSeq      int64     `json:"requested_seq"`
	RowCount          int64     `json:"row_count"`
	ByteCount         int64     `json:"byte_count"`
	Coverage          string    `json:"coverage"`
	Complete          bool      `json:"complete"`
}

func (o Output) Validate() error {
	if o.Version != 2 || o.TaskID == uuid.Nil || o.ArtifactID == uuid.Nil || o.OutputID == uuid.Nil || o.ObjectID == uuid.Nil || o.Generation < 1 || o.ContentGeneration < 1 || len(o.ArtifactHash) != 64 || len(o.InputDigest) != 64 || !o.Complete || o.RowCount < 0 || o.ByteCount < 0 {
		return ErrInvalid
	}
	return nil
}
func Hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func HashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return Hash(b)
}
func Semantic(kind string, in Input) string {
	i := in.Intent
	return fmt.Sprintf("ai.v2.%s:%s:%d:%s", strings.TrimPrefix(kind, "ai."), i.ObjectID, i.ContentGeneration, HashJSON(in))
}
func Targets(requested []string, profile Profile) ([]string, error) {
	if len(requested) == 0 {
		requested = []string{"all"}
	}
	out := []string{}
	for _, v := range requested {
		if v == "all" {
			out = append(out, "source", "text")
			if profile.EnableVectors {
				out = append(out, "vectors")
			}
			if profile.EnableSummary {
				out = append(out, "summary")
			}
			continue
		}
		if !slices.Contains([]string{"source", "text", "vectors", "summary", "projection_text", "projection_vector"}, v) {
			return nil, ErrInvalid
		}
		if (v == "vectors" || v == "projection_vector") && !profile.EnableVectors || v == "summary" && !profile.EnableSummary {
			return nil, errors.New("ai.capability_disabled")
		}
		out = append(out, v)
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) == 0 {
		return nil, ErrInvalid
	}
	return out, nil
}

type Scope struct {
	Mode              string      `json:"mode"`
	FileIDs           []uuid.UUID `json:"file_ids,omitempty"`
	FolderID          *uuid.UUID  `json:"folder_id,omitempty"`
	IncludeSubfolders bool        `json:"include_subfolders,omitempty"`
}

func (s *Scope) Normalize() error {
	if s.Mode == "" {
		s.Mode = "all"
	}
	slices.SortFunc(s.FileIDs, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	s.FileIDs = slices.Compact(s.FileIDs)
	switch s.Mode {
	case "all":
		if len(s.FileIDs) > 0 || s.FolderID != nil {
			return ErrInvalid
		}
	case "selected":
		if len(s.FileIDs) == 0 || len(s.FileIDs) > 50 || s.FolderID != nil {
			return ErrInvalid
		}
	case "folder":
		if s.FolderID == nil || *s.FolderID == uuid.Nil || len(s.FileIDs) > 0 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	for _, id := range s.FileIDs {
		if id == uuid.Nil {
			return ErrInvalid
		}
	}
	return nil
}

type ScopeGrant struct {
	Principal    uuid.UUID `json:"principal"`
	Scope        Scope     `json:"scope"`
	ContentEpoch int64     `json:"content_epoch"`
	ACLEpoch     int64     `json:"acl_epoch"`
	RequestID    string    `json:"request_id"`
	Action       string    `json:"action"`
	ExpiresAt    int64     `json:"expires_at"`
}
type ProcessingGrant struct {
	IntentID          uuid.UUID `json:"intent_id"`
	InputIdentity     string    `json:"input_identity"`
	ObjectID          uuid.UUID `json:"object_id"`
	ContentGeneration int64     `json:"content_generation"`
	ContentHash       string    `json:"content_hash"`
	Deadline          int64     `json:"deadline"`
	Action            string    `json:"action"`
}

func (g ProcessingGrant) Valid(in Input) bool {
	return g.IntentID == in.Intent.RequestID && g.InputIdentity == HashJSON(in.Intent) && g.ObjectID == in.Intent.ObjectID && g.ContentGeneration == in.Intent.ContentGeneration && g.ContentHash == in.Intent.ContentHash && g.Deadline > time.Now().Unix() && g.Action == "object.read"
}
