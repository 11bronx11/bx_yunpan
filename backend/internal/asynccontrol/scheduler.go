package asynccontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Budget struct {
	Queue                                 string
	InflightLimit, OwnerLimit, ResultHigh int
	FairCursor                            string
	PriorityTurn                          int64
	PausedUntil                           time.Time
	Backpressured                         bool
	RecoveryAfter                         time.Time
}

func (Budget) TableName() string { return "queue_budgets" }

// Reserve serializes each queue's accounting across all scheduler instances.
// Reserved deliveries occupy the same global budget as running work.
func (s *Store) Reserve(ctx context.Context, queue string, moduleToken ...uuid.UUID) (Task, bool, error) {
	var t Task
	ok := false
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var b Budget
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&b, "queue=?", queue).Error; err != nil {
			return err
		}
		var active, pending int64
		if err := tx.Model(&Task{}).Where("queue=? AND state IN ?", queue, []string{Running, Dispatched}).Count(&active).Error; err != nil {
			return err
		}
		if active >= int64(b.InflightLimit) {
			return nil
		}
		if err := tx.Model(&Task{}).Where("queue=? AND state IN ?", queue, []string{Computed, Converging}).Count(&pending).Error; err != nil {
			return err
		}
		if pending >= int64(b.ResultHigh) {
			return tx.Model(&Budget{}).Where("queue=?", queue).Updates(map[string]any{"backpressured": true, "recovery_after": gorm.Expr("clock_timestamp()+interval '10 seconds'")}).Error
		}
		if b.Backpressured {
			var recovered bool
			if err := tx.Raw("SELECT clock_timestamp()>=?", b.RecoveryAfter).Scan(&recovered).Error; err != nil {
				return err
			}
			if pending > int64(b.ResultHigh/2) || !recovered {
				return nil
			}
			if err := tx.Model(&Budget{}).Where("queue=?", queue).Update("backpressured", false).Error; err != nil {
				return err
			}
		}
		if pending > 0 {
			var stale bool
			if err := tx.Raw(`SELECT EXISTS(SELECT 1 FROM tasks WHERE queue=? AND state IN ('computed','convergence_pending') AND commit_deadline<clock_timestamp()+(?*interval '1 millisecond') LIMIT 1)`, queue, (s.CommitWindow - time.Minute).Milliseconds()).Scan(&stale).Error; err != nil {
				return err
			}
			if stale {
				return tx.Model(&Budget{}).Where("queue=?", queue).Updates(map[string]any{"backpressured": true, "recovery_after": gorm.Expr("clock_timestamp()+interval '10 seconds'")}).Error
			}
		}
		// Owner rotation happens before per-owner selection. A saturated owner is
		// removed in SQL so it cannot occupy the bounded candidate window forever.
		var owner struct{ OwnerKey string }
		q := tx.Raw(`SELECT t.owner_key FROM tasks t WHERE t.queue=? AND t.state IN ('ready','retry_wait')
   AND t.not_before<=clock_timestamp() AND t.deadline>clock_timestamp() AND t.attempts<t.max_attempts AND t.dispatches<t.max_dispatches
   AND (SELECT count(*) FROM tasks a WHERE a.queue=t.queue AND a.owner_key=t.owner_key AND a.state IN ('running','dispatched')) < ?
   AND NOT EXISTS(SELECT 1 FROM queue_budgets WHERE queue=? AND paused_until>clock_timestamp())
   GROUP BY t.owner_key ORDER BY (t.owner_key>?) DESC,t.owner_key LIMIT 1`, queue, b.OwnerLimit, queue, b.FairCursor).Scan(&owner)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected == 0 {
			return nil
		}
		priority := "priority ASC"
		if b.PriorityTurn%5 == 4 {
			priority = "priority DESC"
		}
		q = tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("queue=? AND owner_key=? AND state IN ? AND not_before<=clock_timestamp() AND deadline>clock_timestamp() AND attempts<max_attempts AND dispatches<max_dispatches", queue, owner.OwnerKey, []string{Ready, RetryWait}).Order(priority).Order("not_before,id").Limit(1).Find(&t)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected == 0 {
			return nil
		}
		t.DispatchGeneration++
		t.Dispatches++
		t.State = Dispatched
		if err := tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"state": Dispatched, "dispatch_generation": t.DispatchGeneration, "dispatches": t.Dispatches, "lease_until": gorm.Expr("clock_timestamp()+(?*interval '1 millisecond')", s.Lease.Milliseconds()), "version": gorm.Expr("version+1"), "updated_at": gorm.Expr("clock_timestamp()")}).Error; err != nil {
			return err
		}
		if err := tx.Model(&Budget{}).Where("queue=?", queue).Updates(map[string]any{"fair_cursor": t.OwnerKey, "priority_turn": gorm.Expr("priority_turn+1")}).Error; err != nil {
			return err
		}
		if len(moduleToken) > 0 {
			var live int64
			if err := tx.Raw("SELECT count(*) FROM module_leases WHERE name='scheduler' AND token=? AND lease_until>clock_timestamp()", moduleToken[0]).Scan(&live).Error; err != nil {
				return err
			}
			if live != 1 {
				return ErrLost
			}
		}
		ok = true
		return nil
	})
	return t, ok, err
}

