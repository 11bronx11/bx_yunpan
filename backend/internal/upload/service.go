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
	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/ownerepoch"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dblock"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/storageurl"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
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

// multipartUploadLister is optional so test doubles and providers without a
// listing API remain usable. MinIO's Core implements it; recovery uses the
// listing to avoid creating a second provider upload after a crash.
type multipartUploadLister interface {
	ListMultipartUploads(context.Context, string, string, string, string, string, int) (minio.ListMultipartUploadsResult, error)
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
			if existing.FolderID != input.FolderID || existing.Filename != filename ||
				existing.DeclaredSHA256 != input.SHA256 || existing.SizeBytes != input.SizeBytes || existing.MimeType != input.MimeType {
				return ErrConflict
			}
			if existing.StorageUploadID == "" && existing.Status == StatusCompleted && existing.CompletedEntryID != nil {
				mode = "instant"
				resultID = *existing.CompletedEntryID
			} else {
				mode = "multipart"
				resultID = existing.ID
				started = &existing
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
			Where("user_id = ? AND folder_id = ? AND filename_normalized = ? AND status IN ? AND expires_at > ?", userID, input.FolderID, normalized, []string{StatusInitializing, StatusCreated, StatusUploading, StatusCompleting, StatusVerifying}, time.Now().UTC()).
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
			epoch, err := ownerepoch.Current(tx, userID)
			if err != nil {
				return err
			}
			if err := s.objects.AddReference(tx, object.ID); err != nil {
				return err
			}
			if err := outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
				EventType: eventcontract.EventFileCreatedV1, AggregateID: file.ID, AggregateVersion: file.Version,
				Producer: "core-api", Payload: eventcontract.FileCreatedV1{OwnerID: userID, FileEntryID: file.ID, ObjectID: object.ID, ContentEpoch: epoch.ContentEpoch},
			}); err != nil {
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
		partSize, partCount := multipartPlan(input.SizeBytes)
		session := Session{
			ID: sessionID, UserID: userID, FolderID: input.FolderID, Filename: filename, FilenameNormalized: normalized,
			DeclaredSHA256: input.SHA256, MimeType: input.MimeType, SizeBytes: input.SizeBytes, ReservedBytes: input.SizeBytes,
			Bucket: s.bucket, ObjectKey: objectKey, StorageUploadID: "", PartSize: partSize, PartCount: partCount,
			Status: StatusInitializing, IdempotencyKey: input.IdempotencyKey, ExpiresAt: time.Now().UTC().Add(s.config.SessionTTL), Version: 1,
		}
		if err := s.quota.ReserveQuota(tx, userID, input.SizeBytes); err != nil {
			return ErrQuotaExceeded
		}
		if err := tx.Create(&session).Error; err != nil {
			return err
		}
		_, err = enqueueUploadOperation(tx, storageops.Input{
			OperationType: storageops.MultipartCreate, IdempotencyKey: "multipart.create:" + session.ID.String(),
			UploadSessionID: &session.ID, SourceBucket: s.bucket, SourceKey: objectKey,
		})
		if err != nil {
			return err
		}
		started = &session
		mode = "multipart"
		resultID = session.ID
		return nil
	})
	if err != nil {
		if errors.Is(err, drive.ErrConflict) {
			return CreateResult{}, ErrFileExists
		}
		return CreateResult{}, err
	}
	if mode == "instant" {
		file, err := s.drive.File(userID, resultID)
		return CreateResult{Mode: mode, File: &file}, err
	}
	return CreateResult{Mode: mode, Upload: started}, nil
}

