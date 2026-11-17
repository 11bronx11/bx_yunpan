package upload

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func expectedPartSize(session Session, number int) int64 {
	if number == session.PartCount {
		return session.SizeBytes - int64(number-1)*session.PartSize
	}
	return session.PartSize
}

func samePart(a, b Part) bool {
	return a.ETag == b.ETag && a.SizeBytes == b.SizeBytes &&
		((a.Checksum == nil && b.Checksum == nil) || (a.Checksum != nil && b.Checksum != nil && *a.Checksum == *b.Checksum))
}

// ConfirmParts retains the full-session interface used by existing callers.
func (s *Service) ConfirmParts(userID, sessionID uuid.UUID, parts []ConfirmedPart) (Session, error) {
	return s.ConfirmPartsContext(context.Background(), userID, sessionID, parts, false)
}

// In delta mode ConfirmedParts contains only this request's acknowledged parts.
// The session row is the common fence for confirmation, completion and abort.
func (s *Service) ConfirmPartsContext(ctx context.Context, userID, sessionID uuid.UUID, parts []ConfirmedPart, delta bool) (Session, error) {
	if len(parts) == 0 || len(parts) > 100 {
		return Session{}, ErrInvalidInput
	}
	var session Session
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", sessionID, userID).Take(&session).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		writable := session.Status == StatusCreated || session.Status == StatusUploading
		frozen := session.Status == StatusCompleting || session.Status == StatusVerifying || session.Status == StatusCompleted
		if (!writable && !frozen) || (writable && !session.ExpiresAt.After(time.Now())) {
			return ErrConflict
		}
		requested := make(map[int]Part, len(parts))
		numbers := make([]int, 0, len(parts))
		for _, p := range parts {
			p.ETag = strings.Trim(strings.TrimSpace(p.ETag), "\"")
			if p.PartNumber < 1 || p.PartNumber > session.PartCount || p.ETag == "" || len(p.ETag) > 512 || p.SizeBytes <= 0 || p.SizeBytes != expectedPartSize(session, p.PartNumber) || (p.Checksum != nil && len(*p.Checksum) > 512) {
				return ErrInvalidInput
			}
			part := Part{SessionID: sessionID, PartNumber: p.PartNumber, ETag: p.ETag, SizeBytes: p.SizeBytes, Checksum: p.Checksum, CompletedAt: time.Now().UTC()}
			if prior, ok := requested[p.PartNumber]; ok {
				if !samePart(prior, part) {
					return ErrInvalidInput
				}
				continue
			}
			requested[p.PartNumber] = part
			numbers = append(numbers, p.PartNumber)
		}
		sort.Ints(numbers)
		var existing []Part
		if err := tx.Where("session_id = ? AND part_number IN ?", sessionID, numbers).Find(&existing).Error; err != nil {
			return err
		}
		for _, part := range existing {
			if !samePart(requested[part.PartNumber], part) {
				return ErrPartConflict
			}
			delete(requested, part.PartNumber)
		}
		if frozen && len(requested) != 0 {
			return ErrConflict
		}
		newParts := make([]Part, 0, len(requested))
		for _, number := range numbers {
			if p, ok := requested[number]; ok {
				newParts = append(newParts, p)
			}
		}
		if len(newParts) > 0 {
			if err := tx.Create(&newParts).Error; err != nil {
				return err
			}
		}
		if session.Status == StatusCreated {
			if err := tx.Model(&Session{}).Where("id = ?", sessionID).Update("status", StatusUploading).Error; err != nil {
				return err
			}
			session.Status = StatusUploading
		}
		session.ConfirmedParts = append(existing, newParts...)
		sort.Slice(session.ConfirmedParts, func(i, j int) bool {
			return session.ConfirmedParts[i].PartNumber < session.ConfirmedParts[j].PartNumber
		})
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	if !delta {
		return s.GetContext(ctx, userID, sessionID)
	}
	return session, nil
}
