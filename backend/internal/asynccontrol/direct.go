package asynccontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Completion contains a durable domain decision, never a computation payload.
// Refs are bounded version identities needed by trusted continuation planners.
type Completion struct {
	TaskID          uuid.UUID       `json:"task_id"`
	InputDigest     string          `json:"input_digest"`
	ContractVersion int             `json:"contract_version"`
	Outcome         string          `json:"outcome"`
	Refs            json.RawMessage `json:"refs"`
}

func (c Completion) Validate(t Task) error {
	if c.TaskID != t.ID || c.InputDigest != t.InputDigest || c.ContractVersion != t.ContractVersion || len(c.Refs) > 64<<10 {
		return ErrConflict
	}
	switch c.Outcome {
	case Succeeded, Superseded, Rejected, Failed:
	default:
		return ErrInvalid
	}
	if len(c.Refs) > 0 && !json.Valid(c.Refs) {
		return ErrInvalid
	}
	return nil
}

// DirectHandler reads and commits domain-local receipts. Lookup must be read-only.
type DirectHandler interface {
	Lookup(context.Context, Task) (Completion, bool, error)
	Execute(context.Context, Task) (Completion, *time.Time, error)
}

func (s *Store) claimDirect(ctx context.Context, n Notice) (t Task, claimed bool, err error) {
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		q := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", n.TaskID).Find(&t)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected != 1 {
			return ErrInvalid
		}
		if n.Version != 3 || !n.validWire() || !n.matches(t) || t.ProtocolVersion != 3 {
			return ErrConflict
		}
		if Terminal(t.State) || n.DispatchGeneration != t.DispatchGeneration || t.State == Running {
			return nil
		}
		if t.State != Pending {
			return ErrInvalid
		}
		var now time.Time
		if e := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; e != nil {
			return e
		}
		if now.Before(t.NotBefore) {
			return tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"dispatch_state": "pending", "dispatch_not_before": t.NotBefore, "publish_token": nil, "publish_lease_until": nil}).Error
		}
		if t.Phase == "reconcile" {
			if t.ReceiptCheckClaimed || t.ReceiptCheckDeadline == nil || !now.Before(*t.ReceiptCheckDeadline) {
				return s.stopDirect(tx, &t, "receipt check exhausted", now)
			}
			t.ReceiptCheckClaimed = true
		} else if !now.Before(t.Deadline) || t.Attempts >= t.MaxAttempts {
			return s.reconcileDirect(tx, &t, now, "execution budget exhausted")
		} else {
			t.Attempts++
			t.Generation++
			t.Phase = "execute"
		}
		token := uuid.New()
		lease := minTime(now.Add(s.Lease), now.Add(time.Duration(t.TimeoutMS)*time.Millisecond))
		if t.Phase == "execute" {
			lease = minTime(lease, t.Deadline)
		} else {
			lease = minTime(lease, *t.ReceiptCheckDeadline)
		}
		t.Token = &token
		t.LeaseUntil = &lease
		t.StartedAt = &now
		t.State = Running
		if e := tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"state": Running, "phase": t.Phase, "generation": t.Generation, "attempts": t.Attempts, "token": token, "lease_until": lease, "started_at": now, "receipt_check_claimed": t.ReceiptCheckClaimed, "dispatch_state": "consumed", "publish_token": nil, "publish_lease_until": nil, "updated_at": now, "version": gorm.Expr("version+1")}).Error; e != nil {
			return e
		}
		claimed = true
		return nil
	})
	return
}

func (s *Store) reserveDirect(ctx context.Context, queue string) (t Task, ok bool, err error) {
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		q := tx.Raw(`SELECT * FROM tasks WHERE protocol_version=3 AND state='pending' AND queue=?
 AND dispatch_not_before<=clock_timestamp() AND dispatches<max_dispatches
 AND (dispatch_state='pending' OR (dispatch_state='publishing' AND publish_lease_until<=clock_timestamp()))
 ORDER BY dispatch_not_before,id LIMIT 1 FOR UPDATE SKIP LOCKED`, queue).Scan(&t)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected == 0 {
			return nil
		}
		token := uuid.New()
		t.DeliveryToken = &token
		t.Dispatches++
		if e := tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"dispatches": t.Dispatches, "dispatch_state": "publishing", "publish_token": token, "publish_lease_until": gorm.Expr("clock_timestamp()+(?*interval '1 millisecond')", s.Lease.Milliseconds()), "updated_at": gorm.Expr("clock_timestamp()")}).Error; e != nil {
			return e
		}
		ok = true
		return nil
	})
	return
}