func (s *Service) persistMultipartUploadID(ctx context.Context, operationID, leaseToken uuid.UUID, uploadID string) error {
	if operationID == uuid.Nil || leaseToken == uuid.Nil || strings.TrimSpace(uploadID) == "" {
		return storageops.ErrInvalidOperation
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&storageops.Operation{}).
			Where("id = ? AND state = ? AND lease_token = ? AND (storage_upload_id IS NULL OR storage_upload_id = '')", operationID, storageops.StateRunning, leaseToken).
			Updates(map[string]any{"storage_upload_id": uploadID, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 1 {
			return nil
		}
		var current storageops.Operation
		if err := tx.First(&current, "id = ?", operationID).Error; err != nil {
			return err
		}
		if current.StorageUploadID == uploadID {
			return nil
		}
		return errors.New("multipart upload operation is no longer writable")
	})
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
		Where("user_id = ? AND status IN ? AND expires_at > ?", userID, []string{StatusInitializing, StatusCreated, StatusUploading, StatusCompleting, StatusVerifying}, time.Now().UTC()).
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
	var session Session
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", sessionID, userID).Take(&session).Error; err != nil {
			return ErrNotFound
		}
		if session.Status == StatusVerifying || session.Status == StatusCompleted {
			return nil
		}
		if session.Status == StatusCompleting {
			return nil
		}
		if session.Status != StatusUploading && session.Status != StatusCreated {
			return ErrConflict
		}
		var partCount int64
		if err := tx.Model(&Part{}).Where("session_id = ?", session.ID).Count(&partCount).Error; err != nil || int(partCount) != session.PartCount {
			return ErrConflict
		}
		if strings.TrimSpace(session.StorageUploadID) == "" {
			return ErrConflict
		}
		result := tx.Model(&Session{}).Where("id = ? AND user_id = ? AND status IN ?", session.ID, userID, []string{StatusCreated, StatusUploading}).
			Updates(map[string]any{"status": StatusCompleting, "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()})
		if result.Error != nil || result.RowsAffected != 1 {
			return ErrConflict
		}
		session.Status = StatusCompleting
		session.Version++
		_, err := enqueueUploadOperation(tx, storageops.Input{
			OperationType: storageops.MultipartComplete, IdempotencyKey: "multipart.complete:" + session.ID.String(),
			UploadSessionID: &session.ID, SourceBucket: session.Bucket, SourceKey: session.ObjectKey,
			StorageUploadID: session.StorageUploadID,
		})
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return session, nil
}

func (s *Service) Abort(ctx context.Context, userID, sessionID uuid.UUID) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session Session
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", sessionID, userID).Take(&session).Error; err != nil {
			return ErrNotFound
		}
		if session.Status == StatusAborted || session.Status == StatusFailed {
			return nil
		}
		if session.Status == StatusCompleted || session.Status == StatusVerifying || session.Status == StatusCompleting {
			return ErrConflict
		}
		if err := s.quota.ReleaseQuota(tx, userID, session.ReservedBytes); err != nil {
			return err
		}
		if err := tx.Model(&session).Updates(map[string]any{
			"status": StatusAborted, "reserved_bytes": 0, "updated_at": time.Now().UTC(), "version": gorm.Expr("version + 1"),
		}).Error; err != nil {
			return err
		}
		if session.StorageUploadID != "" {
			_, err := enqueueUploadOperation(tx, storageops.Input{
				OperationType: storageops.MultipartAbort, IdempotencyKey: "multipart.abort:" + session.ID.String(),
				UploadSessionID: &session.ID, SourceBucket: session.Bucket, SourceKey: session.ObjectKey,
				StorageUploadID: session.StorageUploadID,
			})
			return err
		}
		return nil
	})
	return err
}

