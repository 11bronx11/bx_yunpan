package sharing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/ownerepoch"
)

var ErrVersionConflict = errors.New("share version conflict")

type ShareView struct {
	Share
	FileName      string
	SizeBytes     int64
	MimeType      string
	FileAvailable bool
}

func lookupError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}

func lockSource(tx *gorm.DB, ownerID, fileID uuid.UUID) error {
	var entry drive.FileEntry
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where("id = ? AND owner_id = ? AND deleted_at IS NULL", fileID, ownerID).First(&entry).Error; err != nil {
		return lookupError(err)
	}
	var ready bool
	if err := tx.Raw("SELECT EXISTS(SELECT 1 FROM file_objects WHERE id = ? AND status = 'ready')", entry.ObjectID).Scan(&ready).Error; err != nil {
		return err
	}
	if !ready {
		return ErrNotFound
	}
	return nil
}

func viewQuery(db *gorm.DB, ownerID uuid.UUID) *gorm.DB {
	return db.Table("shares s").Select(`s.*, COALESCE(e.name, '') AS file_name,
		COALESCE(o.size_bytes, 0) AS size_bytes, COALESCE(o.mime_type, '') AS mime_type,
		(e.id IS NOT NULL AND e.deleted_at IS NULL AND o.status = 'ready') AS file_available`).
		Joins("LEFT JOIN file_entries e ON e.id = s.file_entry_id AND e.owner_id = s.owner_id").
		Joins("LEFT JOIN file_objects o ON o.id = e.object_id").Where("s.owner_id = ?", ownerID)
}

func (s *Service) GetView(ctx context.Context, ownerID, shareID uuid.UUID) (ShareView, error) {
	var view ShareView
	err := viewQuery(s.db.WithContext(ctx), ownerID).Where("s.id = ?", shareID).Take(&view).Error
	return view, lookupError(err)
}

type shareCursor struct {
	OwnerID uuid.UUID `json:"owner"`
	Created time.Time `json:"created"`
	ID      uuid.UUID `json:"id"`
}

func (s *Service) ListPage(ctx context.Context, ownerID uuid.UUID, cursor string, limit int) ([]ShareView, string, error) {
	if limit < 1 || limit > 100 {
		return nil, "", ErrInvalidInput
	}
	query := viewQuery(s.db.WithContext(ctx), ownerID)
	if cursor != "" {
		parts := strings.Split(cursor, ".")
		if len(parts) != 2 || len(cursor) > 1024 {
			return nil, "", ErrInvalidInput
		}
		mac := hmac.New(sha256.New, s.secret)
		mac.Write([]byte("share-page:" + parts[0]))
		sig, sigErr := base64.RawURLEncoding.DecodeString(parts[1])
		data, dataErr := base64.RawURLEncoding.DecodeString(parts[0])
		var after shareCursor
		if sigErr != nil || dataErr != nil || !hmac.Equal(sig, mac.Sum(nil)) || json.Unmarshal(data, &after) != nil || after.OwnerID != ownerID || after.ID == uuid.Nil || after.Created.IsZero() {
			return nil, "", ErrInvalidInput
		}
		query = query.Where("(s.created_at, s.id) < (?, ?)", after.Created, after.ID)
	}
	var views []ShareView
	if err := query.Order("s.created_at DESC, s.id DESC").Limit(limit + 1).Scan(&views).Error; err != nil {
		return nil, "", err
	}
	next := ""
	if len(views) > limit {
		views = views[:limit]
		last := views[len(views)-1]
		data, _ := json.Marshal(shareCursor{OwnerID: ownerID, Created: last.CreatedAt, ID: last.ID})
		payload := base64.RawURLEncoding.EncodeToString(data)
		mac := hmac.New(sha256.New, s.secret)
		mac.Write([]byte("share-page:" + payload))
		next = payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	return views, next, nil
}

// UpdateExpiry may renew an expired share, but never restores a revoked share.
// The version prevents a stale browser from silently overwriting another edit.
func (s *Service) UpdateExpiry(ctx context.Context, ownerID, shareID uuid.UUID, version int64, expiresAt *time.Time) error {
	if version < 1 || (expiresAt != nil && !expiresAt.After(time.Now().UTC())) {
		return ErrInvalidInput
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var share Share
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND owner_id = ?", shareID, ownerID).First(&share).Error; err != nil {
			return lookupError(err)
		}
		if share.RevokedAt != nil {
			return ErrNotFound
		}
		if share.Version != version {
			return ErrVersionConflict
		}
		if err := lockSource(tx, ownerID, share.FileEntryID); err != nil {
			return err
		}
		if err := tx.Model(&share).Updates(map[string]any{"expires_at": expiresAt, "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
		epoch, err := ownerepoch.BumpACL(tx, ownerID)
		if err != nil {
			return err
		}
		return outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
			EventType: eventcontract.EventShareUpdatedV1, AggregateID: share.ID, AggregateVersion: share.Version + 1,
			Producer: "core-api", Payload: eventcontract.ShareUpdatedV1{OwnerID: ownerID, ShareID: share.ID, ExpiresAt: expiresAt, ACLEpoch: epoch.ACLEpoch},
		})
	})
}