func (s *Store) publishedDirect(ctx context.Context, t Task, cause error) error {
	if t.DeliveryToken == nil {
		return ErrInvalid
	}
	state := "confirmed"
	if cause != nil {
		state = "pending"
	}
	return s.DB.WithContext(ctx).Exec(`UPDATE tasks SET dispatch_state=?,publish_token=NULL,publish_lease_until=NULL,
 publish_confirmed_at=CASE WHEN ?='confirmed' THEN clock_timestamp() ELSE publish_confirmed_at END,
 dispatch_not_before=clock_timestamp()+(?*interval '1 millisecond'),updated_at=clock_timestamp()
 WHERE id=? AND protocol_version=3 AND dispatch_generation=? AND publish_token=? AND dispatch_state='publishing'`, state, state, Backoff(t.Dispatches, s.RetryBase, s.RetryCap, 0).Milliseconds(), t.ID, t.DispatchGeneration, *t.DeliveryToken).Error
}

func (s *Store) Complete(ctx context.Context, t Task, c Completion) (err error) {
	s.CompletionReports.Add(1)
	defer func() {
		if err != nil {
			s.CompletionErrors.Add(1)
		}
	}()
	if t.ProtocolVersion != 3 {
		return ErrInvalid
	}
	if e := c.Validate(t); e != nil {
		return e
	}
	return s.critical().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var cur Task
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&cur, "id=?", t.ID).Error; e != nil {
			return e
		}
		if cur.InputDigest != c.InputDigest || cur.ContractVersion != c.ContractVersion {
			return ErrConflict
		}
		if cur.State == Done {
			if cur.Outcome == c.Outcome {
				return nil
			}
			return ErrConflict
		}
		var now time.Time
		if e := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; e != nil {
			return e
		}
		if cur.State != Running || cur.Token == nil || t.Token == nil || *cur.Token != *t.Token || cur.Generation != t.Generation || cur.LeaseUntil == nil || !cur.LeaseUntil.After(now) {
			return ErrLost
		}
		if c.Outcome == Succeeded && s.PlanCompleted != nil {
			specs, e := s.PlanCompleted(cur, c)
			if e != nil {
				return e
			}
			if len(specs) > 128 {
				return ErrInvalid
			}
			for ordinal, spec := range specs {
				spec.ParentID = &cur.ID
				if len(spec.Trace) == 0 {
					spec.Trace = json.RawMessage(cur.Trace)
				}
				if spec.Kind != "ai.artifact.gc" && (spec.Deadline.IsZero() || spec.Deadline.After(cur.Deadline)) {
					spec.Deadline = cur.Deadline
				}
				child, e := s.ensure(tx, spec, fmt.Sprintf("completion:v%d:%s:%d", cur.ContractVersion, cur.ID, ordinal))
				if e != nil {
					return e
				}
				digest, _, e := Digest(spec.Input)
				if e != nil || child.InputDigest != digest {
					return ErrConflict
				}
			}
		}
		if cur.EffectTarget == EffectAI {
			if e := tx.Table("ai_job_task_links").Where("task_id=?", cur.ID).Update("completion_refs", string(refsOrEmpty(c.Refs))).Error; e != nil {
				return e
			}
		}
		if e := tx.Model(&Task{}).Where("id=?", cur.ID).Updates(map[string]any{"state": Done, "outcome": c.Outcome, "token": nil, "lease_until": nil, "stopped_at": now, "updated_at": now, "version": gorm.Expr("version+1")}).Error; e != nil {
			return e
		}
		cur.State = Done
		cur.Outcome = c.Outcome
		if s.AfterTerminal != nil {
			return s.AfterTerminal(tx, s, cur, c.Outcome)
		}
		return nil
	})
}

func refsOrEmpty(refs json.RawMessage) []byte {
	if len(refs) == 0 {
		return []byte(`{}`)
	}
	return refs
}

func (s *Store) abandonDirect(tx *gorm.DB, t Task) error {
	if s.PlanAbandoned == nil || t.Generation == 0 || t.Phase == "reconcile" {
		return nil
	}
	specs, e := s.PlanAbandoned(t)
	if e != nil {
		return e
	}
	for _, spec := range specs {
		if _, e = s.ensure(tx, spec, "abandoned:"+t.ID.String()); e != nil {
			return e
		}
	}
	return nil
}

func (s *Store) reconcileDirect(tx *gorm.DB, t *Task, now time.Time, reason string) error {
	if t.Phase == "reconcile" || t.ReceiptCheckClaimed || t.Dispatches >= t.MaxDispatches {
		return s.stopDirect(tx, t, reason, now)
	}
	t.Phase = "reconcile"
	deadline := now.Add(s.Lease)
	t.ReceiptCheckDeadline = &deadline
	if e := tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"phase": "reconcile", "receipt_check_deadline": deadline}).Error; e != nil {
		return e
	}
	return s.retry(tx, t, now, reason)
}

