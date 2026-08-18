package sharing

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dblock"
)

var (
	ErrNotFound = errors.New("share not found")
	ErrConflict = errors.New("share conflict")
)

type Share struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey"`
	OwnerID     uuid.UUID `gorm:"type:uuid"`
	FileEntryID uuid.UUID `gorm:"type:uuid"`
	KeyHash     []byte
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
	Version     int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (Share) TableName() string { return "shares" }

type Import struct {
	ID              uuid.UUID `gorm:"type:uuid;primaryKey"`
	ShareID         uuid.UUID `gorm:"type:uuid"`
	UserID          uuid.UUID `gorm:"type:uuid"`
	TargetFolderID  uuid.UUID `gorm:"type:uuid"`
	ImportedEntryID uuid.UUID `gorm:"type:uuid"`
	IdempotencyKey  string
	CreatedAt       time.Time
}

func (Import) TableName() string { return "share_imports" }

type Quota interface {
	AddLogicalUsage(*gorm.DB, uuid.UUID, int64) error
}

type Service struct {
	db        *gorm.DB
	drive     *drive.Service
	objects   *objectstore.Service
	quota     Quota
	secret    []byte
	accessTTL time.Duration
}

func NewService(db *gorm.DB, driveService *drive.Service, objects *objectstore.Service, quota Quota, cfg config.Sharing) *Service {
	return &Service{db: db, drive: driveService, objects: objects, quota: quota, secret: []byte(cfg.Secret), accessTTL: cfg.AccessTTL}
}

func (s *Service) Create(ownerID, fileID uuid.UUID, expiresAt *time.Time) (Share, string, error) {
	if _, err := s.drive.File(ownerID, fileID); err != nil {
		return Share{}, "", ErrNotFound
	}
	if expiresAt != nil && !expiresAt.After(time.Now().UTC()) {
		return Share{}, "", ErrConflict
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return Share{}, "", err
	}
	key := base64.RawURLEncoding.EncodeToString(raw)
	share := Share{
		ID: uuid.Must(uuid.NewV7()), OwnerID: ownerID, FileEntryID: fileID,
		KeyHash: s.keyHash(key), ExpiresAt: expiresAt, Version: 1,
	}
	if err := s.db.Create(&share).Error; err != nil {
		return Share{}, "", err
	}
	return share, key, nil
}

func (s *Service) List(ownerID uuid.UUID) ([]Share, error) {
	var shares []Share
	err := s.db.Where("owner_id = ?", ownerID).Order("created_at DESC").Find(&shares).Error
	return shares, err
}

func (s *Service) Get(ownerID, shareID uuid.UUID) (Share, error) {
	var share Share
	if err := s.db.Where("id = ? AND owner_id = ?", shareID, ownerID).First(&share).Error; err != nil {
		return Share{}, ErrNotFound
	}
	return share, nil
}

func (s *Service) Revoke(ownerID, shareID uuid.UUID) error {
	now := time.Now().UTC()
	result := s.db.Model(&Share{}).Where("id = ? AND owner_id = ? AND revoked_at IS NULL", shareID, ownerID).
		Updates(map[string]any{"revoked_at": now, "version": gorm.Expr("version + 1"), "updated_at": now})
	if result.Error != nil || result.RowsAffected != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) Resolve(key string) (Share, drive.FileView, string, time.Time, error) {
	if len(key) != 22 {
		return Share{}, drive.FileView{}, "", time.Time{}, ErrNotFound
	}
	var share Share
	if err := s.db.Where("key_hash = ?", s.keyHash(key)).First(&share).Error; err != nil {
		return Share{}, drive.FileView{}, "", time.Time{}, ErrNotFound
	}
	if err := validateShare(share); err != nil {
		return Share{}, drive.FileView{}, "", time.Time{}, err
	}
	file, err := s.drive.File(share.OwnerID, share.FileEntryID)
	if err != nil {
		return Share{}, drive.FileView{}, "", time.Time{}, ErrNotFound
	}
	expiresAt := time.Now().UTC().Add(s.accessTTL)
	token, err := s.signAccess(share.ID, expiresAt)
	return share, file, token, expiresAt, err
}

