package storageops

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

var ErrLeaseLost = errors.New("storage operation lease lost")
var errFinished = errors.New("storage operation finished")

// RunOwned renews a storage lease for the entire provider call. Task receipt
// leases protect a TaskID; a recovery event with a new ID needs this lease too.
func RunOwned(ctx context.Context, db *gorm.DB, op Operation, lease time.Duration, fn func(context.Context) error) error {
	if op.LeaseToken == nil || lease <= 0 {
		return ErrInvalidOperation
	}
	ctx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			renew, stop := context.WithTimeout(ctx, min(5*time.Second, lease/6))
			q := db.WithContext(renew).Model(&Operation{}).
				Where("id = ? AND state = ? AND lease_token = ? AND lease_until > clock_timestamp()", op.ID, StateRunning, *op.LeaseToken).
				Updates(map[string]any{"lease_until": gorm.Expr("clock_timestamp() + (? * interval '1 millisecond')", lease.Milliseconds()), "updated_at": gorm.Expr("clock_timestamp()")})
			if q.Error == nil && q.RowsAffected == 0 {
				current, err := Get(db.WithContext(renew), op.ID)
				if err == nil && current.State == StateSucceeded {
					stop()
					return
				}
			}
			stop()
			if ctx.Err() != nil {
				return
			}
			if q.Error != nil || q.RowsAffected != 1 {
				cancel(errors.Join(ErrLeaseLost, q.Error))
				return
			}
		}
	}()
	defer func() { cancel(errFinished); <-done }()
	err := fn(ctx)
	cancel(errFinished)
	<-done
	if cause := context.Cause(ctx); !errors.Is(cause, errFinished) {
		return errors.Join(err, cause)
	}
	return err
}

// RecordError persists failure even when shutdown canceled the provider call.
// A failed database write must remain visible to the caller.
func RecordError(ctx context.Context, db *gorm.DB, op Operation, cause error) error {
	if op.LeaseToken == nil {
		return errors.Join(cause, ErrInvalidOperation)
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if errors.Is(ctx.Err(), context.Canceled) {
		q := db.WithContext(persist).Model(&Operation{}).
			Where("id = ? AND state = ? AND lease_token = ?", op.ID, StateRunning, *op.LeaseToken).
			Updates(map[string]any{"state": StatePending, "attempt": gorm.Expr("GREATEST(0, attempt - 1)"), "available_at": gorm.Expr("clock_timestamp()"), "lease_until": nil, "lease_token": nil, "last_error": cause.Error()})
		if q.Error != nil {
			return errors.Join(cause, q.Error)
		}
		if q.RowsAffected != 1 {
			return errors.Join(cause, ErrLeaseLost)
		}
		return cause
	}
	return errors.Join(cause, MarkFailedOwned(db.WithContext(persist), op.ID, *op.LeaseToken, cause, time.Second))
}