func (s *Service) Expire(ctx context.Context, limit int) (int, error) {
	var ids []uuid.UUID
	if err := s.db.WithContext(ctx).Model(&Session{}).
		Where("status IN ? AND expires_at <= ?", []string{StatusInitializing, StatusCreated, StatusUploading}, time.Now().UTC()).
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
		if (session.Status != StatusInitializing && session.Status != StatusCreated && session.Status != StatusUploading) || session.ExpiresAt.After(time.Now().UTC()) {
			return nil
		}
		if err := s.quota.ReleaseQuota(tx, session.UserID, session.ReservedBytes); err != nil {
			return err
		}
		if err := tx.Model(&session).Updates(map[string]any{
			"status": StatusExpired, "reserved_bytes": 0, "updated_at": time.Now().UTC(), "version": gorm.Expr("version + 1"),
		}).Error; err != nil {
			return err
		}
		if session.StorageUploadID != "" {
			_, err := enqueueUploadOperation(tx, storageops.Input{
				OperationType: storageops.MultipartAbort, IdempotencyKey: "multipart.abort:" + session.ID.String(),
				UploadSessionID: &session.ID, SourceBucket: session.Bucket, SourceKey: session.ObjectKey,
				StorageUploadID: session.StorageUploadID,
			})
			if err != nil {
				return err
			}
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
		return s.scheduleStagingDelete(ctx, session)
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

	canonicalKey, err := s.ensureCanonical(ctx, session, actualHash, size)
	if err != nil {
		return err
	}
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

		verifiedAt := time.Now().UTC()
		object, err := s.objects.CreateOrGet(tx, objectstore.Object{
			ID: uuid.Must(uuid.NewV7()), SHA256: actualHash, SizeBytes: size, MimeType: session.MimeType, Version: 1,
			Bucket: session.Bucket, ObjectKey: canonicalKey, Status: "ready", VerifiedAt: &verifiedAt,
		})
		if err != nil {
			return err
		}
		entry, err := s.drive.CreateFile(tx, session.UserID, session.FolderID, object.ID, session.Filename)
		if err != nil {
			return err
		}
		epoch, err := ownerepoch.Current(tx, session.UserID)
		if err != nil {
			return err
		}
		if err := s.objects.AddReference(tx, object.ID); err != nil {
			return err
		}
		// AddReference advances the object aggregate version. Reload it before
		// emitting object.ready so the event version matches the committed fact.
		if err := tx.First(&object, "id = ?", object.ID).Error; err != nil {
			return err
		}
		if err := s.quota.CommitQuota(tx, session.UserID, session.ReservedBytes, session.SizeBytes); err != nil {
			return err
		}
		if err := tx.Model(&locked).Updates(map[string]any{"status": StatusCompleted, "completed_object_id": object.ID, "completed_entry_id": entry.ID, "reserved_bytes": 0, "updated_at": verifiedAt}).Error; err != nil {
			return err
		}
		if err := outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
			EventType: eventcontract.EventFileCreatedV1, AggregateID: entry.ID, AggregateVersion: entry.Version,
			Producer: "core-api", Payload: eventcontract.FileCreatedV1{OwnerID: session.UserID, FileEntryID: entry.ID, ObjectID: object.ID, ContentEpoch: epoch.ContentEpoch},
		}); err != nil {
			return err
		}
		if err := outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
			EventType: eventcontract.EventObjectReadyV1, AggregateID: object.ID, AggregateVersion: object.Version,
			Producer: "core-api", Payload: eventcontract.ObjectReadyV1{ObjectID: object.ID},
		}); err != nil {
			return err
		}
		if _, err := enqueueStagingDelete(tx, locked); err != nil {
			return err
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
		return s.scheduleStagingDelete(ctx, session)
	}
	return err
}

func (s *Service) FailVerification(ctx context.Context, sessionID uuid.UUID, errorCode string) error {
	return s.fail(ctx, sessionID, errorCode)
}

func (s *Service) fail(ctx context.Context, sessionID uuid.UUID, errorCode string) error {
	var session Session
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&session, "id = ?", sessionID).Error; err != nil {
			return err
		}
		if session.Status != StatusVerifying {
			if session.Status == StatusFailed || session.Status == StatusCompleted {
				return nil
			}
			return ErrConflict
		}
		if err := s.registerCanonicalCleanup(ctx, tx, session); err != nil {
			return err
		}
		if err := s.quota.ReleaseQuota(tx, session.UserID, session.ReservedBytes); err != nil {
			return err
		}
		if err := tx.Model(&session).
			Updates(map[string]any{"status": StatusFailed, "error_code": errorCode, "reserved_bytes": 0, "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
		_, err := enqueueStagingDelete(tx, session)
		return err
	})
	if err != nil {
		return err
	}
	return s.scheduleStagingDelete(ctx, session)
}

func (s *Service) ensureCanonical(ctx context.Context, session Session, sha256Value string, size int64) (string, error) {
	canonicalKey := fmt.Sprintf("objects/sha256/%s/%s", sha256Value[:2], sha256Value)
	var operation storageops.Operation
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Session
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", session.ID).Error; err != nil {
			return err
		}
		if current.Status != StatusVerifying {
			return ErrConflict
		}
		if err := dblock.Transaction(tx, "upload:object:"+sha256Value+":"+strconv.FormatInt(size, 10)); err != nil {
			return err
		}
		var existing objectstore.Object
		if err := tx.Where("sha256 = ? AND size_bytes = ?", sha256Value, size).First(&existing).Error; err == nil {
			switch existing.Status {
			case "ready":
				canonicalKey = existing.ObjectKey
				return nil
			case "deleted":
				canonicalKey = objectstore.RevivalKey(existing)
			default:
				return errors.New("canonical object is being deleted")
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var err error
		operation, err = storageops.Enqueue(tx, storageops.Input{
			OperationType: storageops.CanonicalEnsure, IdempotencyKey: "canonical.ensure:" + canonicalKey,
			UploadSessionID: &session.ID, SourceBucket: session.Bucket, SourceKey: session.ObjectKey,
			TargetBucket: session.Bucket, TargetKey: canonicalKey,
		})
		return err
	})
	if err != nil || operation.ID == uuid.Nil || operation.State == storageops.StateSucceeded {
		return canonicalKey, err
	}
	return canonicalKey, s.executeCanonicalEnsure(ctx, operation)
}