func (s *Service) Access(raw string) (Share, drive.FileView, error) {
	if len(raw) == 0 || len(raw) > 1024 {
		return Share{}, drive.FileView{}, ErrNotFound
	}
	shareID, err := s.verifyAccess(raw)
	if err != nil {
		return Share{}, drive.FileView{}, ErrNotFound
	}
	var share Share
	if err := s.db.First(&share, "id = ?", shareID).Error; err != nil {
		return Share{}, drive.FileView{}, ErrNotFound
	}
	if err := validateShare(share); err != nil {
		return Share{}, drive.FileView{}, err
	}
	file, err := s.drive.File(share.OwnerID, share.FileEntryID)
	if err != nil {
		return Share{}, drive.FileView{}, ErrNotFound
	}
	return share, file, nil
}

// Import 需要 ctx：事务里写 Outbox 时要注入当前 span context。
func (s *Service) Import(ctx context.Context, userID, shareID, targetFolderID uuid.UUID, idempotencyKey, accessToken string) (drive.FileView, error) {
	share, source, err := s.Access(accessToken)
	if err != nil || share.ID != shareID || len(idempotencyKey) == 0 || len(idempotencyKey) > 128 {
		return drive.FileView{}, ErrNotFound
	}
	if _, err := s.drive.Folder(userID, targetFolderID); err != nil {
		return drive.FileView{}, ErrNotFound
	}
	object, err := s.objects.Get(source.ObjectID)
	if err != nil {
		return drive.FileView{}, ErrNotFound
	}
	var importedID uuid.UUID
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		lockKey := "share:import:" + userID.String() + ":" + shareID.String() + ":" + idempotencyKey
		if err := dblock.Transaction(tx, lockKey); err != nil {
			return err
		}
		var prior Import
		err := tx.Where("user_id = ? AND share_id = ? AND idempotency_key = ?", userID, shareID, idempotencyKey).Take(&prior).Error
		if err == nil {
			importedID = prior.ImportedEntryID
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := s.quota.AddLogicalUsage(tx, userID, object.SizeBytes); err != nil {
			return ErrConflict
		}
		entry, err := s.drive.CreateFile(tx, userID, targetFolderID, object.ID, source.Name)
		if err != nil {
			return err
		}
		importedID = entry.ID
		if err := s.objects.AddReference(tx, object.ID); err != nil {
			return err
		}
		row := Import{ID: uuid.Must(uuid.NewV7()), ShareID: share.ID, UserID: userID, TargetFolderID: targetFolderID, ImportedEntryID: entry.ID, IdempotencyKey: idempotencyKey}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		return outbox.Add(tx, "share", share.ID, outbox.EventShareImported, map[string]any{"share_id": share.ID, "file_entry_id": entry.ID})
	})
	if err != nil {
		return drive.FileView{}, err
	}
	return s.drive.File(userID, importedID)
}

func (s *Service) keyHash(key string) []byte {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(key))
	return mac.Sum(nil)
}

type accessClaims struct {
	ShareID   uuid.UUID `json:"share_id"`
	ExpiresAt int64     `json:"expires_at"`
}

func (s *Service) signAccess(shareID uuid.UUID, expiresAt time.Time) (string, error) {
	payload, err := json.Marshal(accessClaims{ShareID: shareID, ExpiresAt: expiresAt.Unix()})
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *Service) verifyAccess(raw string) (uuid.UUID, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return uuid.Nil, ErrNotFound
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(parts[0]))
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(signature, mac.Sum(nil)) {
		return uuid.Nil, ErrNotFound
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	var claims accessClaims
	if json.Unmarshal(payload, &claims) != nil || time.Now().UTC().Unix() >= claims.ExpiresAt {
		return uuid.Nil, ErrNotFound
	}
	return claims.ShareID, nil
}

func validateShare(share Share) error {
	now := time.Now().UTC()
	if share.RevokedAt != nil || (share.ExpiresAt != nil && !share.ExpiresAt.After(now)) {
		return ErrNotFound
	}
	return nil
}
