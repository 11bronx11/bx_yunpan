package upload

import (
	"context"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Advance past broken rows even when an item fails before updating its age.
// Each periodic work class owns a cursor; concurrent replicas remain protected
// by the session locks in the actual state transitions.
func (s *Service) maintenancePage(ctx context.Context, kind string, candidates *gorm.DB, limit int, rows *[]Session) error {
	if limit <= 0 {
		return nil
	}
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	if s.scanAfter == nil {
		s.scanAfter = make(map[string]uuid.UUID)
	}
	query := func(after uuid.UUID) error {
		q := candidates.WithContext(ctx).Session(&gorm.Session{}).Order("id ASC").Limit(limit)
		if after != uuid.Nil {
			q = q.Where("id > ?", after)
		}
		return q.Find(rows).Error
	}
	cursor := s.scanAfter[kind]
	if err := query(cursor); err != nil {
		return err
	}
	if len(*rows) == 0 && cursor != uuid.Nil {
		if err := query(uuid.Nil); err != nil {
			return err
		}
	}
	if len(*rows) > 0 {
		s.scanAfter[kind] = (*rows)[len(*rows)-1].ID
	} else {
		delete(s.scanAfter, kind)
	}
	return nil
}
