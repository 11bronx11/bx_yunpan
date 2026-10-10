package ai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/minio/minio-go/v7"
	"go.opentelemetry.io/otel/attribute"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	searchindex "github.com/11bronx11/bx_yunpan/backend/internal/search"
)

const (
	pipelineVersion    = "document-index-v2"
	embeddingBatchSize = 10
)

var (
	ErrNotFound    = errors.New("AI resource not found")
	ErrInvalid     = errors.New("invalid AI request")
	ErrUnavailable = errors.New("AI service unavailable")
	ErrQuota       = errors.New("AI provider quota exhausted")
	ErrProcessing  = errors.New("AI document is already processing")
)

type Service struct {
	db             *gorm.DB
	drive          *drive.Service
	objects        *objectstore.Service
	storage        *minio.Client
	provider       Provider
	parser         ParserGateway
	reranker       Reranker
	config         config.AI
	embeddingCache QueryEmbeddingCache
	retrieval      searchindex.RetrievalBackend
	searchConfig   config.Search
	shadowRecorder ShadowRecorder
}

// QueryEmbeddingCache is deliberately narrower than a general cache API. It
// stores only derived query vectors; callers must treat errors as cache misses
// and continue against the provider.
type QueryEmbeddingCache interface {
	Get(context.Context, string) ([]float32, bool, error)
	Set(context.Context, string, []float32) error
}

func NewService(db *gorm.DB, driveService *drive.Service, objects *objectstore.Service, storage *minio.Client, cfg config.AI) *Service {
	provider := NewProvider(cfg)
	return &Service{db: db, drive: driveService, objects: objects, storage: storage, provider: provider, parser: NewBuiltinParser(provider), reranker: NewReranker(cfg), config: cfg}
}

func NewServiceWithCache(db *gorm.DB, driveService *drive.Service, objects *objectstore.Service, storage *minio.Client, cfg config.AI, embeddingCache QueryEmbeddingCache) *Service {
	service := NewService(db, driveService, objects, storage, cfg)
	service.embeddingCache = embeddingCache
	return service
}

// WithRetrieval attaches the optional derived-index read plane. The service
// keeps PostgreSQL as the default and can fall back per request when an
// external backend is unavailable or returns stale candidates.
func (s *Service) WithRetrieval(backend searchindex.RetrievalBackend, cfg config.Search) *Service {
	s.retrieval, s.searchConfig = backend, cfg
	return s
}

func (s *Service) WithShadowRecorder(recorder ShadowRecorder) *Service {
	s.shadowRecorder = recorder
	return s
}

func (s *Service) ProcessObject(ctx context.Context, objectID uuid.UUID) error {
	return s.processObjectWithMetadata(ctx, objectID, false, IndexMetadata{})
}

func (s *Service) processObject(ctx context.Context, objectID uuid.UUID, force bool) error {
	return s.processObjectWithMetadata(ctx, objectID, force, IndexMetadata{})
}

func (s *Service) processObjectWithMetadata(ctx context.Context, objectID uuid.UUID, force bool, metadata IndexMetadata) error {
	// ai.index 是索引链路的业务根 span：它的父节点是 RabbitMQ 消费 span，
	// 后者又续接上传请求的 trace，因此一次上传能一路看到索引完成。
	ctx, span := startSpan(ctx, "ai.index",
		attribute.String("ai.object_id", objectID.String()),
		attribute.Bool("ai.force", force),
	)
	if metadata.RunID != nil {
		span.SetAttributes(attribute.String("ai.run_id", metadata.RunID.String()))
	}
	err := s.runIndex(ctx, objectID, force, metadata)
	endSpan(span, err)
	return err
}

