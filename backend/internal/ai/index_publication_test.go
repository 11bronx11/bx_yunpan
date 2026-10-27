package ai

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"gorm.io/gorm"
)

func publicationFixture(t *testing.T) (*gorm.DB, *Service, uuid.UUID) {
	t.Helper()
	db := versionDB(t)
	objectID := uuid.New()
	text := "Search projection must follow active AI publication " + objectID.String()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", fmt.Sprint(len(text)))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		_, _ = w.Write([]byte(text))
	}))
	t.Cleanup(server.Close)
	storage, err := minio.New(strings.TrimPrefix(server.URL, "http://"), &minio.Options{Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
	if err := db.Exec(`INSERT INTO file_objects (id, sha256, size_bytes, mime_type, bucket, object_key, status, reference_count, version)
		VALUES (?, ?, ?, 'text/plain', 'test', ?, 'ready', 0, 1)`, objectID, digest, len(text), objectID.String()).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM outbox_events WHERE aggregate_id = ?", objectID).Error
		_ = db.Exec("DELETE FROM ai_documents WHERE object_id = ?", objectID).Error
		_ = db.Exec("DELETE FROM file_objects WHERE id = ?", objectID).Error
	})
	service := NewService(db, nil, objectstore.NewService(db), storage, config.AI{
		Provider: "fake", Dimension: 1024, MaxObjectBytes: 1 << 20, RequestTimeout: time.Second,
	})
	return db, service, objectID
}

func TestIndexPublicationEmitsDurableSearchNotification(t *testing.T) {
	db, service, objectID := publicationFixture(t)
	ctx := outbox.WithSearchEnabled(context.Background(), true)
	for _, force := range []bool{false, false, true} {
		if err := service.processObject(ctx, objectID, force); err != nil {
			t.Fatal(err)
		}
	}
	var document Document
	if err := db.Where("object_id = ?", objectID).First(&document).Error; err != nil {
		t.Fatal(err)
	}
	var events []outbox.Event
	if err := db.Where("aggregate_id = ? AND event_type = ?", objectID, eventcontract.EventAIIndexedV1).Order("created_at").Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("initial indexing and reprocess must emit once each; got %d", len(events))
	}
	var payload eventcontract.AIIndexedV1
	if err := json.Unmarshal(events[1].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ObjectID != objectID || document.ActiveVersionID == nil || payload.DocumentVersionID != *document.ActiveVersionID {
		t.Fatalf("notification does not identify active document version: %#v", payload)
	}
	var deliveries []outbox.Delivery
	if err := db.Where("event_id = ?", events[1].ID).Find(&deliveries).Error; err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 || deliveries[0].Destination != outbox.TaskSearchProjectV1+"@search" {
		t.Fatalf("completion must only project search: %#v", deliveries)
	}
}

func TestIndexPublicationRollsBackWhenSearchNotificationFails(t *testing.T) {
	db, service, objectID := publicationFixture(t)
	const callback = "test:fail_search_notification"
	if err := db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "outbox_events" {
			tx.AddError(fmt.Errorf("injected outbox failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Create().Remove(callback) })
	if err := service.ProcessObject(outbox.WithSearchEnabled(context.Background(), true), objectID); err == nil {
		t.Fatal("publication unexpectedly succeeded")
	}
	var document Document
	if err := db.Where("object_id = ?", objectID).First(&document).Error; err != nil {
		t.Fatal(err)
	}
	var chunks int64
	if err := db.Table("ai_chunks").Where("document_id = ?", document.ID).Count(&chunks).Error; err != nil {
		t.Fatal(err)
	}
	if document.ActiveVersionID != nil || chunks != 0 || document.Status == "indexed" {
		t.Fatalf("notification failure committed publication: active=%v chunks=%d status=%s", document.ActiveVersionID, chunks, document.Status)
	}
}
