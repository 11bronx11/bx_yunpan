package ai

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func versionDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestStartDocumentVersionRetainsFailedGenerationAndFencesLease(t *testing.T) {
	db := versionDB(t)
	objectID := uuid.New()
	digest := strings.Repeat("c", 64)
	if err := db.Exec(`INSERT INTO file_objects (id, sha256, size_bytes, mime_type, bucket, object_key, status, reference_count, version) VALUES (?, ?, 1, 'text/plain', 'test', ?, 'ready', 0, 1)`, objectID, digest, "version/"+objectID.String()).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM ai_documents WHERE object_id = ?", objectID).Error
		_ = db.Exec("DELETE FROM file_objects WHERE id = ?", objectID).Error
	})
	service := &Service{db: db, provider: &fakeProvider{dimension: 3}, config: config.AI{Dimension: 3, RequestTimeout: time.Second}}
	document, first, acquired, err := service.startDocumentVersion(objectID, false)
	if err != nil || !acquired || first.Generation != 1 {
		t.Fatalf("first generation = document:%#v version:%#v acquired:%v err:%v", document, first, acquired, err)
	}
	if _, _, _, err := service.startDocumentVersion(objectID, false); !errors.Is(err, ErrProcessing) {
		t.Fatalf("active lease error = %v", err)
	}
	if err := db.Model(&DocumentVersion{}).Where("id = ?", first.ID).Update("lease_until", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	_, second, acquired, err := service.startDocumentVersion(objectID, true)
	if err != nil || !acquired || second.Generation != 2 || second.ID == first.ID {
		t.Fatalf("replacement generation = %#v acquired:%v err:%v", second, acquired, err)
	}
	var failed DocumentVersion
	if err := db.First(&failed, "id = ?", first.ID).Error; err != nil {
		t.Fatal(err)
	}
	if failed.Status != "failed" || failed.ErrorCode == nil || *failed.ErrorCode != "ai.lease_expired" {
		t.Fatalf("expired generation = %#v", failed)
	}
	// A stale token cannot mark the replacement generation indexed.
	if err := db.Model(&DocumentVersion{}).Where("id = ? AND lease_token = ?", second.ID, first.LeaseToken).Update("status", "indexed").Error; err != nil {
		t.Fatal(err)
	}
	var current DocumentVersion
	if err := db.First(&current, "id = ?", second.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.Status != "building" {
		t.Fatalf("stale token changed replacement status: %#v", current)
	}
}

func TestVersionBackfillHasActivePointersForIndexedDocuments(t *testing.T) {
	db := versionDB(t)
	var missing int64
	if err := db.Table("ai_documents").Where("status = 'indexed' AND active_version_id IS NULL").Count(&missing).Error; err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("indexed documents without active pointer = %d", missing)
	}
	var orphaned int64
	if err := db.Table("ai_chunks").Where("document_version_id IS NOT NULL").Joins("LEFT JOIN ai_document_versions v ON v.id = ai_chunks.document_version_id").Where("v.id IS NULL").Count(&orphaned).Error; err != nil {
		t.Fatal(err)
	}
	if orphaned != 0 {
		t.Fatalf("orphaned version chunks = %d", orphaned)
	}
}

func TestEmbedTextsRenewsVersionLeaseWithFence(t *testing.T) {
	db := versionDB(t)
	objectID := uuid.New()
	documentID := uuid.New()
	versionID := uuid.New()
	leaseToken := uuid.New()
	digest := strings.Repeat("e", 64)
	initialLease := time.Now().UTC().Add(2 * time.Second)
	if err := db.Exec(`INSERT INTO file_objects (id, sha256, size_bytes, mime_type, bucket, object_key, status, reference_count, version)
		VALUES (?, ?, 1, 'text/plain', 'test', ?, 'ready', 0, 1)`, objectID, digest, "embed-lease/"+objectID.String()).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO ai_documents (id, object_id, status, pipeline_version, model_version)
		VALUES (?, ?, 'processing', ?, ?)`, documentID, objectID, pipelineVersion, "local-deterministic-v1").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO ai_document_versions
		(id, document_id, object_id, pipeline_version, model_version, generation, status, lease_token, lease_until)
		VALUES (?, ?, ?, ?, ?, 1, 'building', ?, ?)`, versionID, documentID, objectID, pipelineVersion, "local-deterministic-v1", leaseToken, initialLease).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM ai_documents WHERE id = ?", documentID).Error
		_ = db.Exec("DELETE FROM file_objects WHERE id = ?", objectID).Error
	})

	service := &Service{db: db, provider: &fakeProvider{dimension: 3}, config: config.AI{Dimension: 3, RequestTimeout: time.Second}}
	embeddings, err := service.embedTexts(context.Background(), versionID, &leaseToken, []string{"lease renewal probe"})
	if err != nil {
		t.Fatalf("embedding failed: %v", err)
	}
	if len(embeddings) != 1 || len(embeddings[0]) != 3 {
		t.Fatalf("unexpected embeddings: %#v", embeddings)
	}
	var renewed DocumentVersion
	if err := db.First(&renewed, "id = ?", versionID).Error; err != nil {
		t.Fatal(err)
	}
	if renewed.LeaseUntil == nil || !renewed.LeaseUntil.After(initialLease) {
		t.Fatalf("lease was not renewed: initial=%s renewed=%v", initialLease, renewed.LeaseUntil)
	}
}