func (s *Service) runIndex(ctx context.Context, objectID uuid.UUID, force bool, metadata IndexMetadata) error {
	object, err := s.objects.Get(objectID)
	if err != nil {
		return ErrNotFound
	}
	document, version, acquired, err := s.startDocumentVersion(objectID, force)
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	if object.SizeBytes > s.config.MaxObjectBytes {
		return s.finishUnsupportedVersion(version, "ai.object_too_large")
	}
	extractionMimeType, err := s.extractionMimeType(objectID, object.MimeType)
	if err != nil {
		return s.finishFailedVersion(document, version, "ai.metadata_read_failed", err)
	}
	stream, err := s.storage.GetObject(ctx, object.Bucket, object.ObjectKey, minio.GetObjectOptions{})
	if err != nil {
		return s.finishFailedVersion(document, version, "ai.storage_read_failed", err)
	}
	defer func() { _ = stream.Close() }()
	data, err := io.ReadAll(io.LimitReader(stream, s.config.MaxObjectBytes+1))
	if err != nil {
		return s.finishFailedVersion(document, version, "ai.storage_read_failed", err)
	}
	parser := s.parser
	if parser == nil {
		parser = NewBuiltinParser(s.provider)
	}
	runID := version.ID
	if metadata.RunID != nil && *metadata.RunID != uuid.Nil {
		runID = *metadata.RunID
	}
	parseResult, err := parser.Parse(ctx, ParseRequest{
		RunID: runID, ObjectID: objectID, DocumentVersionID: version.ID, ContentHash: object.SHA256,
		MediaType: extractionMimeType, ObjectAddress: ObjectReadAddress{MediaType: extractionMimeType, Data: data},
		ParserProfileVersion: metadata.ParserProfileVersion, SizeLimitBytes: s.config.MaxObjectBytes,
	})
	if errors.Is(err, errInvalidInput) {
		return s.finishUnsupportedVersion(version, "ai.invalid_content")
	}
	if IsParseUnsupported(err) || errors.Is(err, errUnsupported) {
		return s.finishUnsupportedVersion(version, "ai.unsupported_type")
	}
	if err != nil {
		return s.finishFailedVersion(document, version, "ai.extraction_failed", err)
	}
	sections := parseResult.Sections
	if err := s.touchDocumentVersion(ctx, version.ID, version.LeaseToken); err != nil {
		return s.finishFailedVersion(document, version, "ai.persistence_failed", err)
	}
	chunks := s.makeChunksTraced(ctx, sections)
	if len(chunks) == 0 {
		return s.finishUnsupportedVersion(version, "ai.empty_content")
	}

	texts := make([]string, len(chunks))
	for index := range chunks {
		texts[index] = chunks[index].Content
	}
	embeddings, err := s.embedTexts(ctx, version.ID, version.LeaseToken, texts)
	if err != nil {
		return s.finishFailedVersion(document, version, "ai.embedding_failed", err)
	}
	insight, err := s.enrich(ctx, strings.Join(texts, "\n"))
	if err != nil {
		return s.finishFailedVersion(document, version, "ai.enrichment_failed", err)
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("document_version_id = ?", version.ID).Delete(&chunkRow{}).Error; err != nil {
			return err
		}
		for index, chunk := range chunks {
			if len(embeddings[index]) != s.config.Dimension {
				return errors.New("invalid embedding dimension")
			}
			if err := tx.Exec(`
				INSERT INTO ai_chunks (id, document_id, document_version_id, ordinal, page_number, section, content, embedding, token_count)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?::vector, ?)`,
				uuid.Must(uuid.NewV7()), document.ID, version.ID, index, chunk.PageNumber, nullableString(chunk.Section), chunk.Content,
				vectorLiteral(embeddings[index]), len([]rune(chunk.Content))/2,
			).Error; err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		versionResult := tx.Model(&DocumentVersion{}).
			Where("id = ? AND status = 'building' AND lease_token = ?", version.ID, version.LeaseToken).
			Updates(map[string]any{"status": "indexed", "summary": insight.Summary, "tags": pq.StringArray(insight.Tags),
				"language": nullableString(insight.Language), "error_code": nil, "last_error": nil,
				"updated_at": now, "indexed_at": now, "lease_token": nil, "lease_until": nil})
		if versionResult.Error != nil {
			return versionResult.Error
		}
		if versionResult.RowsAffected != 1 {
			return ErrProcessing
		}
		if err := tx.Model(&DocumentVersion{}).Where("document_id = ? AND id <> ? AND status = 'indexed' AND retired_at IS NULL", document.ID, version.ID).
			Updates(map[string]any{"retired_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		return tx.Model(&Document{}).Where("id = ?", document.ID).Updates(map[string]any{
			"status": "indexed", "summary": insight.Summary, "tags": pq.StringArray(insight.Tags),
			"language": nullableString(insight.Language), "error_code": nil, "model_version": s.provider.ModelVersion(),
			"active_version_id": version.ID, "updated_at": now,
		}).Error
	})
	if err != nil {
		return s.finishFailedVersion(document, version, "ai.persistence_failed", err)
	}
	return nil
}