func (s *Service) executeCanonicalEnsure(ctx context.Context, operation storageops.Operation) error {
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		claimed, leaseToken, err := storageops.TryStartWithToken(s.db, operation.ID, 5*time.Minute)
		if err != nil {
			return err
		}
		if claimed {
			operation.LeaseToken = &leaseToken
			if err := s.reconcileOperation(ctx, operation); err != nil {
				_ = storageops.MarkFailedOwned(s.db, operation.ID, leaseToken, err, time.Second)
				return err
			}
			return nil
		}
		current, err := storageops.Get(s.db, operation.ID)
		if err != nil {
			return err
		}
		switch current.State {
		case storageops.StateSucceeded:
			return nil
		case storageops.StateFailed:
			if current.AvailableAt.After(time.Now().UTC()) {
				// Wait for the retry window instead of racing a worker that
				// still owns the current lease/backoff.
			} else {
				operation = current
			}
		case storageops.StatePending:
			operation = current
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("canonical ensure operation still running")
		case <-ticker.C:
		}
	}
}

func (s *Service) scheduleStagingDelete(ctx context.Context, session Session) error {
	if !strings.HasPrefix(session.ObjectKey, "staging/") {
		return nil
	}
	var operation storageops.Operation
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		operation, err = enqueueStagingDelete(tx, session)
		return err
	})
	if err != nil {
		return err
	}
	if operation.ID == uuid.Nil {
		return nil
	}
	if operation.State == storageops.StateSucceeded {
		return nil
	}
	claimed, leaseToken, claimErr := storageops.TryStartWithToken(s.db, operation.ID, 5*time.Minute)
	if claimErr != nil {
		return claimErr
	}
	if !claimed {
		current, getErr := storageops.Get(s.db, operation.ID)
		if getErr != nil {
			return getErr
		}
		if current.State == storageops.StateSucceeded {
			return nil
		}
		return errors.New("staging delete operation is already running")
	}
	if err := s.storage.RemoveObject(ctx, session.Bucket, session.ObjectKey, minio.RemoveObjectOptions{}); err != nil {
		_ = storageops.MarkFailedOwned(s.db, operation.ID, leaseToken, err, time.Second)
		return err
	}
	return storageops.MarkSucceededOwned(s.db, operation.ID, leaseToken)
}

func enqueueStagingDelete(tx *gorm.DB, session Session) (storageops.Operation, error) {
	if tx == nil || !strings.HasPrefix(session.ObjectKey, "staging/") {
		return storageops.Operation{}, nil
	}
	return storageops.Enqueue(tx, storageops.Input{
		OperationType: storageops.StagingDelete, IdempotencyKey: "staging.delete:" + session.ID.String(),
		UploadSessionID: &session.ID, SourceBucket: session.Bucket, SourceKey: session.ObjectKey,
	})
}

// ReconcileStorageOperations claims durable operations left by a crashed
// process and executes them outside database transactions. It is deliberately
// safe to run from several worker replicas because Claim uses row locks and a
// lease in PostgreSQL.
func (s *Service) ReconcileStorageOperations(ctx context.Context, limit int) (int, error) {
	operations, err := storageops.ClaimTypes(ctx, s.db, limit, 5*time.Minute,
		storageops.MultipartCreate, storageops.MultipartComplete, storageops.MultipartAbort,
		storageops.CanonicalEnsure, storageops.StagingDelete)
	if err != nil {
		return 0, err
	}
	completed := 0
	for _, operation := range operations {
		if err := s.reconcileOperation(ctx, operation); err != nil {
			if operation.LeaseToken != nil {
				_ = storageops.MarkFailedOwned(s.db, operation.ID, *operation.LeaseToken, err, time.Second)
			}
			return completed, err
		}
		completed++
	}
	converged, err := s.reconcileSucceededStorageOperations(ctx, limit-completed)
	if err != nil {
		return completed, err
	}
	orphans, err := s.reconcileCanonicalOrphans(ctx, limit-completed-converged)
	return completed + converged + orphans, err
}

