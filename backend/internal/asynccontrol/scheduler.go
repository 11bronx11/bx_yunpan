package asynccontrol

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Reserve only claims a due publisher record. It reserves no execution slot,
// rotates no owner/priority, and never changes a task's processing state.
func (s *Store) Reserve(ctx context.Context, queue string, _ ...uuid.UUID) (t Task, ok bool, err error) {
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		q := tx.Raw(`SELECT t.* FROM tasks t JOIN task_deliveries d ON d.task_id=t.id AND d.generation=t.dispatch_generation
 WHERE t.protocol_version=2 AND t.state='pending' AND t.queue=? AND d.not_before<=clock_timestamp()
 AND (d.state='pending' OR (d.state='publishing' AND d.lease_until<=clock_timestamp()))
 AND t.dispatches<t.max_dispatches ORDER BY d.not_before,t.id LIMIT 1 FOR UPDATE OF t SKIP LOCKED`, queue).Scan(&t)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected == 0 {
			return nil
		}
		token := uuid.New()
		t.DeliveryToken = &token
		t.Dispatches++
		if err := tx.Exec("UPDATE tasks SET dispatches=dispatches+1,updated_at=clock_timestamp() WHERE id=?", t.ID).Error; err != nil {
			return err
		}
		if err := owned(tx.Exec(`UPDATE task_deliveries SET state='publishing',token=?,lease_until=clock_timestamp()+(?*interval '1 millisecond'),sends=sends+1,updated_at=clock_timestamp() WHERE task_id=? AND generation=?`, token, s.Lease.Milliseconds(), t.ID, t.DispatchGeneration)); err != nil {
			return err
		}
		ok = true
		return nil
	})
	return
}

func (s *Store) Published(ctx context.Context, t Task, cause error) error {
	if t.DeliveryToken == nil {
		return ErrInvalid
	}
	// A consumer may have claimed/completed already. The token condition makes
	// that race a harmless no-op, with no update to tasks.
	q := s.DB.WithContext(ctx).Exec(`UPDATE task_deliveries SET state=CASE WHEN ?='' THEN 'confirmed' ELSE 'pending' END,
 confirmed_at=CASE WHEN ?='' THEN clock_timestamp() ELSE confirmed_at END,
 not_before=clock_timestamp()+(?*interval '1 millisecond'),token=NULL,lease_until=NULL,last_error=?,updated_at=clock_timestamp()
 WHERE task_id=? AND generation=? AND token=? AND state='publishing'`, errorText(cause), errorText(cause), Backoff(t.Dispatches, s.RetryBase, s.RetryCap, 0).Milliseconds(), errorText(cause), t.ID, t.DispatchGeneration, *t.DeliveryToken)
	return q.Error
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Recovery is limited to expired leases, deadlines and exhausted sends. A
// confirmed queued notification is left to RabbitMQ until its explicit deadline.
func (s *Store) Recover(ctx context.Context, limit int) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []Task
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where(`protocol_version=2 AND
 ((state='running' AND lease_until<=clock_timestamp()) OR
 (state='pending' AND ((result_id IS NULL AND phase='compute' AND (deadline<=clock_timestamp() OR attempts>=max_attempts))
 OR (phase<>'compute' AND (commit_deadline<=clock_timestamp() OR (commit_deadline IS NULL AND deadline<=clock_timestamp())))
 OR EXISTS(SELECT 1 FROM task_deliveries d WHERE d.task_id=tasks.id AND d.state IN ('pending','publishing') AND (d.lease_until IS NULL OR d.lease_until<=clock_timestamp()) AND dispatches>=max_dispatches))))`).Order("updated_at,id").Limit(limit).Find(&rows).Error; err != nil {
			return err
		}
		var now time.Time
		if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return err
		}
		for _, t := range rows {
			if t.State == Running && t.Phase == "compute" {
				if err := tx.Exec("UPDATE executions SET finished_at=?,outcome='lease_lost' WHERE task_id=? AND generation=? AND finished_at IS NULL", now, t.ID, t.Generation).Error; err != nil {
					return err
				}
			}
			if t.ResultID != nil || Command(t.Kind) {
				if t.Phase == "reconcile" {
					if err := s.stop(tx, &t, "receipt reconciliation did not complete", now); err != nil {
						return err
					}
					continue
				}
				if t.CommitAttempts >= t.MaxCommitAttempts || t.CommitDeadline != nil && !now.Before(*t.CommitDeadline) || Command(t.Kind) && !now.Before(t.Deadline) {
					if err := s.reconcile(tx, &t, now, "commit lease or budget exhausted"); err != nil {
						return err
					}
					continue
				}
			} else if t.Attempts >= t.MaxAttempts || !now.Before(t.Deadline) {
				if err := s.stop(tx, &t, "execution lease or budget exhausted", now); err != nil {
					return err
				}
				continue
			}
			if t.Dispatches >= t.MaxDispatches {
				if err := s.stop(tx, &t, "publication budget exhausted", now); err != nil {
					return err
				}
				continue
			}
			if err := s.retry(tx, &t, now.Add(s.RetryBase), "execution lease expired"); err != nil {
				return err
			}
		}
		return nil
	})
}

// Used exclusively by historical import. Online commands never store a fake
// compute result. Imported accepted results retain their original identity.
func (s *Store) commandResult(tx *gorm.DB, t Task, body []byte, state string, now time.Time) error {
	digest, b, err := Digest(body)
	if err != nil {
		return err
	}
	id := uuid.NewSHA1(t.ID, []byte(fmt.Sprintf("command:%d:%s", t.Generation, state)))
	r := Result{ID: id, TaskID: t.ID, Generation: t.Generation, InputDigest: t.InputDigest, ResultDigest: digest, Body: b, CreatedAt: now, EffectTarget: t.EffectTarget}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&r).Error; err != nil {
		return err
	}
	return tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"result_id": id, "phase": "commit", "commit_deadline": now.Add(s.CommitWindow)}).Error
}