func (s *Service) extractionMimeType(objectID uuid.UUID, mimeType string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0]))
	if normalized != "application/octet-stream" && normalized != "binary/octet-stream" {
		return mimeType, nil
	}
	var names []string
	if err := s.db.Table("file_entries").
		Where("object_id = ? AND deleted_at IS NULL", objectID).
		Pluck("name", &names).Error; err != nil {
		return "", err
	}
	for _, name := range names {
		if isSourceTextFileName(name) {
			return "text/plain", nil
		}
	}
	return mimeType, nil
}

func (s *Service) RequestReprocess(ctx context.Context, ownerID, fileID uuid.UUID, idempotencyKey string) (Task, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return Task{}, ErrInvalid
	}
	file, err := s.drive.File(ownerID, fileID)
	if err != nil {
		return Task{}, ErrNotFound
	}
	dedupeKey := ownerID.String() + ":" + fileID.String() + ":" + idempotencyKey
	task := Task{
		ID: uuid.Must(uuid.NewV7()), OwnerID: ownerID, TaskType: "ai.reprocess", DedupeKey: dedupeKey,
		ResourceType: "file", ResourceID: fileID, Status: "pending", Progress: 0, Version: 1,
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "task_type"}, {Name: "dedupe_key"}}, DoNothing: true}).Create(&task)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return tx.Where("task_type = ? AND dedupe_key = ?", task.TaskType, dedupeKey).First(&task).Error
		}
		return outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
			EventType: eventcontract.EventAIReprocessRequestedV1, AggregateID: task.ID, AggregateVersion: task.Version,
			Producer: "core-api", Payload: eventcontract.AIReprocessRequestedV1{TaskID: task.ID, ObjectID: file.ObjectID, ContentHash: file.SHA256},
		})
	})
	return task, err
}

func (s *Service) ProcessTask(ctx context.Context, taskID, objectID uuid.UUID) error {
	return s.processTaskWithMetadata(ctx, taskID, objectID, IndexMetadata{})
}

func (s *Service) processTaskWithMetadata(ctx context.Context, taskID, objectID uuid.UUID, metadata IndexMetadata) error {
	now := time.Now().UTC()
	result := s.db.Model(&Task{}).Where("id = ? AND status IN ?", taskID, []string{"pending", "running", "failed"}).Updates(map[string]any{
		"status": "running", "progress": 10, "attempt": gorm.Expr("attempt + 1"), "version": gorm.Expr("version + 1"), "updated_at": now,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return nil
	}
	if err := s.processObjectWithMetadata(ctx, objectID, true, metadata); err != nil {
		message := limitRunes(err.Error(), 500)
		s.db.Model(&Task{}).Where("id = ?", taskID).Updates(map[string]any{
			"status": "failed", "progress": 100, "version": gorm.Expr("version + 1"), "error_code": "ai.reprocess_failed", "error_message": message, "updated_at": time.Now().UTC(),
		})
		return err
	}
	return s.db.Model(&Task{}).Where("id = ?", taskID).Updates(map[string]any{
		"status": "succeeded", "progress": 100, "version": gorm.Expr("version + 1"), "error_code": nil, "error_message": nil, "updated_at": time.Now().UTC(),
	}).Error
}

func (s *Service) startDocument(objectID uuid.UUID, force bool) (Document, bool, error) {
	now := time.Now().UTC()
	document := Document{
		ID: uuid.Must(uuid.NewV7()), ObjectID: objectID, Status: "processing", Tags: pq.StringArray{},
		PipelineVersion: pipelineVersion, ModelVersion: s.provider.ModelVersion(),
	}
	result := s.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "object_id"}}, DoNothing: true}).Create(&document)
	if result.Error != nil {
		return Document{}, false, result.Error
	}
	if result.RowsAffected == 1 {
		return document, true, nil
	}

	query := s.db.Model(&Document{}).
		Where("object_id = ? AND (status <> ? OR updated_at < ?)", objectID, "processing", now.Add(-s.processingLease()))
	if !force {
		query = query.Where("status <> ? OR pipeline_version <> ? OR model_version <> ?", "indexed", pipelineVersion, s.provider.ModelVersion())
	}
	result = query.Updates(map[string]any{
		"status": "processing", "pipeline_version": pipelineVersion, "model_version": s.provider.ModelVersion(),
		"error_code": nil, "updated_at": now,
	})
	if result.Error != nil {
		return Document{}, false, result.Error
	}
	if result.RowsAffected == 0 {
		document = Document{}
		if err := s.db.Where("object_id = ?", objectID).First(&document).Error; err != nil {
			return Document{}, false, errors.New("load AI document")
		}
		if document.Status == "processing" {
			return document, false, ErrProcessing
		}
		return document, false, nil
	}
	document = Document{}
	if s.db.Where("object_id = ?", objectID).First(&document).Error != nil {
		return Document{}, false, errors.New("load AI document")
	}
	return document, true, nil
}

