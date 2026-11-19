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

// delivery is called with the task row locked (or just inserted). A retry gets
// one new delivery identity; publisher retries keep that identity unchanged.
func (s *Store) delivery(tx *gorm.DB, t *Task, due time.Time) error {
	t.DispatchGeneration++
	if err := tx.Model(&Task{}).Where("id=?", t.ID).Update("dispatch_generation", t.DispatchGeneration).Error; err != nil {
		return err
	}
	return tx.Exec(`INSERT INTO task_deliveries(task_id,generation,state,not_before) VALUES(?,?,'pending',?)
 ON CONFLICT(task_id) DO UPDATE SET generation=EXCLUDED.generation,state='pending',not_before=EXCLUDED.not_before,token=NULL,lease_until=NULL,sends=0,confirmed_at=NULL,last_error='',updated_at=clock_timestamp()`, t.ID, t.DispatchGeneration, due).Error
}

func (s *Store) Claim(ctx context.Context, n Notice) (t Task, claimed bool, err error) {
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		q := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", n.TaskID).Find(&t)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected == 0 {
			return ErrInvalid
		}
		if !n.validWire() || !n.matches(t) || t.ProtocolVersion != 2 {
			return ErrConflict
		}
		if Terminal(t.State) || n.DispatchGeneration != t.DispatchGeneration {
			return nil
		}
		var now time.Time
		if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return err
		}
		if t.State == Running {
			return nil
		} // durable running lease is recovered centrally
		if t.State != Pending {
			return ErrInvalid
		}
		if now.Before(t.NotBefore) {
			// Even an unexpectedly early current notice can only be ACKed after the
			// future delivery is pending again. A confirmed/consumed record alone
			// would never publish at the future due time.
			return owned(tx.Exec("UPDATE task_deliveries SET state='pending',not_before=?,token=NULL,lease_until=NULL,updated_at=clock_timestamp() WHERE task_id=? AND generation=?", t.NotBefore, t.ID, t.DispatchGeneration))
		}
		if t.Phase == "reconcile" && (t.CommitDeadline == nil || !now.Before(*t.CommitDeadline)) {
			return s.stop(tx, &t, "receipt reconciliation deadline expired", now)
		}
		if t.Phase != "reconcile" {
			if t.ResultID == nil && !Command(t.Kind) && (!now.Before(t.Deadline) || t.Attempts >= t.MaxAttempts) {
				return s.stop(tx, &t, "execution budget exhausted", now)
			}
			if (t.ResultID != nil || Command(t.Kind)) && (t.CommitAttempts >= t.MaxCommitAttempts || t.CommitDeadline != nil && !now.Before(*t.CommitDeadline) || Command(t.Kind) && !now.Before(t.Deadline)) {
				return s.reconcile(tx, &t, now, "commit budget exhausted")
			}
		}
		token := uuid.New()
		lease := now.Add(min(s.Lease, time.Duration(t.TimeoutMS)*time.Millisecond))
		t.Token = &token
		t.LeaseUntil = &lease
		t.StartedAt = &now
		t.State = Running
		if t.ResultID == nil && !Command(t.Kind) {
			t.Generation++
			t.Attempts++
			t.Phase = "compute"
		} else {
			t.CommitAttempts++
			if t.Phase != "reconcile" {
				t.Phase = "commit"
				if Command(t.Kind) {
					t.Phase = "command"
				}
			}
		}
		if Command(t.Kind) && t.CommitDeadline == nil {
			d := minTime(t.Deadline, now.Add(s.CommitWindow))
			t.CommitDeadline = &d
		}
		if err := tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"state": Running, "phase": t.Phase, "generation": t.Generation, "attempts": t.Attempts, "commit_attempts": t.CommitAttempts, "commit_deadline": t.CommitDeadline, "token": token, "lease_until": lease, "started_at": now, "version": gorm.Expr("version+1"), "updated_at": now}).Error; err != nil {
			return err
		}
		if t.Phase == "compute" {
			if err := tx.Exec("INSERT INTO executions(task_id,generation,token) VALUES(?,?,?)", t.ID, t.Generation, token).Error; err != nil {
				return err
			}
		}
		if err := tx.Exec("UPDATE task_deliveries SET state='consumed',token=NULL,lease_until=NULL,updated_at=clock_timestamp() WHERE task_id=? AND generation=?", t.ID, n.DispatchGeneration).Error; err != nil {
			return err
		}
		claimed = true
		return nil
	})
	return
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (s *Store) Renew(ctx context.Context, t Task) error {
	if t.Token == nil {
		return ErrLost
	}
	ctx, cancel := context.WithTimeout(ctx, s.Lease/6)
	defer cancel()
	return owned(s.critical().WithContext(ctx).Exec(`UPDATE tasks SET lease_until=LEAST(CASE WHEN phase='compute' THEN deadline ELSE COALESCE(commit_deadline,deadline) END,started_at+(timeout_ms*interval '1 millisecond'),clock_timestamp()+(?*interval '1 millisecond')),updated_at=clock_timestamp()
 WHERE id=? AND generation=? AND token=? AND state='running' AND lease_until>clock_timestamp() AND started_at+(timeout_ms*interval '1 millisecond')>clock_timestamp()`, s.Lease.Milliseconds(), t.ID, t.Generation, *t.Token))
}