// Unknown domain state never triggers destructive compensation or fake failure.
func (s *Store) stopDirect(tx *gorm.DB, t *Task, reason string, now time.Time) error {
	if Terminal(t.State) {
		return nil
	}
	if err := tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"state": Stopped, "outcome": NeedsAttention, "last_error": reason, "token": nil, "lease_until": nil, "publish_token": nil, "publish_lease_until": nil, "dispatch_state": "stopped", "stopped_at": now, "updated_at": now, "version": gorm.Expr("version+1")}).Error; err != nil {
		return err
	}
	t.State, t.Outcome = Stopped, NeedsAttention
	if s.AfterTerminal != nil {
		return s.AfterTerminal(tx, s, *t, NeedsAttention)
	}
	return nil
}

func (s *Store) failDirect(ctx context.Context, t Task, cause error, permanent bool) error {
	return s.critical().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var cur Task
		q := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=? AND token=? AND generation=? AND state='running' AND lease_until>clock_timestamp()", t.ID, t.Token, t.Generation).Take(&cur)
		if errors.Is(q.Error, gorm.ErrRecordNotFound) {
			return ErrLost
		}
		if q.Error != nil {
			return q.Error
		}
		var now time.Time
		if e := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; e != nil {
			return e
		}
		if e := s.abandonDirect(tx, cur); e != nil {
			return e
		}
		if permanent || cur.Attempts >= cur.MaxAttempts || !now.Before(cur.Deadline) || cur.Phase == "reconcile" {
			return s.reconcileDirect(tx, &cur, now, cause.Error())
		}
		if cur.Dispatches >= cur.MaxDispatches {
			return s.stopDirect(tx, &cur, "publication budget exhausted", now)
		}
		return s.retry(tx, &cur, now.Add(Backoff(cur.Attempts, s.RetryBase, s.RetryCap, rand.Float64()*2-1)), cause.Error())
	})
}

func (s *Store) recoverDirect(ctx context.Context, limit int) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []Task
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where(`protocol_version=3 AND
 ((state='running' AND lease_until<=clock_timestamp()) OR
 (state='pending' AND ((phase='execute' AND (deadline<=clock_timestamp() OR attempts>=max_attempts))
 OR (phase='reconcile' AND receipt_check_deadline<=clock_timestamp())
 OR (dispatch_state IN ('pending','publishing') AND (publish_lease_until IS NULL OR publish_lease_until<=clock_timestamp()) AND dispatches>=max_dispatches))))`).Order("updated_at,id").Limit(limit).Find(&rows).Error; e != nil {
			return e
		}
		var now time.Time
		if e := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; e != nil {
			return e
		}
		for _, t := range rows {
			if e := s.abandonDirect(tx, t); e != nil {
				return e
			}
			if t.Phase == "reconcile" || t.Attempts >= t.MaxAttempts || !now.Before(t.Deadline) {
				if e := s.reconcileDirect(tx, &t, now, "lease or budget exhausted"); e != nil {
					return e
				}
				continue
			}
			if t.Dispatches >= t.MaxDispatches {
				if e := s.stopDirect(tx, &t, "publication budget exhausted", now); e != nil {
					return e
				}
				continue
			}
			if e := s.retry(tx, &t, now.Add(s.RetryBase), "execution lease expired"); e != nil {
				return e
			}
		}
		return nil
	})
}

func (s *Store) executeDirect(ctx context.Context, t Task, h DirectHandler) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("direct execution panic: %v", p)
		}
	}()
	if t.Phase == "reconcile" {
		s.ReceiptChecks.Add(1)
	}
	c, found, err := h.Lookup(ctx, t)
	if err != nil {
		return err
	}
	if !found {
		if t.Phase == "reconcile" {
			return ErrReceiptMissing
		}
		var due *time.Time
		s.DirectExecutions.Add(1)
		if t.Attempts > 1 {
			s.DirectRecomputes.Add(1)
		}
		c, due, err = h.Execute(ctx, t)
		if err != nil {
			return err
		}
		if due != nil {
			return s.rescheduleDirect(ctx, t, *due)
		}
	} else {
		s.ReceiptHits.Add(1)
	}
	return s.Complete(ctx, t, c)
}

func (s *Store) rescheduleDirect(ctx context.Context, t Task, due time.Time) error {
	if t.Kind != "upload.expire" || !due.After(time.Now()) || !due.Before(t.Deadline) {
		return ErrInvalid
	}
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var cur Task
		q := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=? AND token=? AND generation=? AND state='running' AND lease_until>clock_timestamp()", t.ID, t.Token, t.Generation).Take(&cur)
		if errors.Is(q.Error, gorm.ErrRecordNotFound) {
			return ErrLost
		}
		if q.Error != nil {
			return q.Error
		}
		return s.retry(tx, &cur, due, "domain expiry deferred")
	})
}