func (s *Service) processingLease() time.Duration {
	lease := 2 * s.config.RequestTimeout
	if lease < 3*time.Minute {
		return 3 * time.Minute
	}
	return lease
}

// startDocumentVersion acquires a fencing token for one building generation.
// The legacy ai_documents row remains a compatibility summary, while chunks
// are written only under the private version ID. A stale indexer can therefore
// never switch the active pointer after another worker has taken over.
func (s *Service) startDocumentVersion(objectID uuid.UUID, force bool) (Document, DocumentVersion, bool, error) {
	now := time.Now().UTC()
	var document Document
	var version DocumentVersion
	var acquired bool
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("object_id = ?", objectID).First(&document).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			document = Document{ID: uuid.Must(uuid.NewV7()), ObjectID: objectID, Status: "processing", Tags: pq.StringArray{}, PipelineVersion: pipelineVersion, ModelVersion: s.provider.ModelVersion()}
			if err := tx.Create(&document).Error; err != nil {
				return err
			}
		} else if !force && document.ActiveVersionID != nil {
			var active DocumentVersion
			if err := tx.First(&active, "id = ?", *document.ActiveVersionID).Error; err == nil && active.Status == "indexed" && active.PipelineVersion == pipelineVersion && active.ModelVersion == s.provider.ModelVersion() {
				return nil
			}
		}

		var building DocumentVersion
		if err := tx.Where("document_id = ? AND status = 'building'", document.ID).Order("generation DESC").First(&building).Error; err == nil {
			if building.LeaseUntil != nil && building.LeaseUntil.After(now) {
				return ErrProcessing
			}
			// An expired building version is retained as failed history. This
			// avoids deleting a generation that another reader may still inspect.
			_ = tx.Model(&DocumentVersion{}).Where("id = ? AND status = 'building'", building.ID).Updates(map[string]any{
				"status": "failed", "error_code": "ai.lease_expired", "last_error": "indexer lease expired", "updated_at": now,
			}).Error
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var generation int64
		if err := tx.Model(&DocumentVersion{}).Where("document_id = ?", document.ID).Select("COALESCE(MAX(generation), 0)").Scan(&generation).Error; err != nil {
			return err
		}
		leaseToken := uuid.Must(uuid.NewV7())
		version = DocumentVersion{ID: uuid.Must(uuid.NewV7()), DocumentID: document.ID, ObjectID: objectID,
			PipelineVersion: pipelineVersion, ModelVersion: s.provider.ModelVersion(), Generation: generation + 1,
			Status: "building", Tags: pq.StringArray{}, LeaseToken: &leaseToken,
			LeaseUntil: timePtr(now.Add(s.processingLease())), CreatedAt: now, UpdatedAt: now}
		if err := tx.Create(&version).Error; err != nil {
			return err
		}
		if err := tx.Model(&Document{}).Where("id = ?", document.ID).Updates(map[string]any{
			"status": "processing", "pipeline_version": pipelineVersion, "model_version": s.provider.ModelVersion(), "error_code": nil, "updated_at": now,
		}).Error; err != nil {
			return err
		}
		acquired = true
		return nil
	})
	return document, version, acquired, err
}

func timePtr(value time.Time) *time.Time { return &value }

