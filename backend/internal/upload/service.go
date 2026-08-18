package upload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dblock"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/storageurl"
)

var (
	ErrNotFound      = errors.New("upload not found")
	ErrConflict      = errors.New("upload conflict")
	ErrFileExists    = errors.New("file already exists")
	ErrNameConflict  = errors.New("file name already exists in folder")
	ErrInvalidInput  = errors.New("invalid upload input")
	ErrQuotaExceeded = errors.New("quota exceeded")
)

type Quota interface {
	ReserveQuota(*gorm.DB, uuid.UUID, int64) error
	ReleaseQuota(*gorm.DB, uuid.UUID, int64) error
	CommitQuota(*gorm.DB, uuid.UUID, int64, int64) error
	AddLogicalUsage(*gorm.DB, uuid.UUID, int64) error
}

type multipartStorage interface {
	NewMultipartUpload(context.Context, string, string, minio.PutObjectOptions) (string, error)
	CompleteMultipartUpload(context.Context, string, string, string, []minio.CompletePart, minio.PutObjectOptions) (minio.UploadInfo, error)
	AbortMultipartUpload(context.Context, string, string, string) error
}

type uploadStorage interface {
	GetObject(context.Context, string, string, minio.GetObjectOptions) (io.ReadCloser, error)
	StatObject(context.Context, string, string, minio.StatObjectOptions) (minio.ObjectInfo, error)
	CopyObject(context.Context, minio.CopyDestOptions, minio.CopySrcOptions) (minio.UploadInfo, error)
	RemoveObject(context.Context, string, string, minio.RemoveObjectOptions) error
}

type minioUploadStorage struct {
	client *minio.Client
}

func (s minioUploadStorage) GetObject(ctx context.Context, bucket, object string, options minio.GetObjectOptions) (io.ReadCloser, error) {
	return s.client.GetObject(ctx, bucket, object, options)
}

func (s minioUploadStorage) StatObject(ctx context.Context, bucket, object string, options minio.StatObjectOptions) (minio.ObjectInfo, error) {
	return s.client.StatObject(ctx, bucket, object, options)
}

func (s minioUploadStorage) CopyObject(ctx context.Context, destination minio.CopyDestOptions, source minio.CopySrcOptions) (minio.UploadInfo, error) {
	return s.client.CopyObject(ctx, destination, source)
}

func (s minioUploadStorage) RemoveObject(ctx context.Context, bucket, object string, options minio.RemoveObjectOptions) error {
	return s.client.RemoveObject(ctx, bucket, object, options)
}

type Service struct {
	db        *gorm.DB
	storage   uploadStorage
	presigner *storageurl.Presigner
	core      multipartStorage
	bucket    string
	drive     *drive.Service
	objects   *objectstore.Service
	quota     Quota
	config    config.Upload
}

type CreateInput struct {
	FolderID       uuid.UUID
	Filename       string
	SHA256         string
	SizeBytes      int64
	MimeType       string
	IdempotencyKey string
}

type CreateResult struct {
	Mode   string
	Upload *Session
	File   *drive.FileView
}

type ConfirmedPart struct {
	PartNumber int
	ETag       string
	SizeBytes  int64
	Checksum   *string
}

func NewService(db *gorm.DB, storage *minio.Client, presigner *storageurl.Presigner, bucket string, driveService *drive.Service, objects *objectstore.Service, quota Quota, cfg config.Upload) *Service {
	return &Service{db: db, storage: minioUploadStorage{client: storage}, presigner: presigner, core: minio.Core{Client: storage}, bucket: bucket, drive: driveService, objects: objects, quota: quota, config: cfg}
}

