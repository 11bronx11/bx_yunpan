package outbox

import (
	"context"
	"fmt"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"time"
)

type aiProfileKey struct{}

func WithAIProfile(ctx context.Context, id, revision string) context.Context {
	return context.WithValue(ctx, aiProfileKey{}, [2]string{id, revision})
}

// ReadyAIIntent appends the first resource intent in the same transaction as
// object readiness. Reusing a content-addressed object does not call a model.
func ReadyAIIntent(ctx context.Context, tx *gorm.DB, id uuid.UUID) error {
	profile, _ := ctx.Value(aiProfileKey{}).([2]string)
	if profile[0] == "" {
		profile = [2]string{"default", "v1"}
	}
	var row struct {
		ContentGeneration, SizeBytes, Version int64
		SHA256, MimeType                      string
	}
	if err := tx.Table("file_objects").Select("content_generation,size_bytes,version,sha256,mime_type").Where("id=? AND status='ready'", id).Take(&row).Error; err != nil {
		return err
	}
	mediaType, err := AIResourceMediaType(tx, id, row.MimeType)
	if err != nil {
		return err
	}
	row.MimeType = mediaType
	requestID := uuid.NewSHA1(id, []byte(fmt.Sprintf("ai.v2.ready:%d", row.ContentGeneration)))
	q := tx.Exec(`INSERT INTO ai_resource_intents(object_id,content_generation,last_request_id,profile_id,profile_revision) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, id, row.ContentGeneration, requestID, profile[0], profile[1])
	if q.Error != nil {
		return q.Error
	}
	if q.RowsAffected == 0 {
		return nil
	}
	in := aiprotocol.Intent{Version: 2, RequestedAt: time.Now().Unix(), RequestID: requestID, ObjectID: id, ContentGeneration: row.ContentGeneration, ContentHash: row.SHA256, Size: row.SizeBytes, MediaType: row.MimeType, LifecycleRevision: row.Version, Seqs: aiprotocol.Seqs{Source: 1, Text: 1, Vector: 1, Summary: 1}, ProfileID: profile[0], ProfileRevision: profile[1], Targets: []string{"source", "text", "vectors", "summary"}}
	return AddTyped(ctx, tx, eventcontract.EnvelopeInput{EventType: aiprotocol.IntentEvent, AggregateID: id, AggregateVersion: row.Version, Producer: "core", Payload: in})
}

// AIResourceMediaType exposes no file name to AI. The core freezes only an
// effective parser media type; later rename/move never invokes a model.
func AIResourceMediaType(tx *gorm.DB, id uuid.UUID, mime string) (string, error) {
	if !aiprotocol.BinaryMediaType(mime) {
		return mime, nil
	}
	var source bool
	if err := tx.Raw("SELECT EXISTS(SELECT 1 FROM file_entries WHERE object_id=? AND deleted_at IS NULL AND lower(name) ~ ?)", id, aiprotocol.SourceNamePattern).Scan(&source).Error; err != nil {
		return "", err
	}
	if source {
		return "text/plain", nil
	}
	return mime, nil
}
func TombstoneAI(ctx context.Context, tx *gorm.DB, id uuid.UUID, generation, revision int64) error {
	return AddTyped(ctx, tx, eventcontract.EnvelopeInput{EventType: aiprotocol.TombstoneEvent, AggregateID: id, AggregateVersion: revision, Producer: "core", Payload: aiprotocol.Tombstone{ObjectID: id, ContentGeneration: generation, LifecycleRevision: revision}})
}