// Saving a checkpoint keeps the same live token. Its generation and digest are
// immutable; every subsequent claim enters commit without another computation.
func (s *Store) SaveResult(ctx context.Context, t Task, body json.RawMessage) (r Result, err error) {
	digest, b, err := Digest(body)
	if err != nil || len(b) > 1<<20 {
		return r, ErrInvalid
	}
	r = Result{ID: uuid.NewSHA1(t.ID, []byte(fmt.Sprint(t.Generation))), TaskID: t.ID, Generation: t.Generation, InputDigest: t.InputDigest, ResultDigest: digest, Body: b, EffectTarget: t.EffectTarget}
	err = s.critical().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if t.Token == nil {
			return ErrLost
		}
		q := tx.Model(&Task{}).Where("id=? AND state=? AND phase='compute' AND result_id IS NULL AND generation=? AND token=? AND lease_until>clock_timestamp() AND deadline>clock_timestamp()", t.ID, Running, t.Generation, *t.Token).Updates(map[string]any{"result_id": r.ID, "phase": "commit", "commit_deadline": gorm.Expr("clock_timestamp()+(?*interval '1 millisecond')", s.CommitWindow.Milliseconds()), "version": gorm.Expr("version+1"), "updated_at": gorm.Expr("clock_timestamp()")})
		if err := owned(q); err != nil {
			return err
		}
		if err := tx.Create(&r).Error; err != nil {
			return err
		}
		if err := s.freezeContinuations(tx, t, r); err != nil {
			return err
		}
		return tx.Exec("UPDATE executions SET finished_at=clock_timestamp(),outcome='computed' WHERE task_id=? AND generation=?", t.ID, t.Generation).Error
	})
	return
}

func (s *Store) BeginCommit(ctx context.Context, t *Task, r Result) error {
	q := s.critical().WithContext(ctx).Model(&Task{}).Where("id=? AND state='running' AND token=? AND generation=? AND result_id=? AND lease_until>clock_timestamp() AND commit_attempts<max_commit_attempts AND commit_deadline>clock_timestamp()", t.ID, t.Token, t.Generation, r.ID).Updates(map[string]any{"commit_attempts": gorm.Expr("commit_attempts+1"), "phase": "commit", "started_at": gorm.Expr("clock_timestamp()")})
	if err := owned(q); err != nil {
		return err
	}
	t.ResultID = &r.ID
	t.Phase = "commit"
	t.CommitAttempts++
	return nil
}

func (s *Store) checkpoint(ctx context.Context, t Task) (r Result, err error) {
	if t.ResultID == nil {
		return r, ErrInvalid
	}
	err = s.DB.WithContext(ctx).First(&r, "id=? AND task_id=? AND generation=? AND input_digest=? AND effect_target=?", *t.ResultID, t.ID, t.Generation, t.InputDigest, t.EffectTarget).Error
	if err != nil {
		return r, err
	}
	if t.State == Done && r.CommittedAt != nil {
		return r, nil
	}
	digest, _, err := Digest(r.Body)
	if err != nil || digest != r.ResultDigest {
		return r, ErrConflict
	}
	return r, nil
}

// CommandResult is a stable receipt identity, not a synthetic stored result.
func CommandResult(t Task) Result {
	digest, _, _ := Digest(t.Input)
	return Result{ID: uuid.NewSHA1(t.ID, []byte("command-v2")), TaskID: t.ID, Generation: t.Generation, InputDigest: t.InputDigest, ResultDigest: digest, EffectTarget: t.EffectTarget, Body: []byte(`{}`)}
}