func (s *Service) Create(ctx context.Context, userID uuid.UUID, input CreateInput) (CreateResult, error) {
	filename, normalized, err := normalizeFilename(input.Filename)
	if err != nil || !validSHA256(input.SHA256) || input.SizeBytes <= 0 || len(input.IdempotencyKey) == 0 || len(input.IdempotencyKey) > 128 || len(input.MimeType) > 255 {
		return CreateResult{}, ErrInvalidInput
	}
	if input.MimeType == "" {
		input.MimeType = "application/octet-stream"
	}
	if _, err := s.drive.Folder(userID, input.FolderID); err != nil {
		return CreateResult{}, ErrNotFound
	}

	var mode string
	var resultID uuid.UUID
	var started *Session
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		idempotencyLock := "upload:idempotency:" + userID.String() + ":" + input.IdempotencyKey
		if err := dblock.Transaction(tx, idempotencyLock); err != nil {
			return err
		}
		var existing Session
		err := tx.Where("user_id = ? AND idempotency_key = ?", userID, input.IdempotencyKey).Take(&existing).Error
		if err == nil {
			if existing.StorageUploadID == "" && existing.Status == StatusCompleted && existing.CompletedEntryID != nil {
				mode = "instant"
				resultID = *existing.CompletedEntryID
			} else {
				mode = "multipart"
				resultID = existing.ID
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		filenameLock := "upload:filename:" + userID.String() + ":" + input.FolderID.String() + ":" + normalized
		if err := dblock.Transaction(tx, filenameLock); err != nil {
			return err
		}
		var fileCount int64
		if err := tx.Model(&drive.FileEntry{}).
			Where("owner_id = ? AND folder_id = ? AND name_normalized = ? AND deleted_at IS NULL", userID, input.FolderID, normalized).
			Count(&fileCount).Error; err != nil {
			return err
		}
		if fileCount > 0 {
			return ErrNameConflict
		}
		var activeWithSameName int64
		if err := tx.Model(&Session{}).
			Where("user_id = ? AND folder_id = ? AND filename_normalized = ? AND status IN ? AND expires_at > ?", userID, input.FolderID, normalized, []string{StatusCreated, StatusUploading, StatusVerifying}, time.Now().UTC()).
			Count(&activeWithSameName).Error; err != nil {
			return err
		}
		if activeWithSameName > 0 {
			return ErrNameConflict
		}

		object, objectErr := s.objects.FindOwnedWith(tx, userID, input.SHA256, input.SizeBytes)
		if objectErr == nil {
			if err := s.quota.AddLogicalUsage(tx, userID, input.SizeBytes); err != nil {
				return ErrQuotaExceeded
			}
			file, err := s.drive.CreateFile(tx, userID, input.FolderID, object.ID, filename)
			if err != nil {
				return err
			}
			if err := s.objects.AddReference(tx, object.ID); err != nil {
				return err
			}
			if err := outbox.Add(tx, "file_entry", file.ID, outbox.EventFileCreated, map[string]any{"file_entry_id": file.ID, "object_id": object.ID}); err != nil {
				return err
			}
			now := time.Now().UTC()
			session := Session{
				ID: uuid.Must(uuid.NewV7()), UserID: userID, FolderID: input.FolderID, Filename: filename, FilenameNormalized: normalized,
				DeclaredSHA256: input.SHA256, MimeType: input.MimeType, SizeBytes: input.SizeBytes, ReservedBytes: 0,
				Bucket: object.Bucket, ObjectKey: object.ObjectKey, StorageUploadID: "", PartSize: 0, PartCount: 0,
				Status: StatusCompleted, IdempotencyKey: input.IdempotencyKey, CompletedObjectID: &object.ID, CompletedEntryID: &file.ID,
				ExpiresAt: now.Add(s.config.SessionTTL), Version: 1, UpdatedAt: now,
			}
			if err := tx.Create(&session).Error; err != nil {
				return err
			}
			mode = "instant"
			resultID = file.ID
			return nil
		}
		if !errors.Is(objectErr, gorm.ErrRecordNotFound) {
			return objectErr
		}

		sessionID := uuid.Must(uuid.NewV7())
		objectKey := fmt.Sprintf("staging/%s/%s", userID, sessionID)
		storageUploadID, err := s.core.NewMultipartUpload(ctx, s.bucket, objectKey, minio.PutObjectOptions{ContentType: input.MimeType})
		if err != nil {
			return err
		}
		partSize, partCount := multipartPlan(input.SizeBytes)
		session := Session{
			ID: sessionID, UserID: userID, FolderID: input.FolderID, Filename: filename, FilenameNormalized: normalized,
			DeclaredSHA256: input.SHA256, MimeType: input.MimeType, SizeBytes: input.SizeBytes, ReservedBytes: input.SizeBytes,
			Bucket: s.bucket, ObjectKey: objectKey, StorageUploadID: storageUploadID, PartSize: partSize, PartCount: partCount,
			Status: StatusCreated, IdempotencyKey: input.IdempotencyKey, ExpiresAt: time.Now().UTC().Add(s.config.SessionTTL), Version: 1,
		}
		started = &session
		if err := s.quota.ReserveQuota(tx, userID, input.SizeBytes); err != nil {
			return ErrQuotaExceeded
		}
		if err := tx.Create(&session).Error; err != nil {
			return err
		}
		mode = "multipart"
		resultID = session.ID
		return nil
	})
	if err != nil {
		if started != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.core.AbortMultipartUpload(cleanupCtx, started.Bucket, started.ObjectKey, started.StorageUploadID)
			cancel()
		}
		if errors.Is(err, drive.ErrConflict) {
			return CreateResult{}, ErrFileExists
		}
		return CreateResult{}, err
	}
	if mode == "instant" {
		file, err := s.drive.File(userID, resultID)
		return CreateResult{Mode: mode, File: &file}, err
	}
	session, err := s.Get(userID, resultID)
	return CreateResult{Mode: mode, Upload: &session}, err
}

