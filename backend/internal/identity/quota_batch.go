package identity

import (
	"context"
	"math"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type quotaBatchKey struct{}

// BatchQuota defers release-only deltas until all compatible session changes
// have succeeded. Admission always executes immediately against the quota row.
func BatchQuota(ctx context.Context) context.Context {
	return context.WithValue(ctx, quotaBatchKey{}, map[uuid.UUID]int64{})
}

func FlushQuota(tx *gorm.DB) error {
	deltas, _ := tx.Statement.Context.Value(quotaBatchKey{}).(map[uuid.UUID]int64)
	for id, delta := range deltas {
		if err := changeQuota(tx.WithContext(context.WithValue(tx.Statement.Context, quotaBatchKey{}, nil)), id, delta, false); err != nil {
			return err
		}
	}
	return nil
}

func deferQuota(tx *gorm.DB, id uuid.UUID, delta int64) (bool, error) {
	deltas, _ := tx.Statement.Context.Value(quotaBatchKey{}).(map[uuid.UUID]int64)
	if deltas == nil || delta >= 0 {
		return false, nil
	}
	if deltas[id] < math.MinInt64-delta {
		return true, ErrConflict
	}
	deltas[id] += delta
	return true, nil
}
