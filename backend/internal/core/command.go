package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Job struct {
	ID, OwnerID, ResourceID                   uuid.UUID
	TaskType, DedupeKey, ResourceType, Status string
	Progress, Attempt                         int
	Version                                   int64
	ErrorCode, ErrorMessage                   *string
	CreatedAt, UpdatedAt                      time.Time
}

func (Job) TableName() string { return "async_tasks" }

type JobRequest struct {
	JobID, OwnerID, FileID, ObjectID                                                         uuid.UUID
	ContentGeneration                                                                        int64
	RequestedTargets, ProfileRevisions, RequestedSeqs, CapabilityOutcomes, ResultVersionRefs datatypes.JSON
	RequestDigest                                                                            string
	CreatedAt                                                                                time.Time
	FinishedAt                                                                               *time.Time
}

func (JobRequest) TableName() string { return "ai_job_requests" }
func jsonData(v any) datatypes.JSON  { b, _ := json.Marshal(v); return b }
func RequestReprocess(ctx context.Context, db *gorm.DB, owner, file uuid.UUID, key string, targets []string, profile aiprotocol.Profile) (Job, []string, error) {
	var job Job
	if len(key) < 1 || len(key) > 200 {
		return job, nil, aiprotocol.ErrInvalid
	}
	effective, err := aiprotocol.Targets(targets, profile)
	if err != nil {
		return job, nil, err
	}
	digest := aiprotocol.HashJSON([]any{file, effective, profile.ID, profile.Revision})
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		id := uuid.NewSHA1(owner, []byte("ai.v2.command:"+key))
		job = Job{ID: id, OwnerID: owner, ResourceID: file, TaskType: "ai.reprocess.v2", DedupeKey: owner.String() + ":" + key, ResourceType: "file", Status: "pending", Version: 1}
		var existing JobRequest
		q := tx.Where("job_id=?", id).Find(&existing)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected > 0 {
			if existing.RequestDigest != digest {
				return errors.New("ai.idempotency_conflict")
			}
			return tx.First(&job, "id=? AND owner_id=?", id, owner).Error
		}
		// Serialize concurrent requests with the same key before changing seqs.
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?,0))", id.String()).Error; err != nil {
			return err
		}
		q = tx.Where("job_id=?", id).Find(&existing)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected > 0 {
			if existing.RequestDigest != digest {
				return errors.New("ai.idempotency_conflict")
			}
			return tx.First(&job, "id=?", id).Error
		}
		var o struct {
			ObjectID                              uuid.UUID
			ContentGeneration, SizeBytes, Version int64
			SHA256, MimeType                      string
		}
		if err := tx.Table("file_entries e").Select("o.id AS object_id,o.content_generation,o.size_bytes,o.version,o.sha256,o.mime_type").Joins("JOIN file_objects o ON o.id=e.object_id").Where("e.id=? AND e.owner_id=? AND e.deleted_at IS NULL AND o.status='ready'", file, owner).Take(&o).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return status.Error(codes.NotFound, "file unavailable")
			}
			return err
		}
		mediaType, err := outbox.AIResourceMediaType(tx, o.ObjectID, o.MimeType)
		if err != nil {
			return err
		}
		o.MimeType = mediaType
		if !supportedMIME(o.MimeType) {
			return errors.New("ai.capability_unsupported")
		}
		if err := tx.Create(&job).Error; err != nil {
			return err
		}
		if err := tx.Exec(`INSERT INTO ai_resource_intents(object_id,content_generation,source_seq,text_seq,vector_seq,summary_seq,last_request_id,profile_id,profile_revision) VALUES(?,?,0,0,0,0,?,?,?) ON CONFLICT DO NOTHING`, o.ObjectID, o.ContentGeneration, id, profile.ID, profile.Revision).Error; err != nil {
			return err
		}
		var row struct{ SourceSeq, TextSeq, VectorSeq, SummarySeq int64 }
		if err := tx.Table("ai_resource_intents").Clauses(clause.Locking{Strength: "UPDATE"}).Where("object_id=? AND content_generation=?", o.ObjectID, o.ContentGeneration).Take(&row).Error; err != nil {
			return err
		}
		source := slices.Contains(effective, "source")
		text := source || slices.Contains(effective, "text")
		vectors := text || slices.Contains(effective, "vectors")
		summary := source || slices.Contains(effective, "summary")
		if source {
			row.SourceSeq++
		}
		if text {
			row.TextSeq++
		}
		if vectors {
			row.VectorSeq++
		}
		if summary {
			row.SummarySeq++
		}
		if err := tx.Table("ai_resource_intents").Where("object_id=? AND content_generation=?", o.ObjectID, o.ContentGeneration).Updates(map[string]any{"source_seq": row.SourceSeq, "text_seq": row.TextSeq, "vector_seq": row.VectorSeq, "summary_seq": row.SummarySeq, "last_request_id": id, "profile_id": profile.ID, "profile_revision": profile.Revision, "updated_at": gorm.Expr("clock_timestamp()")}).Error; err != nil {
			return err
		}
		seqs := aiprotocol.Seqs{Source: row.SourceSeq, Text: row.TextSeq, Vector: row.VectorSeq, Summary: row.SummarySeq}
		request := JobRequest{JobID: id, OwnerID: owner, FileID: file, ObjectID: o.ObjectID, ContentGeneration: o.ContentGeneration, RequestedTargets: jsonData(effective), ProfileRevisions: jsonData(profile.Revision), RequestedSeqs: jsonData(seqs), RequestDigest: digest, CapabilityOutcomes: jsonData(map[string]any{}), ResultVersionRefs: jsonData(map[string]any{})}
		if err := tx.Create(&request).Error; err != nil {
			return err
		}
		intent := aiprotocol.Intent{Version: 2, RequestedAt: time.Now().Unix(), RequestID: id, ObjectID: o.ObjectID, ContentGeneration: o.ContentGeneration, ContentHash: o.SHA256, Size: o.SizeBytes, MediaType: o.MimeType, LifecycleRevision: o.Version, Seqs: seqs, ProfileID: profile.ID, ProfileRevision: profile.Revision, Targets: effective, OwnerID: owner, JobID: id}
		return outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{EventType: aiprotocol.IntentEvent, AggregateID: o.ObjectID, AggregateVersion: o.Version, Producer: "core-api", Payload: intent})
	})
	return job, effective, err
}

func supportedMIME(m string) bool {
	return aiprotocol.SupportedSource(m)
}
