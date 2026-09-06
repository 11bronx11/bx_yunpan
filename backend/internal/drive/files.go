package drive

import (
	"context"
	"mime"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/ownerepoch"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/storageurl"
)

type LogicalUsage interface {
	RemoveLogicalUsage(*gorm.DB, uuid.UUID, int64) error
}

type FileManager struct {
	drive      *Service
	objects    *objectstore.Service
	quota      LogicalUsage
	presigner  *storageurl.Presigner
	variant    PreviewVariant
	readURLTTL time.Duration
}

type PreviewVariant interface {
	PreviewURL(context.Context, uuid.UUID) (string, time.Time, bool, error)
}

func NewFileManager(driveService *Service, objects *objectstore.Service, quota LogicalUsage, presigner *storageurl.Presigner, variant PreviewVariant, readURLTTL time.Duration) *FileManager {
	return &FileManager{drive: driveService, objects: objects, quota: quota, presigner: presigner, variant: variant, readURLTTL: readURLTTL}
}

func (m *FileManager) Get(ownerID, fileID uuid.UUID) (FileView, error) {
	return m.drive.File(ownerID, fileID)
}

func (m *FileManager) Rename(ownerID, fileID uuid.UUID, version int64, name string) (FileView, error) {
	return m.drive.RenameFile(ownerID, fileID, version, name)
}

func (m *FileManager) RenameWithContext(ctx context.Context, ownerID, fileID uuid.UUID, version int64, name string) (FileView, error) {
	return m.drive.RenameFileWithContext(ctx, ownerID, fileID, version, name)
}

func (m *FileManager) Move(ownerID, fileID, targetFolderID uuid.UUID, version int64) (FileView, error) {
	return m.drive.MoveFile(ownerID, fileID, targetFolderID, version)
}

func (m *FileManager) MoveWithContext(ctx context.Context, ownerID, fileID, targetFolderID uuid.UUID, version int64) (FileView, error) {
	return m.drive.MoveFileWithContext(ctx, ownerID, fileID, targetFolderID, version)
}

// Delete 需要 ctx：事务里写 Outbox 时要把当前 span context 一起注入，
// 让异步 GC 能续接同一条 trace。
func (m *FileManager) Delete(ctx context.Context, ownerID, fileID uuid.UUID, version int64) error {
	return m.drive.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var entry FileEntry
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND owner_id = ? AND deleted_at IS NULL", fileID, ownerID).First(&entry).Error; err != nil {
			return ErrNotFound
		}
		if entry.Version != version {
			return ErrConflict
		}
		object, err := m.objects.Get(entry.ObjectID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := tx.Model(&entry).Updates(map[string]any{"deleted_at": now, "version": gorm.Expr("version + 1"), "updated_at": now}).Error; err != nil {
			return err
		}
		epoch, err := ownerepoch.BumpContent(tx, ownerID)
		if err != nil {
			return err
		}
		remaining, err := m.objects.ReleaseReference(tx, entry.ObjectID)
		if err != nil {
			return err
		}
		if err := m.quota.RemoveLogicalUsage(tx, ownerID, object.SizeBytes); err != nil {
			return err
		}
		if err := outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
			EventType: eventcontract.EventFileDeletedV1, AggregateID: entry.ID, AggregateVersion: entry.Version + 1,
			Producer: "core-api", Payload: eventcontract.FileDeletedV1{OwnerID: ownerID, FileEntryID: entry.ID, ObjectID: entry.ObjectID, TombstoneAt: now, ContentEpoch: epoch.ContentEpoch},
		}); err != nil {
			return err
		}
		if remaining == 0 {
			// ReleaseReference advances the object aggregate version. Read it back
			// so the GC command fences against the committed version.
			if err := tx.First(&object, "id = ?", object.ID).Error; err != nil {
				return err
			}
			return outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
				EventType: eventcontract.EventObjectGCRequestedV1, AggregateID: object.ID, AggregateVersion: object.Version,
				Producer: "core-api", Payload: eventcontract.ObjectGCRequestedV1{ObjectID: object.ID},
			})
		}
		return nil
	})
}

func (m *FileManager) DownloadURL(ctx context.Context, ownerID, fileID uuid.UUID) (string, time.Time, error) {
	file, err := m.drive.File(ownerID, fileID)
	if err != nil {
		return "", time.Time{}, err
	}
	object, err := m.objects.Get(file.ObjectID)
	if err != nil {
		return "", time.Time{}, ErrNotFound
	}
	expiresAt := time.Now().UTC().Add(m.readURLTTL)
	params := url.Values{"response-content-disposition": {mime.FormatMediaType("attachment", map[string]string{"filename": file.Name})}}
	presigned, err := m.presigner.PresignedGetObject(ctx, object.Bucket, object.ObjectKey, m.readURLTTL, params)
	if err != nil {
		return "", time.Time{}, err
	}
	return presigned.String(), expiresAt, nil
}

func (m *FileManager) Preview(ctx context.Context, ownerID, fileID uuid.UUID) (string, string, time.Time, error) {
	file, err := m.drive.File(ownerID, fileID)
	if err != nil {
		return "", "", time.Time{}, err
	}
	kind := previewKind(file.MimeType)
	if kind == "unsupported" {
		return kind, "", time.Time{}, nil
	}
	object, err := m.objects.Get(file.ObjectID)
	if err != nil {
		return "", "", time.Time{}, err
	}
	expiresAt := time.Now().UTC().Add(m.readURLTTL)
	if kind == "image" && m.variant != nil {
		if value, variantExpiry, ok, variantErr := m.variant.PreviewURL(ctx, file.ObjectID); variantErr != nil {
			return "", "", time.Time{}, variantErr
		} else if ok {
			return kind, value, variantExpiry, nil
		}
	}
	contentType := file.MimeType
	if kind == "text" {
		contentType = "text/plain; charset=utf-8"
	}
	params := url.Values{"response-content-type": {contentType}, "response-content-disposition": {"inline"}}
	presigned, err := m.presigner.PresignedGetObject(ctx, object.Bucket, object.ObjectKey, m.readURLTTL, params)
	if err != nil {
		return "", "", time.Time{}, err
	}
	return kind, presigned.String(), expiresAt, nil
}

func previewKind(mimeType string) string {
	switch mimeType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return "image"
	case "application/pdf":
		return "pdf"
	default:
		if strings.HasPrefix(mimeType, "text/") {
			return "text"
		}
		return "unsupported"
	}
}