func (s *Store) Fail(ctx context.Context, t Task, cause error, permanent bool) error {
	if t.Token == nil || cause == nil {
		return ErrInvalid
	}
	return s.critical().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Task
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id=? AND token=? AND state='running' AND lease_until>clock_timestamp()", t.ID, *t.Token).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrLost
			}
			return err
		}
		var now time.Time
		if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return err
		}
		if current.ResultID != nil || Command(current.Kind) {
			if current.Phase == "reconcile" {
				return s.stop(tx, &current, cause.Error(), now)
			}
			if permanent || current.CommitAttempts >= current.MaxCommitAttempts || current.CommitDeadline != nil && !now.Before(*current.CommitDeadline) {
				return s.reconcile(tx, &current, now, cause.Error())
			}
		} else {
			if err := tx.Exec("UPDATE executions SET finished_at=?,outcome='retry_wait',error=? WHERE task_id=? AND generation=?", now, cause.Error(), t.ID, t.Generation).Error; err != nil {
				return err
			}
			if permanent || current.Attempts >= current.MaxAttempts || !now.Before(current.Deadline) {
				return s.stop(tx, &current, cause.Error(), now)
			}
		}
		attempt := current.Attempts
		if current.ResultID != nil || Command(current.Kind) {
			attempt = current.CommitAttempts
		}
		return s.retry(tx, &current, now.Add(Backoff(attempt, s.RetryBase, s.RetryCap, rand.Float64()*2-1)), cause.Error())
	})
}

func (s *Store) retry(tx *gorm.DB, t *Task, due time.Time, reason string) error {
	if err := tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"state": Pending, "token": nil, "lease_until": nil, "not_before": due, "last_error": reason, "version": gorm.Expr("version+1"), "updated_at": gorm.Expr("clock_timestamp()")}).Error; err != nil {
		return err
	}
	return s.delivery(tx, t, due)
}

func (s *Store) reconcile(tx *gorm.DB, t *Task, now time.Time, reason string) error {
	if t.Phase == "reconcile" {
		return s.stop(tx, t, reason, now)
	}
	t.Phase = "reconcile"
	if err := tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"phase": "reconcile", "commit_deadline": now.Add(s.Lease)}).Error; err != nil {
		return err
	}
	return s.retry(tx, t, now, reason)
}

func (s *Store) stop(tx *gorm.DB, t *Task, reason string, now time.Time) error {
	if Terminal(t.State) {
		return nil
	}
	if err := tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"state": Stopped, "outcome": NeedsAttention, "last_error": reason, "token": nil, "lease_until": nil, "stopped_at": now, "updated_at": now, "version": gorm.Expr("version+1")}).Error; err != nil {
		return err
	}
	if err := tx.Exec("UPDATE task_deliveries SET state='stopped',token=NULL,lease_until=NULL,updated_at=? WHERE task_id=?", now, t.ID).Error; err != nil {
		return err
	}
	if t.ResultID == nil && !Command(t.Kind) {
		if d, ok := s.Registry[t.Kind]; ok && d.Converge && d.Target() == EffectCore {
			// The failure command has its own bounded lifetime; it cannot refresh the
			// original operation's attempts or replace an accepted successful result.
			if _, err := s.ensure(tx, Spec{Kind: "failure.converge", SemanticKey: t.SemanticKey + ":failure", OwnerKey: t.OwnerKey, ResourceKey: t.ResourceKey, ShardKey: t.ShardKey, Input: json.RawMessage(t.Input), ParentID: &t.ID}, "failure:"+t.ID.String()); err != nil {
				return err
			}
		}
		if s.PlanFailure != nil {
			specs, err := s.PlanFailure(*t)
			if err != nil {
				return err
			}
			for _, spec := range specs {
				if _, err = s.ensure(tx, spec, "failure:"+t.ID.String()); err != nil {
					return err
				}
			}
		}
	}
	if s.AfterTerminal != nil {
		return s.AfterTerminal(tx, s, *t, NeedsAttention)
	}
	return nil
}

func (s *Store) Report(ctx context.Context, t Task, r Result, outcome string) error {
	if outcome != Succeeded && outcome != Superseded && outcome != Rejected && outcome != Failed {
		return ErrInvalid
	}
	return s.critical().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var cur Task
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&cur, "id=?", t.ID).Error; err != nil {
			return err
		}
		if cur.State == Done {
			if cur.Outcome == outcome {
				return nil
			}
			return ErrConflict
		}
		if cur.State != Running || cur.Token == nil || t.Token == nil || *cur.Token != *t.Token || cur.Generation != t.Generation {
			return ErrLost
		}
		if cur.InputDigest != r.InputDigest || cur.EffectTarget != r.EffectTarget {
			return ErrConflict
		}
		if cur.ResultID != nil {
			if *cur.ResultID != r.ID {
				return ErrConflict
			}
			if err := owned(tx.Model(&Result{}).Where("id=? AND result_digest=?", r.ID, r.ResultDigest).Updates(map[string]any{"outcome": outcome, "committed_at": gorm.Expr("clock_timestamp()")})); err != nil {
				return err
			}
			if err := s.releaseContinuations(tx, cur, r, outcome); err != nil {
				return err
			}
		} else if !Command(cur.Kind) || r.ID != CommandResult(cur).ID {
			return ErrConflict
		}
		if err := tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"state": Done, "outcome": outcome, "token": nil, "lease_until": nil, "version": gorm.Expr("version+1"), "stopped_at": gorm.Expr("clock_timestamp()"), "updated_at": gorm.Expr("clock_timestamp()")}).Error; err != nil {
			return err
		}
		if s.AfterTerminal != nil {
			return s.AfterTerminal(tx, s, cur, outcome)
		}
		return nil
	})
}