func (s *Service) Get(userID, sessionID uuid.UUID) (Session, error) {
	var session Session
	if err := s.db.Preload("ConfirmedParts", func(db *gorm.DB) *gorm.DB { return db.Order("part_number ASC") }).
		Where("id = ? AND user_id = ?", sessionID, userID).First(&session).Error; err != nil {
		return Session{}, ErrNotFound
	}
	return session, nil
}

func (s *Service) ListActive(userID uuid.UUID) ([]Session, error) {
	var sessions []Session
	err := s.db.Preload("ConfirmedParts", func(db *gorm.DB) *gorm.DB { return db.Order("part_number ASC") }).
		Where("user_id = ? AND status IN ? AND expires_at > ?", userID, []string{StatusCreated, StatusUploading, StatusVerifying}, time.Now().UTC()).
		Order("created_at DESC").Find(&sessions).Error
	return sessions, err
}

func (s *Service) PresignParts(ctx context.Context, userID, sessionID uuid.UUID, partNumbers []int) (map[int]string, time.Time, error) {
	session, err := s.Get(userID, sessionID)
	if err != nil || (session.Status != StatusCreated && session.Status != StatusUploading) {
		return nil, time.Time{}, ErrConflict
	}
	expires := s.config.PartURLTTL
	expiresAt := time.Now().UTC().Add(expires)
	result := make(map[int]string, len(partNumbers))
	for _, number := range partNumbers {
		if number < 1 || number > session.PartCount {
			return nil, time.Time{}, ErrInvalidInput
		}
		query := url.Values{"uploadId": {session.StorageUploadID}, "partNumber": {strconv.Itoa(number)}}
		presigned, err := s.presigner.Presign(ctx, http.MethodPut, session.Bucket, session.ObjectKey, expires, query)
		if err != nil {
			return nil, time.Time{}, err
		}
		result[number] = presigned.String()
	}
	return result, expiresAt, nil
}

