package asynccontrol

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Store struct {
	ProtocolVersion int
	PlanCompleted   func(Task, Completion) ([]Spec, error)
	PlanAbandoned   func(Task) ([]Spec, error)
	Observe         func(*gorm.DB, Task) error
	AfterTerminal   func(*gorm.DB, *Store, Task, string) error
	// These hooks are trusted immutable planners, never worker supplied specs.
	PlanContinuations                                                                      func(Task, Result) ([]Spec, error)
	PlanFailure                                                                            func(Task) ([]Spec, error)
	CommitCalls, CommitNanos, ReceiptHits                                                  atomic.Uint64
	DirectExecutions, DirectRecomputes, CompletionReports, CompletionErrors, ReceiptChecks atomic.Uint64
	MaxAttempts, MaxDispatches, MaxRecoveries, MaxResultSends                              int
	Critical                                                                               *gorm.DB
	DB                                                                                     *gorm.DB
	Registry                                                                               Registry
	Lease                                                                                  time.Duration
	RetryBase, RetryCap                                                                    time.Duration
	CommitWindow                                                                           time.Duration
}

func New(db *gorm.DB) *Store {
	return &Store{ProtocolVersion: 2, MaxAttempts: 12, MaxDispatches: 20, MaxRecoveries: 12, MaxResultSends: 20, DB: db, Registry: CoreRegistry(), Lease: 30 * time.Second, RetryBase: 5 * time.Second, RetryCap: 5 * time.Minute, CommitWindow: 24 * time.Hour}
}

func (s *Store) Ensure(ctx context.Context, spec Spec, source string) (Task, error) {
	var t Task
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error { var err error; t, err = s.ensure(tx, spec, source); return err })
	if err == nil {
		digest, _, digestErr := Digest(spec.Input)
		if digestErr != nil {
			return t, digestErr
		}
		if t.InputDigest != digest || t.Kind != spec.Kind || t.ShardKey != spec.ShardKey {
			return t, ErrConflict
		}
	}
	return t, err
}

func (s *Store) ensure(tx *gorm.DB, spec Spec, source string) (Task, error) {
	d, err := s.Registry.Validate(spec)
	if err != nil {
		return Task{}, err
	}
	digest, input, err := Digest(spec.Input)
	if err != nil {
		return Task{}, err
	}
	var now time.Time
	if err = tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
		return Task{}, err
	}
	due := spec.NotBefore
	if due.IsZero() {
		due = now
	}
	trace := spec.Trace
	if len(trace) == 0 {
		trace = json.RawMessage(`{}`)
	}
	t := Task{ProtocolVersion: s.ProtocolVersion, Phase: "compute", MaxCommitAttempts: s.MaxAttempts, EffectTarget: d.Target(), ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("bx/async/v1/"+spec.SemanticKey)), SemanticKey: spec.SemanticKey, Kind: spec.Kind, ContractVersion: 1, InputDigest: digest, Input: input, Queue: d.Queue, OwnerKey: spec.OwnerKey, ResourceKey: spec.ResourceKey, ShardKey: spec.ShardKey, LayoutVersion: 0, Shard: 0, Priority: d.Priority, State: Pending, Version: 1, NotBefore: due, Deadline: maxTime(due, now).Add(d.Lifetime), Attempts: spec.Attempts, Dispatches: spec.Dispatches, Recoveries: spec.Recoveries, MaxAttempts: s.MaxAttempts, MaxDispatches: s.MaxDispatches, MaxRecoveries: s.MaxRecoveries, MaxResultSends: s.MaxResultSends, TimeoutMS: d.Timeout.Milliseconds(), ParentID: spec.ParentID, Trace: []byte(trace), CreatedAt: now, UpdatedAt: now}
	if Command(t.Kind) {
		t.Phase = "command"
	}
	if t.ProtocolVersion == 3 {
		t.Phase = "execute"
		t.DispatchState = "pending"
		t.DispatchNotBefore = due
	}
	if !spec.Deadline.IsZero() && spec.Deadline.Before(t.Deadline) {
		t.Deadline = spec.Deadline
	}
	if spec.MaxAttempts > 0 {
		t.MaxAttempts = min(t.MaxAttempts, spec.MaxAttempts)
	}
	if spec.MaxDispatches > 0 {
		t.MaxDispatches = min(t.MaxDispatches, spec.MaxDispatches)
	}
	if spec.MaxRecoveries > 0 {
		t.MaxRecoveries = min(t.MaxRecoveries, spec.MaxRecoveries)
	}
	if Command(t.Kind) {
		t.MaxCommitAttempts = max(1, t.MaxAttempts-t.Attempts)
		if t.Attempts >= t.MaxAttempts {
			t.Phase = "reconcile"
			reconcileDeadline := now.Add(s.Lease)
			t.CommitDeadline = &reconcileDeadline
		}
	}
	create := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "semantic_key"}}, DoNothing: true})
	if t.ProtocolVersion == 3 {
		create = create.Omit("LayoutVersion", "Shard", "Priority", "CommitAttempts", "MaxCommitAttempts", "CommitDeadline", "ResultID", "Recoveries", "MaxRecoveries", "ResultSends", "MaxResultSends")
	}
	res := create.Create(&t)
	if res.Error != nil {
		return t, res.Error
	}
	if res.RowsAffected == 0 {
		if err := tx.Where("semantic_key = ?", spec.SemanticKey).First(&t).Error; err != nil {
			return t, err
		}
		if t.InputDigest != digest || t.Kind != spec.Kind || t.ShardKey != spec.ShardKey {
			return t, s.quarantine(tx, "input:"+spec.SemanticKey, &t.ID, "same semantic identity with different input", spec.Input)
		}
	}
	if res.RowsAffected == 1 {
		if err := s.delivery(tx, &t, due); err != nil {
			return t, err
		}
	}
	if source != "" {
		if err := tx.Exec("INSERT INTO task_sources(task_id,source_key) VALUES(?,?) ON CONFLICT DO NOTHING", t.ID, source).Error; err != nil {
			return t, err
		}
	}
	if s.Observe != nil {
		if err := s.Observe(tx, t); err != nil {
			return t, err
		}
	}
	return t, nil
}

