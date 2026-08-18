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
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
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
	db       *gorm.DB
	drive    *drive.Service
	objects  *objectstore.Service
	storage  *minio.Client
	provider Provider
	config   config.AI
}

func NewService(db *gorm.DB, driveService *drive.Service, objects *objectstore.Service, storage *minio.Client, cfg config.AI) *Service {
	return &Service{db: db, drive: driveService, objects: objects, storage: storage, provider: NewProvider(cfg), config: cfg}
}

func (s *Service) ProcessObject(ctx context.Context, objectID uuid.UUID) error {
	return s.processObject(ctx, objectID, false)
}

func (s *Service) processObject(ctx context.Context, objectID uuid.UUID, force bool) error {
	object, err := s.objects.Get(objectID)
	if err != nil {
		return ErrNotFound
	}
	document, acquired, err := s.startDocument(objectID, force)
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	if object.SizeBytes > s.config.MaxObjectBytes {
		return s.finishUnsupported(document.ID, "ai.object_too_large")
	}
	extractionMimeType, err := s.extractionMimeType(objectID, object.MimeType)
	if err != nil {
		return s.finishFailed(document.ID, "ai.metadata_read_failed", err)
	}
	stream, err := s.storage.GetObject(ctx, object.Bucket, object.ObjectKey, minio.GetObjectOptions{})
	if err != nil {
		return s.finishFailed(document.ID, "ai.storage_read_failed", err)
	}
	defer func() { _ = stream.Close() }()
	data, err := io.ReadAll(io.LimitReader(stream, s.config.MaxObjectBytes+1))
	if err != nil {
		return s.finishFailed(document.ID, "ai.storage_read_failed", err)
	}
	sections, err := extract(ctx, s.provider, extractionMimeType, data)
	if errors.Is(err, errUnsupported) {
		return s.finishUnsupported(document.ID, "ai.unsupported_type")
	}
	if errors.Is(err, errInvalidInput) {
		return s.finishUnsupported(document.ID, "ai.invalid_content")
	}
	if err != nil {
		return s.finishFailed(document.ID, "ai.extraction_failed", err)
	}
	if err := s.touchDocument(ctx, document.ID); err != nil {
		return s.finishFailed(document.ID, "ai.persistence_failed", err)
	}
	chunks := makeChunks(sections)
	if len(chunks) == 0 {
		return s.finishUnsupported(document.ID, "ai.empty_content")
	}

	texts := make([]string, len(chunks))
	for index := range chunks {
		texts[index] = chunks[index].Content
	}
	embeddings := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += embeddingBatchSize {
		end := start + embeddingBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch, err := s.provider.Embeddings(ctx, texts[start:end])
		if err != nil {
			return s.finishFailed(document.ID, "ai.embedding_failed", err)
		}
		embeddings = append(embeddings, batch...)
		if err := s.touchDocument(ctx, document.ID); err != nil {
			return s.finishFailed(document.ID, "ai.persistence_failed", err)
		}
	}
	insight, err := s.provider.Enrich(ctx, strings.Join(texts, "\n"))
	if err != nil {
		return s.finishFailed(document.ID, "ai.enrichment_failed", err)
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("document_id = ?", document.ID).Delete(&chunkRow{}).Error; err != nil {
			return err
		}
		for index, chunk := range chunks {
			if len(embeddings[index]) != s.config.Dimension {
				return errors.New("invalid embedding dimension")
			}
			if err := tx.Exec(`
				INSERT INTO ai_chunks (id, document_id, ordinal, page_number, section, content, embedding, token_count)
				VALUES (?, ?, ?, ?, ?, ?, ?::vector, ?)`,
				uuid.Must(uuid.NewV7()), document.ID, index, chunk.PageNumber, nullableString(chunk.Section), chunk.Content,
				vectorLiteral(embeddings[index]), len([]rune(chunk.Content))/2,
			).Error; err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		return tx.Model(&Document{}).Where("id = ?", document.ID).Updates(map[string]any{
			"status": "indexed", "summary": insight.Summary, "tags": pq.StringArray(insight.Tags),
			"language": nullableString(insight.Language), "error_code": nil, "model_version": s.provider.ModelVersion(), "updated_at": now,
		}).Error
	})
	if err != nil {
		return s.finishFailed(document.ID, "ai.persistence_failed", err)
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
		ResourceType: "file", ResourceID: fileID, Status: "pending", Progress: 0,
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "task_type"}, {Name: "dedupe_key"}}, DoNothing: true}).Create(&task)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return tx.Where("task_type = ? AND dedupe_key = ?", task.TaskType, dedupeKey).First(&task).Error
		}
		return outbox.Add(tx, "ai_task", task.ID, outbox.EventAIReprocessRequested, map[string]any{
			"task_id": task.ID, "object_id": file.ObjectID,
		})
	})
	return task, err
}

func (s *Service) ProcessTask(ctx context.Context, taskID, objectID uuid.UUID) error {
	now := time.Now().UTC()
	result := s.db.Model(&Task{}).Where("id = ? AND status IN ?", taskID, []string{"pending", "running", "failed"}).Updates(map[string]any{
		"status": "running", "progress": 10, "attempt": gorm.Expr("attempt + 1"), "updated_at": now,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return nil
	}
	if err := s.processObject(ctx, objectID, true); err != nil {
		message := limitRunes(err.Error(), 500)
		s.db.Model(&Task{}).Where("id = ?", taskID).Updates(map[string]any{
			"status": "failed", "progress": 100, "error_code": "ai.reprocess_failed", "error_message": message, "updated_at": time.Now().UTC(),
		})
		return err
	}
	return s.db.Model(&Task{}).Where("id = ?", taskID).Updates(map[string]any{
		"status": "succeeded", "progress": 100, "error_code": nil, "error_message": nil, "updated_at": time.Now().UTC(),
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
