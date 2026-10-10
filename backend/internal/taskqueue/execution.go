package taskqueue

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrTaskBusy = errors.New("taskqueue: execution already owned or retry not due")
var ErrTaskDead = fmt.Errorf("taskqueue: terminal execution: %w", SkipRetry)
var ErrTaskConflict = errors.New("taskqueue: task identity reused with different payload")
var ErrLeaseLost = errors.New("taskqueue: execution lease lost")
var ErrTaskStorage = errors.New("taskqueue: execution persistence failed")
var errTaskSucceeded = errors.New("taskqueue: already succeeded")

type deliveryContextKey struct{}

func (s *Store) leaseDuration() time.Duration { return 3 * (s.stopTimeout + 5*time.Second) }

// renew cancels the handler before an unrenewable lease can be taken over.
// The broker's cancellation grace is shorter than the remaining lease.
func (s *Store) renew(ctx context.Context, t Task, cancel context.CancelCauseFunc, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(s.leaseDuration() / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		renewCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		q := s.db.WithContext(renewCtx).Model(&Execution{}).Where("task_id=? AND status='running' AND lease_token=? AND lease_until>clock_timestamp()", t.ID, t.leaseToken).Updates(map[string]any{"lease_until": gorm.Expr("clock_timestamp() + (? * interval '1 millisecond')", s.leaseDuration().Milliseconds()), "updated_at": gorm.Expr("clock_timestamp()")})
		stop()
		if ctx.Err() != nil {
			return
		}
		if q.Error != nil || q.RowsAffected != 1 {
			cancel(errors.Join(ErrLeaseLost, q.Error))
			return
		}
	}
}

type Execution struct {
	TaskID        string `gorm:"primaryKey"`
	PayloadSHA256 string `gorm:"column:payload_sha256"`
	TaskType      string
	Queue         string
	Status        string
	LeaseToken    *uuid.UUID `gorm:"type:uuid"`
	LeaseUntil    *time.Time
	AvailableAt   time.Time
	RetryCount    int
	MaxRetry      int
	TimeoutMS     int64 `gorm:"column:timeout_ms"`
	UpdatedAt     time.Time
}

func (Execution) TableName() string { return "task_executions" }

func fingerprint(t *Task) string {
	h := sha256.New()
	_, _ = h.Write([]byte(t.Kind))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(t.Queue))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(t.Body)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// claim serializes duplicate deliveries for one TaskID while keeping the
// lease in PostgreSQL so a crashed process can be replaced safely.
func (s *Store) claim(ctx context.Context, t *Task) error {
	if t.Timeout <= 0 {
		t.Timeout = 30 * time.Minute
	}
	token := uuid.New()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 741823))", t.ID).Error; err != nil {
			return err
		}
		var now time.Time
		if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return err
		}
		var existing Execution
		identity := tx.Where("task_id=?", t.ID).Find(&existing)
		if identity.Error != nil {
			return identity.Error
		}
		if identity.RowsAffected > 0 && existing.PayloadSHA256 != fingerprint(t) {
			return ErrTaskConflict
		}
		var result Result
		q := tx.Where("task_id = ?", t.ID).Find(&result)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected > 0 {
			if result.Status == "succeeded" {
				return errTaskSucceeded
			}
			return ErrTaskDead
		}
		var row Execution
		q = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("task_id = ?", t.ID).First(&row)
		if q.Error != nil && !errors.Is(q.Error, gorm.ErrRecordNotFound) {
			return q.Error
		}
		if errors.Is(q.Error, gorm.ErrRecordNotFound) {
			row = Execution{TaskID: t.ID, PayloadSHA256: fingerprint(t), TaskType: t.Kind, Queue: t.Queue,
				Status: "running", LeaseToken: &token, LeaseUntil: ptrTime(now.Add(s.leaseDuration())),
				AvailableAt: now, RetryCount: t.RetryCount, MaxRetry: t.MaxRetry,
				TimeoutMS: t.Timeout.Milliseconds(), UpdatedAt: now}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		} else {
			if row.PayloadSHA256 != fingerprint(t) {
				return ErrTaskConflict
			}
			if row.Status == "running" && row.LeaseUntil != nil && row.LeaseUntil.After(now) {
				return ErrTaskBusy
			}
			if row.Status == "retry" && row.AvailableAt.After(now) {
				return ErrTaskBusy
			}
			if row.Status == "finished" {
				return ErrTaskDead
			}
			row.Status = "running"
			row.LeaseToken = &token
			row.LeaseUntil = ptrTime(now.Add(s.leaseDuration()))
			row.UpdatedAt = now
			if err := tx.Save(&row).Error; err != nil {
				return err
			}
		}
		t.leaseToken = token
		t.RetryCount = row.RetryCount
		t.MaxRetry = row.MaxRetry
		t.Timeout = time.Duration(row.TimeoutMS) * time.Millisecond
		return nil
	})
}
func ptrTime(t time.Time) *time.Time { return &t }

func (s *Store) finish(ctx context.Context, t *Task, cause error) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 741823))", t.ID).Error; err != nil {
			return err
		}
		var now time.Time
		if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return err
		}
		updates := map[string]any{"status": "finished", "lease_token": nil, "lease_until": nil, "updated_at": now}
		shutdown := false
		if parent, ok := ctx.Value(deliveryContextKey{}).(context.Context); ok {
			shutdown = parent.Err() != nil
		}
		terminal := cause != nil && !shutdown && (errors.Is(cause, SkipRetry) || t.RetryCount >= t.MaxRetry)
		if cause != nil && !terminal {
			count := t.RetryCount
			available := now
			if !shutdown {
				count++
				available = now.Add(retryDelay(count))
			}
			updates["status"] = "retry"
			updates["retry_count"] = count
			updates["available_at"] = available
		}
		q := tx.Model(&Execution{}).Where("task_id=? AND status='running' AND lease_token=? AND lease_until>clock_timestamp()", t.ID, t.leaseToken).Updates(updates)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected != 1 {
			return ErrLeaseLost
		}
		if cause == nil {
			return insertSuccess(tx, t.ID, now)
		}
		if terminal {
			return recordFailure(tx, *t, cause, now)
		}
		return nil
	})
}

// Keep the running lease during the final persistence/exit window: another
// worker must not execute while the stuck goroutine is still alive.
func (s *Store) recordStuck(ctx context.Context, t *Task, cause error) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 741823))", t.ID).Error; err != nil {
			return err
		}
		var now time.Time
		if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return err
		}
		shutdown := false
		if parent, ok := ctx.Value(deliveryContextKey{}).(context.Context); ok {
			shutdown = parent.Err() != nil
		}
		count := t.RetryCount
		terminal := !shutdown && !errors.Is(cause, ErrLeaseLost) && count >= t.MaxRetry
		if !shutdown && !errors.Is(cause, ErrLeaseLost) && !terminal {
			count++
		}
		delay := retryDelay(count) + 5*time.Second
		q := tx.Model(&Execution{}).Where("task_id=? AND status='running' AND lease_token=? AND lease_until>clock_timestamp()", t.ID, t.leaseToken).Updates(map[string]any{"retry_count": count, "available_at": now.Add(delay), "lease_until": now.Add(delay), "updated_at": now})
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected != 1 {
			return ErrLeaseLost
		}
		if terminal {
			return recordFailure(tx, *t, cause, now)
		}
		return nil
	})
}