func (s *Service) touchDocument(ctx context.Context, documentID uuid.UUID) error {
	result := s.db.WithContext(ctx).Model(&Document{}).
		Where("id = ? AND status = ?", documentID, "processing").
		Update("updated_at", time.Now().UTC())
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrProcessing
	}
	return nil
}

func (s *Service) touchDocumentVersion(ctx context.Context, versionID uuid.UUID, token *uuid.UUID) error {
	if token == nil {
		return ErrProcessing
	}
	result := s.db.WithContext(ctx).Model(&DocumentVersion{}).
		Where("id = ? AND status = 'building' AND lease_token = ? AND lease_until > ?", versionID, *token, time.Now().UTC()).
		Updates(map[string]any{"updated_at": time.Now().UTC(), "lease_until": time.Now().UTC().Add(s.processingLease())})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrProcessing
	}
	return nil
}

func (s *Service) finishUnsupported(documentID uuid.UUID, code string) error {
	return s.db.Model(&Document{}).Where("id = ?", documentID).Updates(map[string]any{
		"status": "unsupported", "error_code": code, "updated_at": time.Now().UTC(),
	}).Error
}

func (s *Service) finishFailed(documentID uuid.UUID, code string, cause error) error {
	if err := s.db.Model(&Document{}).Where("id = ?", documentID).Updates(map[string]any{
		"status": "failed", "error_code": code, "updated_at": time.Now().UTC(),
	}).Error; err != nil {
		return err
	}
	return fmt.Errorf("%s: %w", code, cause)
}

func (s *Service) finishUnsupportedVersion(version DocumentVersion, code string) error {
	now := time.Now().UTC()
	return s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&DocumentVersion{}).
			Where("id = ? AND status = 'building' AND lease_token = ?", version.ID, version.LeaseToken).
			Updates(map[string]any{
				"status": "unsupported", "error_code": code, "updated_at": now, "lease_token": nil, "lease_until": nil,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrProcessing
		}

		// The legacy summary row is still read by compatibility callers. A
		// first-generation unsupported document must leave processing, while a
		// failed replacement must keep the previously indexed active version.
		var document Document
		if err := tx.Select("id", "active_version_id").First(&document, "id = ?", version.DocumentID).Error; err != nil {
			return err
		}
		if document.ActiveVersionID != nil {
			var active DocumentVersion
			if err := tx.Select("id", "status").First(&active, "id = ?", *document.ActiveVersionID).Error; err != nil {
				return err
			}
			if active.Status == "indexed" {
				return tx.Model(&Document{}).
					Where("id = ? AND active_version_id = ?", document.ID, *document.ActiveVersionID).
					Updates(map[string]any{"status": "indexed", "error_code": nil, "updated_at": now}).Error
			}
		}
		return tx.Model(&Document{}).
			Where("id = ? AND active_version_id IS NULL", document.ID).
			Updates(map[string]any{"status": "unsupported", "error_code": code, "updated_at": now}).Error
	})
}

func (s *Service) finishFailedVersion(document Document, version DocumentVersion, code string, cause error) error {
	if err := s.db.Model(&DocumentVersion{}).Where("id = ? AND status = 'building' AND lease_token = ?", version.ID, version.LeaseToken).Updates(map[string]any{
		"status": "failed", "error_code": code, "last_error": limitRunes(cause.Error(), 500), "updated_at": time.Now().UTC(), "lease_token": nil, "lease_until": nil,
	}).Error; err != nil {
		return err
	}
	// Preserve a previously indexed active version. Only a document without an
	// active pointer needs its legacy summary marked failed.
	if document.ActiveVersionID == nil {
		_ = s.db.Model(&Document{}).Where("id = ? AND active_version_id IS NULL", document.ID).Updates(map[string]any{"status": "failed", "error_code": code, "updated_at": time.Now().UTC()}).Error
	}
	return fmt.Errorf("%s: %w", code, cause)
}

type chunkRow struct{ ID uuid.UUID }

func (chunkRow) TableName() string { return "ai_chunks" }

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func vectorLiteral(vector []float32) string {
	var value strings.Builder
	value.WriteByte('[')
	for index, item := range vector {
		if index > 0 {
			value.WriteByte(',')
		}
		value.WriteString(strconv.FormatFloat(float64(item), 'g', -1, 32))
	}
	value.WriteByte(']')
	return value.String()
}