// ReconcileStorageOperation handles a single recovery nudge. The operation
// row is re-read and claimed again so a published recovery event is harmless
// when another worker already completed the work.
func (s *Service) ReconcileStorageOperation(ctx context.Context, operationID uuid.UUID) error {
	operation, err := storageops.Get(s.db, operationID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if operation.State == storageops.StateSucceeded {
		if operation.OperationType == storageops.MultipartCreate || operation.OperationType == storageops.MultipartComplete {
			return s.convergeSucceededOperation(ctx, operation)
		}
		return nil
	}
	claimed, token, err := storageops.TryStartWithToken(s.db, operation.ID, 5*time.Minute)
	if err != nil || !claimed {
		return err
	}
	operation.LeaseToken = &token
	if err := s.reconcileOperation(ctx, operation); err != nil {
		_ = storageops.MarkFailedOwned(s.db, operation.ID, token, err, time.Second)
		return err
	}
	return nil
}

// reconcileSucceededStorageOperations handles the window where the external
// provider operation was durably marked succeeded but the business-state
// transaction failed. It is deliberately separate from ClaimTypes because a
// succeeded operation must never repeat its external side effect.
func (s *Service) reconcileSucceededStorageOperations(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	var operations []storageops.Operation
	if err := s.db.WithContext(ctx).
		Where("state = ? AND operation_type IN ? AND upload_session_id IS NOT NULL", storageops.StateSucceeded, []string{storageops.MultipartCreate, storageops.MultipartComplete}).
		Where(`(operation_type = ? AND EXISTS (SELECT 1 FROM upload_sessions s WHERE s.id = storage_operations.upload_session_id AND s.status = ?)) OR
			(operation_type = ? AND EXISTS (SELECT 1 FROM upload_sessions s WHERE s.id = storage_operations.upload_session_id AND s.status = ?))`,
			storageops.MultipartCreate, StatusInitializing, storageops.MultipartComplete, StatusCompleting).
		Order("updated_at ASC").Limit(limit).Find(&operations).Error; err != nil {
		return 0, err
	}
	converged := 0
	for _, operation := range operations {
		if err := s.convergeSucceededOperation(ctx, operation); err != nil {
			return converged, err
		}
		converged++
	}
	return converged, nil
}

func (s *Service) convergeSucceededOperation(ctx context.Context, operation storageops.Operation) error {
	if operation.UploadSessionID == nil {
		return errors.New("succeeded storage operation has no session")
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session Session
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&session, "id = ?", *operation.UploadSessionID).Error; err != nil {
			return err
		}
		switch operation.OperationType {
		case storageops.MultipartCreate:
			if session.Status != StatusInitializing {
				return nil
			}
			if strings.TrimSpace(operation.StorageUploadID) == "" {
				return errors.New("succeeded multipart create has no provider upload id")
			}
			result := tx.Model(&Session{}).Where("id = ? AND status = ?", session.ID, StatusInitializing).
				Updates(map[string]any{"status": StatusCreated, "storage_upload_id": operation.StorageUploadID, "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()})
			if result.Error != nil {
				return result.Error
			}
			return nil
		case storageops.MultipartComplete:
			if session.Status == StatusVerifying || session.Status == StatusCompleted {
				return nil
			}
			if session.Status != StatusCompleting {
				return errors.New("succeeded multipart complete session is not completing")
			}
			result := tx.Model(&Session{}).Where("id = ? AND status = ?", session.ID, StatusCompleting).
				Updates(map[string]any{"status": StatusVerifying, "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()})
			if result.Error != nil || result.RowsAffected != 1 {
				return ErrConflict
			}
			return outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
				EventType: eventcontract.EventObjectVerifyRequestedV1, AggregateID: session.ID, AggregateVersion: session.Version + 1,
				Producer: "core-api", Payload: eventcontract.ObjectVerifyRequestedV1{UploadID: session.ID},
			})
		default:
			return nil
		}
	})
}

func (s *Service) reconcileOperation(ctx context.Context, operation storageops.Operation) error {
	switch operation.OperationType {
	case storageops.MultipartCreate:
		return s.reconcileMultipartCreate(ctx, operation)
	case storageops.MultipartComplete:
		return s.reconcileMultipartComplete(ctx, operation)
	case storageops.MultipartAbort:
		if operation.StorageUploadID == "" {
			return storageops.MarkSucceededOwned(s.db, operation.ID, operationLeaseToken(operation))
		}
		if err := s.core.AbortMultipartUpload(ctx, operation.SourceBucket, operation.SourceKey, operation.StorageUploadID); err != nil {
			return err
		}
		if err := storageops.MarkSucceededOwned(s.db, operation.ID, operationLeaseToken(operation)); err != nil {
			return err
		}
		if operation.UploadSessionID != nil {
			return s.db.Model(&Session{}).Where("id = ?", *operation.UploadSessionID).Update("storage_upload_id", "").Error
		}
		return nil
	case storageops.CanonicalEnsure:
		if operation.UploadSessionID != nil {
			var session Session
			if err := s.db.WithContext(ctx).First(&session, "id = ?", *operation.UploadSessionID).Error; err != nil {
				return err
			}
			if session.Status == StatusFailed || session.Status == StatusAborted || session.Status == StatusExpired {
				return storageops.MarkSucceededOwned(s.db, operation.ID, operationLeaseToken(operation))
			}
		}
		if _, err := s.storage.CopyObject(ctx,
			minio.CopyDestOptions{Bucket: operation.TargetBucket, Object: operation.TargetKey},
			minio.CopySrcOptions{Bucket: operation.SourceBucket, Object: operation.SourceKey}); err != nil {
			return err
		}
		return storageops.MarkSucceededOwned(s.db, operation.ID, operationLeaseToken(operation))
	case storageops.StagingDelete:
		if err := s.storage.RemoveObject(ctx, operation.SourceBucket, operation.SourceKey, minio.RemoveObjectOptions{}); err != nil {
			return err
		}
		return storageops.MarkSucceededOwned(s.db, operation.ID, operationLeaseToken(operation))
	default:
		return errors.New("storage operation must be handled by objectstore reconciler")
	}
}

func operationLeaseToken(operation storageops.Operation) uuid.UUID {
	if operation.LeaseToken == nil {
		return uuid.Nil
	}
	return *operation.LeaseToken
}

func (s *Service) reconcileMultipartCreate(ctx context.Context, operation storageops.Operation) error {
	if operation.UploadSessionID == nil {
		return errors.New("multipart create operation has no session")
	}
	var session Session
	if err := s.db.First(&session, "id = ?", *operation.UploadSessionID).Error; err != nil {
		return err
	}
	terminal := session.Status == StatusAborted || session.Status == StatusExpired || session.Status == StatusFailed
	if session.Status != StatusInitializing && !terminal {
		return storageops.MarkSucceededOwned(s.db, operation.ID, operationLeaseToken(operation))
	}
	leaseToken := operationLeaseToken(operation)
	uploadID := operation.StorageUploadID
	if uploadID == "" {
		var err error
		if lister, ok := s.core.(multipartUploadLister); ok {
			uploadID, err = s.recoverExistingMultipartUpload(ctx, lister, operation.SourceBucket, operation.SourceKey)
		}
		if err == nil && uploadID == "" && !terminal {
			uploadID, err = s.core.NewMultipartUpload(ctx, operation.SourceBucket, operation.SourceKey, minio.PutObjectOptions{ContentType: session.MimeType})
		}
		if err != nil {
			return err
		}
	}
	if uploadID == "" && terminal {
		return storageops.MarkSucceededOwned(s.db, operation.ID, leaseToken)
	}
	if err := s.persistMultipartUploadID(ctx, operation.ID, leaseToken, uploadID); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Session
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", session.ID).Error; err != nil {
			return err
		}
		if current.Status == StatusInitializing {
			if err := tx.Model(&current).Updates(map[string]any{"status": StatusCreated, "storage_upload_id": uploadID, "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()}).Error; err != nil {
				return err
			}
		} else if current.Status == StatusAborted || current.Status == StatusExpired || current.Status == StatusFailed {
			if _, err := enqueueUploadOperation(tx, storageops.Input{
				OperationType: storageops.MultipartAbort, IdempotencyKey: "multipart.abort:" + session.ID.String(),
				UploadSessionID: &session.ID, SourceBucket: session.Bucket, SourceKey: session.ObjectKey, StorageUploadID: uploadID,
			}); err != nil {
				return err
			}
		}
		return storageops.MarkSucceededOwned(tx, operation.ID, leaseToken)
	})
}

