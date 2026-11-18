package upload

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestBatchConfirmationAndFrozenReplay(t *testing.T) {
	f := newConcurrencyFixture(t)
	session := Session{ID: uuid.New(), UserID: f.userID, FolderID: f.rootID, Filename: "batch.bin", FilenameNormalized: "batch.bin",
		DeclaredSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", MimeType: "application/octet-stream",
		SizeBytes: 9, PartSize: 4, PartCount: 3, Status: StatusCreated, StorageUploadID: "multipart", Bucket: "test", ObjectKey: "staging/batch",
		IdempotencyKey: uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour), Version: 1}
	if err := f.db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	parts := []ConfirmedPart{{PartNumber: 3, ETag: "three", SizeBytes: 1}, {PartNumber: 1, ETag: "\"one\"", SizeBytes: 4}, {PartNumber: 2, ETag: "two", SizeBytes: 4}}
	got, err := f.service.ConfirmPartsContext(context.Background(), f.userID, session.ID, parts, true)
	if err != nil || len(got.ConfirmedParts) != 3 {
		t.Fatalf("confirmation: %+v %v", got, err)
	}
	// Compare persisted timestamps: PG stores microseconds, while the first
	// delta response can still carry the Go timestamp's nanoseconds.
	stored, err := f.service.Get(f.userID, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstTime := stored.ConfirmedParts[0].CompletedAt
	got, err = f.service.ConfirmPartsContext(context.Background(), f.userID, session.ID, parts[:1], true)
	if err != nil || len(got.ConfirmedParts) != 1 || got.ConfirmedParts[0].PartNumber != 3 {
		t.Fatalf("delta: %+v %v", got, err)
	}
	if _, err := f.service.Complete(context.Background(), f.userID, session.ID); err != nil {
		t.Fatal(err)
	}
	got, err = f.service.ConfirmPartsContext(context.Background(), f.userID, session.ID, parts, true)
	if err != nil || got.Status != StatusCompleting || !got.ConfirmedParts[0].CompletedAt.Equal(firstTime) {
		t.Fatalf("frozen replay: %+v %v", got, err)
	}
	parts[0].ETag = "conflict"
	if _, err := f.service.ConfirmPartsContext(context.Background(), f.userID, session.ID, parts, true); !errors.Is(err, ErrPartConflict) {
		t.Fatalf("conflict: %v", err)
	}
}

func TestBatchConfirmationRejectsWholeInvalidBatch(t *testing.T) {
	f := newConcurrencyFixture(t)
	session := Session{ID: uuid.New(), UserID: f.userID, FolderID: f.rootID, Filename: "invalid.bin", FilenameNormalized: "invalid.bin",
		DeclaredSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", MimeType: "application/octet-stream",
		SizeBytes: 5, PartSize: 4, PartCount: 2, Status: StatusCreated, StorageUploadID: "multipart", Bucket: "test", ObjectKey: "staging/invalid",
		IdempotencyKey: uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour), Version: 1}
	if err := f.db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	parts := []ConfirmedPart{{PartNumber: 1, ETag: "one", SizeBytes: 4}, {PartNumber: 2, ETag: "two", SizeBytes: 4}}
	if _, err := f.service.ConfirmPartsContext(context.Background(), f.userID, session.ID, parts, true); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid: %v", err)
	}
	var count int64
	if err := f.db.Model(&Part{}).Where("session_id=?", session.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("partial batch: %d %v", count, err)
	}
	if _, err := f.service.Complete(context.Background(), f.userID, session.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("incomplete: %v", err)
	}
}
