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
var ErrInactive = errors.New("inactive shares are read-only")

type ShareView struct {
	Share
	FileAvailable bool
	Status        string
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
	return db.Table("shares s").Select(`s.*, (s.source_deleted_at IS NULL) AS file_available,
		CASE WHEN s.source_deleted_at IS NOT NULL THEN 'source_deleted'
		WHEN s.revoked_at IS NOT NULL THEN 'revoked'
		WHEN s.expires_at <= statement_timestamp() THEN 'expired' ELSE 'active' END AS status`).
		Where("s.owner_id = ?", ownerID)
}

// All source-dependent mutations take locks in the same order as deletion:
// source entry first, then share. The first lookup only locates immutable IDs.
func lockShareForMutation(tx *gorm.DB, ownerID, shareID uuid.UUID) (Share, error) {
	var share Share
	if err := tx.Where("id = ? AND owner_id = ?", shareID, ownerID).First(&share).Error; err != nil {
		return Share{}, lookupError(err)
	}
	if share.SourceDeletedAt != nil {
		return Share{}, ErrInactive
	}
	if err := lockSource(tx, ownerID, share.FileEntryID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return Share{}, ErrInactive
		}
		return Share{}, err
	}
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND owner_id = ?", shareID, ownerID).First(&share).Error; err != nil {
		return Share{}, lookupError(err)
	}
	return share, nil
}

func (s *Service) GetView(ctx context.Context, ownerID, shareID uuid.UUID) (ShareView, error) {
	var view ShareView
	err := viewQuery(s.db.WithContext(ctx), ownerID).Where("s.id = ?", shareID).Take(&view).Error
	return view, lookupError(err)
}

type shareCursor struct {
	OwnerID uuid.UUID `json:"owner"`
	Status  string    `json:"status"`
	Created time.Time `json:"created"`
	ID      uuid.UUID `json:"id"`
}

func (s *Service) ListPage(ctx context.Context, ownerID uuid.UUID, status, cursor string, limit int) ([]ShareView, string, error) {
	if limit < 1 || limit > 100 {
		return nil, "", ErrInvalidInput
	}
	query := viewQuery(s.db.WithContext(ctx), ownerID)
	switch status {
	case "active":
		query = query.Where("s.source_deleted_at IS NULL AND s.revoked_at IS NULL AND (s.expires_at IS NULL OR s.expires_at > statement_timestamp())")
	case "expired":
		query = query.Where("s.source_deleted_at IS NULL AND s.revoked_at IS NULL AND s.expires_at <= statement_timestamp()")
	case "revoked":
		query = query.Where("s.source_deleted_at IS NULL AND s.revoked_at IS NOT NULL")
	case "source_deleted":
		query = query.Where("s.source_deleted_at IS NOT NULL")
	default:
		return nil, "", ErrInvalidInput
	}
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
		if sigErr != nil || dataErr != nil || !hmac.Equal(sig, mac.Sum(nil)) || json.Unmarshal(data, &after) != nil || after.OwnerID != ownerID || after.Status != status || after.ID == uuid.Nil || after.Created.IsZero() {
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
		data, _ := json.Marshal(shareCursor{OwnerID: ownerID, Status: status, Created: last.CreatedAt, ID: last.ID})
		payload := base64.RawURLEncoding.EncodeToString(data)
		mac := hmac.New(sha256.New, s.secret)
		mac.Write([]byte("share-page:" + payload))
		next = payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	return views, next, nil
}

// UpdateExpiry only changes active shares. Expired/revoked shares are read-only.
// The version prevents a stale browser from silently overwriting another edit.
func (s *Service) UpdateExpiry(ctx context.Context, ownerID, shareID uuid.UUID, version int64, expiresAt *time.Time) error {
	if version < 1 {
		return ErrInvalidInput
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		share, err := lockShareForMutation(tx, ownerID, shareID)
		if err != nil {
			return err
		}
		epoch, err := ownerepoch.BumpACL(tx, ownerID)
		if err != nil {
			return err
		}
		now, err := databaseNow(tx)
		if err != nil {
			return err
		}
		if validateShareAt(share, now) != nil {
			return ErrInactive
		}
		if share.Version != version {
			return ErrVersionConflict
		}
		if expiresAt != nil && !expiresAt.After(now) {
			return ErrInvalidInput
		}
		query := tx.Model(&share).Where("source_deleted_at IS NULL AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > clock_timestamp())")
		if expiresAt != nil {
			query = query.Where("? > clock_timestamp()", *expiresAt)
		}
		result := query.Updates(map[string]any{"expires_at": expiresAt, "version": gorm.Expr("version + 1"), "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrInactive
		}
		return outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
			EventType: eventcontract.EventShareUpdatedV1, AggregateID: share.ID, AggregateVersion: share.Version + 1,
			Producer: "core-api", Payload: eventcontract.ShareUpdatedV1{OwnerID: ownerID, ShareID: share.ID, ExpiresAt: expiresAt, ACLEpoch: epoch.ACLEpoch},
		})
	})
}