func (s *Service) recoverExistingMultipartUpload(ctx context.Context, lister multipartUploadLister, bucket, objectKey string) (string, error) {
	var uploads []minio.ObjectMultipartInfo
	keyMarker, uploadIDMarker := "", ""
	for {
		result, err := lister.ListMultipartUploads(ctx, bucket, objectKey, keyMarker, uploadIDMarker, "", 1000)
		if err != nil {
			return "", err
		}
		for _, upload := range result.Uploads {
			if upload.Err != nil {
				return "", upload.Err
			}
			if upload.Key == objectKey && upload.UploadID != "" {
				uploads = append(uploads, upload)
			}
		}
		if !result.IsTruncated {
			break
		}
		if result.NextKeyMarker == keyMarker && result.NextUploadIDMarker == uploadIDMarker {
			return "", errors.New("multipart upload listing did not advance")
		}
		keyMarker, uploadIDMarker = result.NextKeyMarker, result.NextUploadIDMarker
	}
	if len(uploads) == 0 {
		return "", nil
	}
	selected := uploads[0]
	for _, upload := range uploads[1:] {
		if upload.Initiated.After(selected.Initiated) {
			selected = upload
		}
	}
	for _, upload := range uploads {
		if upload.UploadID == selected.UploadID {
			continue
		}
		if err := s.core.AbortMultipartUpload(ctx, bucket, objectKey, upload.UploadID); err != nil {
			return "", err
		}
	}
	return selected.UploadID, nil
}

