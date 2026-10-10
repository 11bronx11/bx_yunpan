package taskqueue

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Results suppress redelivery after a recorded success and keep terminal
// failures terminal. They supplement each business handler's transactional
// idempotency/lease: the side-effect/receipt crash window still allows replay.
type Result struct {
	TaskID     string `gorm:"primaryKey"`
	Status     string
	FinishedAt time.Time
}

func (Result) TableName() string { return "task_results" }

type Failure struct {
	TaskID     string `gorm:"primaryKey"`
	TaskType   string
	Queue      string
	Payload    []byte
	RetryCount int
	LastError  string
	FailedAt   time.Time
}

func (Failure) TableName() string { return "task_failures" }

type Store struct {
	db          *gorm.DB
	stopTimeout time.Duration
}

func NewStore(db *gorm.DB) *Store { return &Store{db: db, stopTimeout: 10 * time.Second} }
func (s *Store) WithStopTimeout(d time.Duration) *Store {
	if d > 0 {
		s.stopTimeout = d
	}
	return s
}
func insertSuccess(tx *gorm.DB, id string, now time.Time) error {
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&Result{TaskID: id, Status: "succeeded", FinishedAt: now}).Error
}
func recordFailure(tx *gorm.DB, t Task, cause error, now time.Time) error {
	var result Result
	q := tx.Where("task_id = ?", t.ID).Find(&result)
	if q.Error != nil {
		return q.Error
	}
	if q.RowsAffected > 0 && result.Status == "succeeded" {
		return nil
	}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&Failure{TaskID: t.ID, TaskType: t.Kind, Queue: t.Queue, Payload: t.Body, RetryCount: t.RetryCount, LastError: cause.Error(), FailedAt: now}).Error; err != nil {
		return err
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&Result{TaskID: t.ID, Status: "dead", FinishedAt: now}).Error
}
func (s *Store) RecordFailure(ctx context.Context, t Task, cause error) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 741823))", t.ID).Error; err != nil {
			return err
		}
		return recordFailure(tx, t, cause, time.Now().UTC())
	})
}
func (s *Store) Middleware() MiddlewareFunc {
	return func(next Handler) Handler {
		return HandlerFunc(func(ctx context.Context, t *Task) error {
			claimCtx, stopClaim := context.WithTimeout(ctx, 5*time.Second)
			claimErr := s.claim(claimCtx, t)
			stopClaim()
			if err := claimErr; err != nil {
				if errors.Is(err, errTaskSucceeded) {
					return nil
				}
				if errors.Is(err, ErrTaskBusy) || errors.Is(err, ErrTaskConflict) || errors.Is(err, ErrTaskDead) {
					return err
				}
				return errors.Join(ErrTaskStorage, err)
			}
			timed, stopTimed := context.WithTimeout(WithMetadata(ctx, *t), t.Timeout)
			defer stopTimed()
			handlerCtx, cancel := context.WithCancelCause(timed)
			renewed := make(chan struct{})
			go s.renew(handlerCtx, *t, cancel, renewed)
			err := invokeBounded(handlerCtx, t, next.ProcessTask, s.stopTimeout, func() {
				persist, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer stop()
				if e := s.recordStuck(persist, t, context.Cause(handlerCtx)); e != nil {
					slog.Error("record stuck task before process exit", "task_id", t.ID, "error", e)
				}
			})
			if err == nil && handlerCtx.Err() != nil {
				err = context.Cause(handlerCtx)
			}
			cancel(nil)
			<-renewed
			if errors.Is(context.Cause(handlerCtx), ErrLeaseLost) {
				return ErrLeaseLost
			}
			persist, stopPersist := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer stopPersist()
			if finishErr := s.finish(persist, t, err); finishErr != nil {
				return errors.Join(ErrTaskStorage, finishErr)
			}
			return err
		})
	}
}