func (s *Service) ConfirmParts(userID, sessionID uuid.UUID, parts []ConfirmedPart) (Session, error) {
	session, err := s.Get(userID, sessionID)
	if err != nil || (session.Status != StatusCreated && session.Status != StatusUploading) {
		return Session{}, ErrConflict
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		for _, confirmed := range parts {
			if confirmed.PartNumber < 1 || confirmed.PartNumber > session.PartCount || len(confirmed.ETag) == 0 || len(confirmed.ETag) > 512 || confirmed.SizeBytes <= 0 || (confirmed.Checksum != nil && len(*confirmed.Checksum) > 512) {
				return ErrInvalidInput
			}
			part := Part{SessionID: session.ID, PartNumber: confirmed.PartNumber, ETag: confirmed.ETag, SizeBytes: confirmed.SizeBytes, Checksum: confirmed.Checksum, CompletedAt: time.Now().UTC()}
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "session_id"}, {Name: "part_number"}}, DoUpdates: clause.AssignmentColumns([]string{"etag", "size_bytes", "checksum", "completed_at"})}).Create(&part).Error; err != nil {
				return err
			}
		}
		return tx.Model(&Session{}).Where("id = ? AND status IN ?", session.ID, []string{StatusCreated, StatusUploading}).Update("status", StatusUploading).Error
	})
	if err != nil {
		return Session{}, err
	}
	return s.Get(userID, sessionID)
}

func (s *Service) Complete(ctx context.Context, userID, sessionID uuid.UUID) (Session, error) {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session Session
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", sessionID, userID).Take(&session).Error; err != nil {
			return ErrNotFound
		}
		if session.Status == StatusVerifying || session.Status == StatusCompleted {
			return nil
		}
		if session.Status != StatusUploading && session.Status != StatusCreated {
			return ErrConflict
		}
		var parts []Part
		if err := tx.Where("session_id = ?", session.ID).Order("part_number ASC").Find(&parts).Error; err != nil || len(parts) != session.PartCount {
			return ErrConflict
		}
		completeParts := make([]minio.CompletePart, 0, len(parts))
		for _, part := range parts {
			completeParts = append(completeParts, minio.CompletePart{PartNumber: part.PartNumber, ETag: part.ETag})
		}
		if _, err := s.core.CompleteMultipartUpload(ctx, session.Bucket, session.ObjectKey, session.StorageUploadID, completeParts, minio.PutObjectOptions{}); err != nil {
			info, statErr := s.storage.StatObject(ctx, session.Bucket, session.ObjectKey, minio.StatObjectOptions{})
			if statErr != nil || info.Size != session.SizeBytes {
				return err
			}
		}
		result := tx.Model(&Session{}).Where("id = ? AND user_id = ? AND status IN ?", session.ID, userID, []string{StatusCreated, StatusUploading}).
			Updates(map[string]any{"status": StatusVerifying, "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()})
		if result.Error != nil || result.RowsAffected != 1 {
			return ErrConflict
		}
		return outbox.Add(tx, "upload_session", session.ID, outbox.EventObjectVerifyRequested, map[string]any{"upload_id": session.ID})
	})
	if err != nil {
		return Session{}, err
	}
	return s.Get(userID, sessionID)
}

func (s *Service) Abort(ctx context.Context, userID, sessionID uuid.UUID) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session Session
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", sessionID, userID).Take(&session).Error; err != nil {
			return ErrNotFound
		}
		if session.Status == StatusAborted || session.Status == StatusFailed {
			return nil
		}
		if session.Status == StatusCompleted || session.Status == StatusVerifying {
			return ErrConflict
		}
		if err := s.core.AbortMultipartUpload(ctx, session.Bucket, session.ObjectKey, session.StorageUploadID); err != nil {
			return err
		}
		if err := s.quota.ReleaseQuota(tx, userID, session.ReservedBytes); err != nil {
			return err
		}
		return tx.Model(&session).Updates(map[string]any{
			"status": StatusAborted, "reserved_bytes": 0, "storage_upload_id": "", "updated_at": time.Now().UTC(),
		}).Error
	})
}