func enqueueUploadOperation(tx *gorm.DB, input storageops.Input) (storageops.Operation, error) {
	op, err := storageops.Enqueue(tx, input)
	if err != nil {
		return op, err
	}
	key := "upload.operation:" + op.ID.String()
	_, err = outbox.EnsureRecovery(tx, key, "upload_operation_created", "storage_operation", op.ID, 1,
		eventcontract.EnvelopeInput{EventType: eventcontract.EventStorageOperationRecoverV1, AggregateID: op.ID, AggregateVersion: 1, Producer: "core-api",
			Payload: eventcontract.StorageOperationRecoverV1{OperationID: op.ID, OperationType: op.OperationType, RecoveryKey: key}})
	return op, err
}

func (s *Service) reconcileMultipartComplete(ctx context.Context, operation storageops.Operation) error {
	if operation.UploadSessionID == nil {
		return errors.New("multipart complete operation has no session")
	}
	var session Session
	if err := s.db.First(&session, "id = ?", *operation.UploadSessionID).Error; err != nil {
		return err
	}
	if session.Status == StatusVerifying || session.Status == StatusCompleted {
		return storageops.MarkSucceededOwned(s.db, operation.ID, operationLeaseToken(operation))
	}
	if session.Status != StatusCompleting {
		return errors.New("multipart complete session is not completing")
	}
	var parts []Part
	if err := s.db.Where("session_id = ?", session.ID).Order("part_number ASC").Find(&parts).Error; err != nil || len(parts) != session.PartCount {
		if err != nil {
			return err
		}
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
	leaseToken := operationLeaseToken(operation)
	if err := storageops.MarkSucceededOwned(s.db, operation.ID, leaseToken); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Session
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", session.ID).Error; err != nil {
			return err
		}
		if current.Status == StatusVerifying || current.Status == StatusCompleted {
			return nil
		}
		if current.Status != StatusCompleting {
			return ErrConflict
		}
		if err := tx.Model(&current).Updates(map[string]any{"status": StatusVerifying, "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
		return outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
			EventType: eventcontract.EventObjectVerifyRequestedV1, AggregateID: current.ID, AggregateVersion: current.Version + 1,
			Producer: "core-api", Payload: eventcontract.ObjectVerifyRequestedV1{UploadID: current.ID},
		})
	})
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