// Recover touches only indexed active/due states; successful history is never
// consulted to resurrect work. The final crashed attempt reaches exhaustion.
func (s *Store) Recover(ctx context.Context, limit int) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []Task
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where(`(state IN ('running','dispatched') AND lease_until<=clock_timestamp()) OR
   (state IN ('ready','retry_wait') AND (deadline<=clock_timestamp() OR attempts>=max_attempts OR dispatches>=max_dispatches)) OR state='exhausted'`).Order("updated_at,id").Limit(limit).Find(&rows).Error; err != nil {
			return err
		}
		for _, t := range rows {
			var now time.Time
			if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
				return err
			}
			exhausted := t.State == Exhausted || t.Attempts >= t.MaxAttempts || t.Dispatches >= t.MaxDispatches || t.Recoveries >= t.MaxRecoveries || !now.Before(t.Deadline)
			if !exhausted {
				if err := tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"state": RetryWait, "recoveries": gorm.Expr("recoveries+1"), "token": nil, "lease_until": nil, "not_before": now.Add(Backoff(t.Recoveries+1, s.RetryBase, s.RetryCap, rand.Float64()*2-1)), "last_error": "delivery or execution lease expired", "version": gorm.Expr("version+1"), "updated_at": now}).Error; err != nil {
					return err
				}
				if t.State == Running {
					if err := tx.Exec("UPDATE executions SET finished_at=?,outcome='lease_lost' WHERE task_id=? AND generation=? AND finished_at IS NULL", now, t.ID, t.Generation).Error; err != nil {
						return err
					}
				}
				continue
			}
			if t.State == Running {
				if err := tx.Exec("UPDATE executions SET finished_at=?,outcome='exhausted' WHERE task_id=? AND generation=? AND finished_at IS NULL", now, t.ID, t.Generation).Error; err != nil {
					return err
				}
			}
			d, known := s.Registry[t.Kind]
			state := NeedsAttention
			if known && d.Converge {
				body, _ := json.Marshal(map[string]any{"convergence": true, "error": t.LastError, "attempts": t.Attempts})
				if err := s.commandResult(tx, t, body, Converging, now); err != nil {
					return err
				}
			} else {
				if err := tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"state": state, "token": nil, "lease_until": nil, "stopped_at": now, "updated_at": now, "version": gorm.Expr("version+1")}).Error; err != nil {
					return err
				}
			}
			slog.Error("async work automatic execution stopped", "task_id", t.ID, "semantic_key", t.SemanticKey, "resource", t.ResourceKey, "generation", t.Generation, "attempts", t.Attempts, "max_attempts", t.MaxAttempts, "dispatches", t.Dispatches, "max_dispatches", t.MaxDispatches, "recoveries", t.Recoveries, "max_recoveries", t.MaxRecoveries, "started_at", t.StartedAt, "deadline", t.Deadline, "last_error", t.LastError, "trace", string(t.Trace))
		}
		return nil
	})
}

func (s *Store) commandResult(tx *gorm.DB, t Task, body []byte, state string, now time.Time) error {
	digest, b, err := Digest(body)
	if err != nil {
		return err
	}
	id := uuid.NewSHA1(t.ID, []byte(fmt.Sprintf("command:%d:%s", t.Generation, state)))
	r := Result{ID: id, TaskID: t.ID, Generation: t.Generation, InputDigest: t.InputDigest, ResultDigest: digest, Body: b, CreatedAt: now}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&r).Error; err != nil {
		return err
	}
	return tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"state": state, "result_id": id, "token": nil, "lease_until": nil, "not_before": now, "commit_deadline": now.Add(s.CommitWindow), "version": gorm.Expr("version+1"), "updated_at": now}).Error
}

func (s *Store) PrepareCommands(ctx context.Context, limit int) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var ts []Task
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("queue='commit' AND state IN ? AND not_before<=clock_timestamp()", []string{Ready, RetryWait}).Order("not_before,id").Limit(limit).Find(&ts).Error; err != nil {
			return err
		}
		var now time.Time
		if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return err
		}
		for _, t := range ts {
			if err := s.commandResult(tx, t, []byte(`{"command":true}`), Computed, now); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) NextResult(ctx context.Context) (Task, bool, error) {
	var t Task
	ok := false
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("INSERT INTO control_settings(key,value) VALUES('result_turn','0') ON CONFLICT DO NOTHING").Error; err != nil {
			return err
		}
		var turn int64
		if err := tx.Raw("UPDATE control_settings SET value=(value::bigint+1)::text WHERE key='result_turn' RETURNING value::bigint").Scan(&turn).Error; err != nil {
			return err
		}
		order := "priority ASC"
		if turn%5 == 0 {
			order = "priority DESC"
		}
		q := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("state IN ? AND not_before<=clock_timestamp()", []string{Computed, Converging}).Order(order).Order("not_before,id").Limit(1).Find(&t)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected == 0 {
			return nil
		}
		var now time.Time
		if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return err
		}
		if t.ResultSends >= t.MaxResultSends || t.CommitDeadline == nil || !now.Before(*t.CommitDeadline) {
			if err := tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"state": NeedsAttention, "stopped_at": now, "last_error": "result submission recovery exhausted", "updated_at": now}).Error; err != nil {
				return err
			}
			slog.Error("async result recovery stopped; business effect needs attention", "task_id", t.ID, "semantic_key", t.SemanticKey, "result_id", t.ResultID, "sends", t.ResultSends)
			return nil
		}
		t.ResultSends++
		// Durable retry reservation also covers publisher success/confirm-loss windows.
		if err := tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"result_sends": t.ResultSends, "not_before": now.Add(Backoff(t.ResultSends, s.RetryBase, s.RetryCap, rand.Float64()*2-1)), "updated_at": now}).Error; err != nil {
			return err
		}
		ok = true
		return nil
	})
	return t, ok, err
}