func (s *Service) Expire(ctx context.Context, limit int) (int, error) {
	var ids []uuid.UUID
	if err := s.db.WithContext(ctx).Model(&Session{}).
		Where("status IN ? AND expires_at <= ?", []string{StatusCreated, StatusUploading}, time.Now().UTC()).
		Order("expires_at ASC").Limit(limit).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	expired := 0
	for _, sessionID := range ids {
		changed, err := s.expireOne(ctx, sessionID)
		if err != nil {
			return expired, err
		}
		if changed {
			expired++
		}
	}
	return expired, nil
}

func (s *Service) expireOne(ctx context.Context, sessionID uuid.UUID) (bool, error) {
	changed := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session Session
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&session, "id = ?", sessionID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if (session.Status != StatusCreated && session.Status != StatusUploading) || session.ExpiresAt.After(time.Now().UTC()) {
			return nil
		}
		if err := s.core.AbortMultipartUpload(ctx, session.Bucket, session.ObjectKey, session.StorageUploadID); err != nil {
			return err
		}
		if err := s.quota.ReleaseQuota(tx, session.UserID, session.ReservedBytes); err != nil {
			return err
		}
		if err := tx.Model(&session).Updates(map[string]any{
			"status": StatusExpired, "reserved_bytes": 0, "storage_upload_id": "", "updated_at": time.Now().UTC(),
		}).Error; err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

func (s *Service) Verify(ctx context.Context, sessionID uuid.UUID) error {
	var session Session
	if err := s.db.First(&session, "id = ?", sessionID).Error; err != nil {
		return ErrNotFound
	}
	if session.Status == StatusCompleted {
		return s.removeStaging(ctx, session)
	}
	if session.Status != StatusVerifying {
		return ErrConflict
	}
	objectReader, err := s.storage.GetObject(ctx, session.Bucket, session.ObjectKey, minio.GetObjectOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = objectReader.Close() }()
	hasher := sha256.New()
	size, err := io.Copy(hasher, objectReader)
	if err != nil {
		return err
	}
	actualHash := hex.EncodeToString(hasher.Sum(nil))
	if size != session.SizeBytes || actualHash != session.DeclaredSHA256 {
		return s.fail(ctx, session.ID, "upload.hash_mismatch")
	}

	canonicalKey := fmt.Sprintf("objects/sha256/%s/%s", actualHash[:2], actualHash)
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var locked Session
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, "id = ?", session.ID).Error; err != nil {
			return err
		}
		if locked.Status == StatusCompleted {
			return nil
		}
		if locked.Status != StatusVerifying {
			return ErrConflict
		}
		if err := dblock.Transaction(tx, "upload:object:"+actualHash+":"+strconv.FormatInt(size, 10)); err != nil {
			return err
		}
		filenameLock := "upload:filename:" + locked.UserID.String() + ":" + locked.FolderID.String() + ":" + locked.FilenameNormalized
		if err := dblock.Transaction(tx, filenameLock); err != nil {
			return err
		}
		var fileCount int64
		if err := tx.Model(&drive.FileEntry{}).
			Where("owner_id = ? AND folder_id = ? AND name_normalized = ? AND deleted_at IS NULL", locked.UserID, locked.FolderID, locked.FilenameNormalized).
			Count(&fileCount).Error; err != nil {
			return err
		}
		if fileCount > 0 {
			return ErrNameConflict
		}

		copiedCanonical := false
		if _, findErr := s.objects.FindByHashWith(tx, actualHash, size); errors.Is(findErr, gorm.ErrRecordNotFound) {
			if _, copyErr := s.storage.CopyObject(ctx,
				minio.CopyDestOptions{Bucket: session.Bucket, Object: canonicalKey},
				minio.CopySrcOptions{Bucket: session.Bucket, Object: session.ObjectKey}); copyErr != nil {
				return copyErr
			}
			copiedCanonical = true
		} else if findErr != nil {
			return findErr
		}
		compensate := func(cause error) error {
			if !copiedCanonical {
				return cause
			}
			if cleanupErr := s.storage.RemoveObject(ctx, session.Bucket, canonicalKey, minio.RemoveObjectOptions{}); cleanupErr != nil {
				return errors.Join(cause, fmt.Errorf("remove uncommitted canonical object: %w", cleanupErr))
			}
			return cause
		}

		verifiedAt := time.Now().UTC()
		object, err := s.objects.CreateOrGet(tx, objectstore.Object{
			ID: uuid.Must(uuid.NewV7()), SHA256: actualHash, SizeBytes: size, MimeType: session.MimeType,
			Bucket: session.Bucket, ObjectKey: canonicalKey, Status: "ready", VerifiedAt: &verifiedAt,
		})
		if err != nil {
			return compensate(err)
		}
		entry, err := s.drive.CreateFile(tx, session.UserID, session.FolderID, object.ID, session.Filename)
		if err != nil {
			return compensate(err)
		}
		if err := s.objects.AddReference(tx, object.ID); err != nil {
			return compensate(err)
		}
		if err := s.quota.CommitQuota(tx, session.UserID, session.ReservedBytes, session.SizeBytes); err != nil {
			return compensate(err)
		}
		if err := tx.Model(&locked).Updates(map[string]any{"status": StatusCompleted, "completed_object_id": object.ID, "completed_entry_id": entry.ID, "reserved_bytes": 0, "updated_at": verifiedAt}).Error; err != nil {
			return compensate(err)
		}
		if err := outbox.Add(tx, "file_object", object.ID, outbox.EventObjectReady, map[string]any{"object_id": object.ID}); err != nil {
			return compensate(err)
		}
		return nil
	})
	if errors.Is(err, ErrNameConflict) {
		return s.fail(ctx, session.ID, "upload.name_conflict")
	}
	if errors.Is(err, drive.ErrConflict) {
		return s.fail(ctx, session.ID, "upload.file_exists")
	}
	if err == nil {
		return s.removeStaging(ctx, session)
	}
	return err
}