// ImportBudget merges conservative legacy counters before any new execution.
// Re-reading a second source must not retain a smaller budget from the first.
func (s *Store) ImportBudget(tx *gorm.DB, t Task, spec Spec) error {
	values := map[string]any{
		"attempts":   gorm.Expr("GREATEST(attempts,?)", spec.Attempts),
		"dispatches": gorm.Expr("GREATEST(dispatches,?)", spec.Dispatches),
		"recoveries": gorm.Expr("GREATEST(recoveries,?)", spec.Recoveries),
	}
	if spec.MaxAttempts > 0 {
		values["max_attempts"] = gorm.Expr("LEAST(max_attempts,?)", spec.MaxAttempts)
	}
	if !spec.Deadline.IsZero() {
		values["deadline"] = gorm.Expr("LEAST(deadline,?::timestamptz)", spec.Deadline)
	}
	return tx.Model(&Task{}).Where("id=? AND generation=0", t.ID).Updates(values).Error
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// Quarantine is durable. It does not overwrite a previously completed effect.
func (s *Store) quarantine(tx *gorm.DB, identity string, id *uuid.UUID, reason string, details []byte) error {
	if len(details) == 0 {
		details = []byte(`{}`)
	}
	q := tx.Exec(`INSERT INTO quarantines(identity,task_id,reason,details) VALUES(?,?,?,?::jsonb) ON CONFLICT(identity) DO NOTHING`, identity, id, reason, string(details))
	if q.Error == nil && q.RowsAffected == 1 {
		slog.Error("async input or result quarantined", "identity", identity, "task_id", id, "reason", reason)
	}
	return q.Error
}

func (s *Store) AcquireModule(ctx context.Context, name, holder string) (uuid.UUID, bool, error) {
	token := uuid.New()
	var row struct{ Token uuid.UUID }
	q := s.DB.WithContext(ctx).Raw(`INSERT INTO module_leases(name,holder,token,lease_until) VALUES(?,?,?,clock_timestamp()+(?*interval '1 millisecond'))
 ON CONFLICT(name) DO UPDATE SET holder=EXCLUDED.holder,token=EXCLUDED.token,lease_until=EXCLUDED.lease_until,updated_at=clock_timestamp()
 WHERE module_leases.lease_until<=clock_timestamp() RETURNING token`, name, holder, token, s.Lease.Milliseconds()).Scan(&row)
	return token, row.Token == token, q.Error
}

func (s *Store) RenewModule(ctx context.Context, name string, token uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, s.Lease/6)
	defer cancel()
	q := s.critical().WithContext(ctx).Exec(`UPDATE module_leases SET lease_until=clock_timestamp()+(?*interval '1 millisecond'),updated_at=clock_timestamp() WHERE name=? AND token=? AND lease_until>clock_timestamp()`, s.Lease.Milliseconds(), name, token)
	return owned(q)
}