func TestFinishUnsupportedVersionUpdatesLegacySummaryWithoutHidingActive(t *testing.T) {
	db := versionDB(t)
	service := &Service{db: db, provider: &fakeProvider{dimension: 3}, config: config.AI{Dimension: 3, RequestTimeout: time.Second}}

	newObject := func(t *testing.T) uuid.UUID {
		t.Helper()
		objectID := uuid.New()
		digest := strings.Repeat(objectID.String(), 4)[:64]
		if err := db.Exec(`INSERT INTO file_objects (id, sha256, size_bytes, mime_type, bucket, object_key, status, reference_count, version) VALUES (?, ?, 1, 'application/octet-stream', 'test', ?, 'ready', 0, 1)`, objectID, digest, "unsupported/"+objectID.String()).Error; err != nil {
			t.Fatal(err)
		}
		return objectID
	}

	firstObject := newObject(t)
	var secondObject uuid.UUID
	t.Cleanup(func() { _ = db.Exec("DELETE FROM file_objects WHERE id IN (?, ?)", firstObject, secondObject).Error })
	document, version, acquired, err := service.startDocumentVersion(firstObject, false)
	if err != nil || !acquired {
		t.Fatalf("start first version: acquired=%v err=%v", acquired, err)
	}
	if err := service.finishUnsupportedVersion(version, "ai.unsupported_type"); err != nil {
		t.Fatal(err)
	}
	var summary Document
	if err := db.First(&summary, "id = ?", document.ID).Error; err != nil {
		t.Fatal(err)
	}
	if summary.Status != "unsupported" || summary.ErrorCode == nil || *summary.ErrorCode != "ai.unsupported_type" {
		t.Fatalf("first unsupported summary = %#v", summary)
	}

	secondObject = newObject(t)
	activeDocument, activeVersion, acquired, err := service.startDocumentVersion(secondObject, false)
	if err != nil || !acquired {
		t.Fatalf("start active baseline: acquired=%v err=%v", acquired, err)
	}
	if err := db.Model(&DocumentVersion{}).Where("id = ?", activeVersion.ID).Updates(map[string]any{"status": "indexed", "indexed_at": time.Now().UTC(), "lease_token": nil, "lease_until": nil}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&Document{}).Where("id = ?", activeDocument.ID).Updates(map[string]any{"status": "indexed", "active_version_id": activeVersion.ID}).Error; err != nil {
		t.Fatal(err)
	}
	_, replacement, acquired, err := service.startDocumentVersion(secondObject, true)
	if err != nil || !acquired {
		t.Fatalf("start replacement: acquired=%v err=%v", acquired, err)
	}
	if err := service.finishUnsupportedVersion(replacement, "ai.unsupported_type"); err != nil {
		t.Fatal(err)
	}
	summary = Document{}
	if err := db.First(&summary, "id = ?", activeDocument.ID).Error; err != nil {
		t.Fatal(err)
	}
	if summary.Status != "indexed" || summary.ActiveVersionID == nil || *summary.ActiveVersionID != activeVersion.ID || summary.ErrorCode != nil {
		t.Fatalf("active summary was hidden by unsupported replacement = %#v", summary)
	}
	var unsupported DocumentVersion
	if err := db.First(&unsupported, "id = ?", replacement.ID).Error; err != nil {
		t.Fatal(err)
	}
	if unsupported.Status != "unsupported" {
		t.Fatalf("replacement version = %#v", unsupported)
	}
}