func (s *Store) LoadResult(ctx context.Context, n Notice) (t Task, r Result, err error) {
	err = s.DB.WithContext(ctx).First(&t, "id=?", n.TaskID).Error
	if err != nil {
		return
	}
	if Command(t.Kind) && t.ResultID == nil {
		r = CommandResult(t)
		return
	}
	if t.ResultID == nil || (n.ResultID != nil && *t.ResultID != *n.ResultID) {
		return t, r, ErrObsolete
	}
	r, err = s.checkpoint(ctx, t)
	return
}

func (s *Store) IsolateResult(ctx context.Context, n Notice, reason string) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		body, _ := json.Marshal(n)
		if err := s.quarantine(tx, "result:"+n.TaskID.String(), &n.TaskID, reason, body); err != nil {
			return err
		}
		return tx.Model(&Task{}).Where("id=? AND state IN ?", n.TaskID, []string{Pending, Running}).Updates(map[string]any{"state": Stopped, "outcome": Quarantined, "last_error": reason, "token": nil, "lease_until": nil, "stopped_at": gorm.Expr("clock_timestamp()")}).Error
	})
}

func (s *Store) Reschedule(ctx context.Context, t Task, r Result, due time.Time) error {
	if !Command(t.Kind) {
		return ErrInvalid
	}
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := owned(tx.Model(&Task{}).Where("id=? AND state='running' AND token=?", t.ID, t.Token).Updates(map[string]any{"state": Pending, "not_before": due, "deadline": gorm.Expr("GREATEST(deadline,?::timestamptz)", due.Add(s.Registry[t.Kind].Lifetime)), "commit_deadline": nil, "phase": "command", "token": nil, "lease_until": nil, "version": gorm.Expr("version+1")})); err != nil {
			return err
		}
		return s.delivery(tx, &t, due)
	})
}

// AcceptedResult is checked before entering a domain database. It proves that
// this immutable checkpoint was accepted by control, including for direct
// administrative replay of a retained result.
func (s *Store) AcceptedResult(ctx context.Context, t Task, r Result) error {
	var current Task
	if err := s.DB.WithContext(ctx).First(&current, "id=? AND (state='done' OR (state='running' AND lease_until>clock_timestamp()))", t.ID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrLost
		}
		return err
	}
	if current.State == Running && (current.Token == nil || t.Token == nil || *current.Token != *t.Token || current.Generation != t.Generation || current.Phase == "reconcile" && t.Phase != "reconcile") {
		return ErrLost
	}
	if current.ID != r.TaskID || current.Kind != t.Kind || current.SemanticKey != t.SemanticKey || current.InputDigest != t.InputDigest || current.EffectTarget != t.EffectTarget {
		return ErrConflict
	}
	if current.State != Done {
		digest, _, err := Digest(t.Input)
		if err != nil || digest != current.InputDigest {
			return ErrConflict
		}
	}
	if current.State != Running && current.State != Done {
		return ErrLost
	}
	if current.ResultID == nil {
		if !Command(current.Kind) || r.ID != CommandResult(current).ID || r.InputDigest != current.InputDigest {
			return ErrConflict
		}
		return nil
	}
	if *current.ResultID != r.ID || current.Generation != r.Generation || current.InputDigest != r.InputDigest || current.EffectTarget != r.EffectTarget {
		return ErrConflict
	}
	digest, _, err := Digest(r.Body)
	if err != nil || digest != r.ResultDigest {
		return ErrConflict
	}
	var count int64
	if err := s.DB.WithContext(ctx).Model(&Result{}).Where("id=? AND task_id=? AND result_digest=? AND input_digest=? AND generation=?", r.ID, t.ID, r.ResultDigest, r.InputDigest, r.Generation).Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return ErrConflict
	}
	return nil
}