func owned(q *gorm.DB) error {
	if q.Error != nil {
		return q.Error
	}
	if q.RowsAffected != 1 {
		return ErrLost
	}
	return nil
}

func (s *Store) critical() *gorm.DB {
	if s.Critical != nil {
		return s.Critical
	}
	return s.DB
}

// ImportState may only seed a never-executed task; rerunning the import cannot
// overwrite live execution or refresh a consumed budget.
func (s *Store) ImportState(tx *gorm.DB, t Task, state string, body json.RawMessage, reason string) error {
	var current Task
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id=?", t.ID).Error; err != nil {
		return err
	}
	if current.Generation != 0 || current.State != Pending {
		return nil
	}
	if len(body) > 0 {
		var now time.Time
		if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return err
		}
		if err := s.commandResult(tx, current, body, state, now); err != nil {
			return err
		}
	}
	if state == Exhausted {
		var now time.Time
		if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return err
		}
		return s.stop(tx, &current, reason, now)
	}
	outcome := ""
	switch state {
	case Computed, Converging:
		state = Pending
	case Succeeded, Superseded, Rejected, Failed:
		outcome = state
		state = Done
	case Exhausted, Quarantined, NeedsAttention:
		outcome = state
		state = Stopped
	}
	return tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"state": state, "outcome": outcome, "last_error": reason, "updated_at": gorm.Expr("clock_timestamp()")}).Error
}

// Redo creates one audited child for a stopped operation. Retrying the operator
// command returns that child; the old task and its consumed budgets remain.
func (s *Store) Redo(ctx context.Context, id uuid.UUID, reason string, validate func(Task) error) (Task, error) {
	return s.RedoPlanned(ctx, id, reason, func(old Task, spec Spec) (Spec, error) {
		if validate == nil {
			return spec, ErrInvalid
		}
		return spec, validate(old)
	})
}

// RedoPlanned lets a trusted domain planner renew an expired authorization or
// route a failed derived step through its current root prerequisites. The
// audit identity, parent and routing keys remain tied to the stopped task.
func (s *Store) RedoPlanned(ctx context.Context, id uuid.UUID, reason string, plan func(Task, Spec) (Spec, error)) (Task, error) {
	var next Task
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var old Task
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&old, "id=?", id).Error; err != nil {
			return err
		}
		key := old.SemanticKey + ":redo:" + uuid.NewSHA1(old.ID, []byte("manual-v1")).String()
		q := tx.Where("semantic_key=?", key).Find(&next)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected > 0 {
			return nil
		}
		if old.State != Stopped && old.State != Failed && old.State != Rejected && old.State != Quarantined && old.State != NeedsAttention {
			return ErrInvalid
		}
		if len(reason) < 8 || len(reason) > 1024 || plan == nil {
			return ErrInvalid
		}
		spec := Spec{Kind: old.Kind, SemanticKey: key, OwnerKey: old.OwnerKey, ResourceKey: old.ResourceKey, ShardKey: old.ShardKey, Input: json.RawMessage(old.Input), Trace: json.RawMessage(old.Trace), ParentID: &old.ID}
		prepared, err := plan(old, spec)
		if err != nil {
			return err
		}
		spec.Kind, spec.Input, spec.Deadline = prepared.Kind, prepared.Input, prepared.Deadline
		next, err = s.ensure(tx, spec, "manual:"+reason)
		if err != nil {
			return err
		}
		return tx.Model(&Task{}).Where("id=?", id).Updates(map[string]any{"state": Stopped, "outcome": Quarantined, "last_error": "explicit redo: " + next.ID.String() + "; " + reason}).Error
	})
	return next, err
}

// EnsureTx is reserved for trusted domain planners composing atomic transitions.
func (s *Store) EnsureTx(tx *gorm.DB, spec Spec, source string) (Task, error) {
	return s.ensure(tx, spec, source)
}