func (s *Service) FailVerification(ctx context.Context, sessionID uuid.UUID, errorCode string) error {
	return s.fail(ctx, sessionID, errorCode)
}

func (s *Service) fail(ctx context.Context, sessionID uuid.UUID, errorCode string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session Session
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&session, "id = ?", sessionID).Error; err != nil {
			return err
		}
		if session.Status != StatusVerifying {
			return nil
		}
		if err := s.removeStaging(ctx, session); err != nil {
			return err
		}
		if err := s.quota.ReleaseQuota(tx, session.UserID, session.ReservedBytes); err != nil {
			return err
		}
		return tx.Model(&session).
			Updates(map[string]any{"status": StatusFailed, "error_code": errorCode, "reserved_bytes": 0, "updated_at": time.Now().UTC()}).Error
	})
}

func (s *Service) removeStaging(ctx context.Context, session Session) error {
	if !strings.HasPrefix(session.ObjectKey, "staging/") {
		return nil
	}
	return s.storage.RemoveObject(ctx, session.Bucket, session.ObjectKey, minio.RemoveObjectOptions{})
}

func normalizeFilename(value string) (string, string, error) {
	name := strings.TrimSpace(value)
	if name == "" || len(name) > 255 || strings.ContainsAny(name, "/\\\x00") {
		return "", "", ErrInvalidInput
	}
	return name, strings.ToLower(name), nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func multipartPlan(size int64) (int64, int) {
	partSize := int64(10 * 1024 * 1024)
	for (size+partSize-1)/partSize > 10000 {
		partSize *= 2
	}
	return partSize, int((size + partSize - 1) / partSize)
}
