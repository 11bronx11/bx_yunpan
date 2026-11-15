package asynccontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Store struct {
	CommitCalls, CommitNanos, ReceiptHits                     atomic.Uint64
	MaxAttempts, MaxDispatches, MaxRecoveries, MaxResultSends int
	BatchRows, BatchBytes                                     int
	BatchWait                                                 time.Duration
	BatchCommit                                               func(context.Context, []Notice) []error
	Critical                                                  *gorm.DB
	DB                                                        *gorm.DB
	Registry                                                  Registry
	Shards                                                    int
	Lease                                                     time.Duration
	RetryBase, RetryCap                                       time.Duration
	CommitWindow                                              time.Duration
}

func New(db *gorm.DB) *Store {
	return &Store{MaxAttempts: 12, MaxDispatches: 20, MaxRecoveries: 12, MaxResultSends: 20, BatchRows: 32, BatchBytes: 1 << 20, BatchWait: 5 * time.Millisecond, DB: db, Registry: CoreRegistry(), Shards: 32, Lease: 30 * time.Second, RetryBase: 5 * time.Second, RetryCap: 5 * time.Minute, CommitWindow: 24 * time.Hour}
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
	t := Task{ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("bx/async/v1/"+spec.SemanticKey)), SemanticKey: spec.SemanticKey, Kind: spec.Kind, ContractVersion: 1, InputDigest: digest, Input: input, Queue: d.Queue, OwnerKey: spec.OwnerKey, ResourceKey: spec.ResourceKey, ShardKey: spec.ShardKey, LayoutVersion: 1, Shard: Shard(spec.ShardKey, s.Shards), Priority: d.Priority, State: Ready, Version: 1, NotBefore: due, Deadline: maxTime(due, now).Add(d.Lifetime), Attempts: spec.Attempts, Dispatches: spec.Dispatches, Recoveries: spec.Recoveries, MaxAttempts: s.MaxAttempts, MaxDispatches: s.MaxDispatches, MaxRecoveries: s.MaxRecoveries, MaxResultSends: s.MaxResultSends, TimeoutMS: d.Timeout.Milliseconds(), ParentID: spec.ParentID, Trace: []byte(trace), CreatedAt: now, UpdatedAt: now}
	if !spec.Deadline.IsZero() && spec.Deadline.Before(t.Deadline) {
		t.Deadline = spec.Deadline
	}
	if spec.MaxAttempts > 0 {
		t.MaxAttempts = min(t.MaxAttempts, spec.MaxAttempts)
	}
	res := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "semantic_key"}}, DoNothing: true}).Create(&t)
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
	if source != "" {
		if err := tx.Exec("INSERT INTO task_sources(task_id,source_key) VALUES(?,?) ON CONFLICT DO NOTHING", t.ID, source).Error; err != nil {
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

func (s *Store) Claim(ctx context.Context, n Notice) (Task, bool, error) {
	var t Task
	claimed := false
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&t, "id = ?", n.TaskID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if n.Version != 1 || t.DispatchGeneration != n.DispatchGeneration || t.State != Dispatched {
			return nil
		}
		if n.InputDigest != "" && !n.matches(t) {
			return nil
		}
		var now time.Time
		if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return err
		}
		if !now.Before(t.Deadline) || t.Attempts >= t.MaxAttempts {
			return nil
		}
		token := uuid.New()
		lease := now.Add(min(s.Lease, time.Duration(t.TimeoutMS)*time.Millisecond))
		t.Token = &token
		t.LeaseUntil = &lease
		t.Generation++
		t.Attempts++
		t.State = Running
		t.StartedAt = &now
		t.Version++
		q := tx.Model(&Task{}).Where("id=? AND state=? AND dispatch_generation=?", t.ID, Dispatched, n.DispatchGeneration).Updates(map[string]any{"state": Running, "generation": t.Generation, "attempts": t.Attempts, "token": token, "lease_until": lease, "started_at": now, "version": t.Version, "updated_at": now})
		if err := owned(q); err != nil {
			return err
		}
		if err := tx.Exec("INSERT INTO executions(task_id,generation,token) VALUES(?,?,?)", t.ID, t.Generation, token).Error; err != nil {
			return err
		}
		claimed = true
		return nil
	})
	return t, claimed, err
}

func (s *Store) critical() *gorm.DB {
	if s.Critical != nil {
		return s.Critical
	}
	return s.DB
}

func (s *Store) Renew(ctx context.Context, t Task) error {
	ctx, cancel := context.WithTimeout(ctx, s.Lease/6)
	defer cancel()
	if t.Token == nil {
		return ErrLost
	}
	return owned(s.critical().WithContext(ctx).Exec(`UPDATE tasks SET lease_until=LEAST(deadline,started_at+(timeout_ms*interval '1 millisecond'),clock_timestamp()+(?*interval '1 millisecond')),updated_at=clock_timestamp()
 WHERE id=? AND generation=? AND token=? AND state='running' AND lease_until>clock_timestamp() AND started_at+(timeout_ms*interval '1 millisecond')>clock_timestamp() AND deadline>clock_timestamp()`, s.Lease.Milliseconds(), t.ID, t.Generation, *t.Token))
}

func (s *Store) SaveResult(ctx context.Context, t Task, body json.RawMessage) (Result, error) {
	digest, b, err := Digest(body)
	if err != nil || len(b) > 1<<20 {
		return Result{}, ErrInvalid
	}
	r := Result{ID: uuid.NewSHA1(t.ID, []byte(fmt.Sprint(t.Generation))), TaskID: t.ID, Generation: t.Generation, InputDigest: t.InputDigest, ResultDigest: digest, Body: b}
	err = s.critical().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if t.Token == nil {
			return ErrLost
		}
		state := Computed
		if d, ok := s.Registry[t.Kind]; ok && !d.BusinessEffect {
			state = Succeeded
			r.Outcome = Succeeded
			now := time.Now().UTC()
			r.CommittedAt = &now
		}
		q := tx.Model(&Task{}).Where("id=? AND state=? AND generation=? AND token=? AND lease_until>clock_timestamp()", t.ID, Running, t.Generation, *t.Token).Updates(map[string]any{"state": state, "result_id": r.ID, "token": nil, "lease_until": nil, "not_before": gorm.Expr("clock_timestamp()"), "commit_deadline": gorm.Expr("clock_timestamp()+(?*interval '1 millisecond')", s.CommitWindow.Milliseconds()), "version": gorm.Expr("version+1"), "updated_at": gorm.Expr("clock_timestamp()")})
		if err := owned(q); err != nil {
			return err
		}
		if err := tx.Create(&r).Error; err != nil {
			return err
		}
		return tx.Exec("UPDATE executions SET finished_at=clock_timestamp(),outcome='computed' WHERE task_id=? AND generation=?", t.ID, t.Generation).Error
	})
	return r, err
}

func (s *Store) Fail(ctx context.Context, t Task, cause error, permanent bool) error {
	if t.Token == nil || cause == nil {
		return ErrInvalid
	}
	return s.critical().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		state := RetryWait
		if permanent || t.Attempts >= t.MaxAttempts {
			state = Exhausted
		}
		q := tx.Model(&Task{}).Where("id=? AND state=? AND generation=? AND token=? AND lease_until>clock_timestamp()", t.ID, Running, t.Generation, *t.Token).Updates(map[string]any{"state": state, "token": nil, "lease_until": nil, "last_error": cause.Error(), "not_before": gorm.Expr("clock_timestamp()+(?*interval '1 millisecond')", Backoff(t.Attempts, s.RetryBase, s.RetryCap, rand.Float64()*2-1).Milliseconds()), "version": gorm.Expr("version+1"), "updated_at": gorm.Expr("clock_timestamp()")})
		if err := owned(q); err != nil {
			return err
		}
		return tx.Exec("UPDATE executions SET finished_at=clock_timestamp(),outcome=?,error=? WHERE task_id=? AND generation=?", state, cause.Error(), t.ID, t.Generation).Error
	})
}

func (s *Store) LoadResult(ctx context.Context, n Notice) (Task, Result, error) {
	var t Task
	var r Result
	if n.Version != 1 || n.ResultID == nil {
		return t, r, ErrInvalid
	}
	if err := s.DB.WithContext(ctx).First(&t, "id=?", n.TaskID).Error; err != nil {
		return t, r, err
	}
	if t.ResultID == nil || *t.ResultID != *n.ResultID {
		return t, r, ErrObsolete
	}
	if t.Shard != n.Shard {
		return t, r, ErrConflict
	}
	if n.InputDigest != "" && !n.matches(t) {
		return t, r, ErrConflict
	}
	if err := s.DB.WithContext(ctx).First(&r, "id=? AND task_id=? AND input_digest=?", *n.ResultID, t.ID, t.InputDigest).Error; err != nil {
		return t, r, err
	}
	if r.Generation != t.Generation {
		return t, r, ErrConflict
	}
	if Terminal(t.State) && t.State != NeedsAttention && r.CommittedAt != nil {
		return t, r, nil
	}
	digest, _, err := Digest(r.Body)
	if err != nil || digest != r.ResultDigest {
		return t, r, ErrConflict
	}
	return t, r, nil
}

// IsolateResult persists a permanent protocol/receipt conflict before the
// caller ACKs. Successful effects and their receipts are never overwritten.
func (s *Store) IsolateResult(ctx context.Context, n Notice, reason string) error {
	body, _ := json.Marshal(n)
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.quarantine(tx, fmt.Sprintf("result:%s:%v", n.TaskID, n.ResultID), nil, reason, body); err != nil {
			return err
		}
		if n.ResultID == nil {
			return nil
		}
		return tx.Model(&Task{}).Where("id=? AND result_id=? AND shard=? AND state IN ?", n.TaskID, *n.ResultID, n.Shard, []string{Computed, Converging}).Updates(map[string]any{"state": Quarantined, "last_error": reason, "stopped_at": gorm.Expr("clock_timestamp()"), "updated_at": gorm.Expr("clock_timestamp()")}).Error
	})
}

// Report only accepts a durable result identity verified by the trusted
// committer after committing a business receipt. Worker lease expiry is irrelevant.
func (s *Store) Report(ctx context.Context, t Task, r Result, outcome string) error {
	if outcome != Succeeded && outcome != Superseded && outcome != Rejected && outcome != Failed {
		return ErrInvalid
	}
	return s.critical().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Task
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id=?", t.ID).Error; err != nil {
			return err
		}
		if current.ResultID == nil || *current.ResultID != r.ID || current.InputDigest != r.InputDigest {
			return ErrConflict
		}
		if Terminal(current.State) && current.State != NeedsAttention {
			if current.State == outcome {
				return nil
			}
			return ErrConflict
		}
		if current.State != Computed && current.State != Converging && current.State != NeedsAttention {
			return ErrLost
		}
		if err := tx.Model(&Result{}).Where("id=? AND result_digest=?", r.ID, r.ResultDigest).Updates(map[string]any{"outcome": outcome, "committed_at": gorm.Expr("clock_timestamp()")}).Error; err != nil {
			return err
		}
		return tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"state": outcome, "version": gorm.Expr("version+1"), "stopped_at": gorm.Expr("clock_timestamp()"), "updated_at": gorm.Expr("clock_timestamp()")}).Error
	})
}

// Reschedule retains semantic identity, all consumed budgets and historical
// results. Only the verified scheduling precondition can move the due time.
func (s *Store) Reschedule(ctx context.Context, t Task, r Result, due time.Time) error {
	return owned(s.DB.WithContext(ctx).Model(&Task{}).Where("id=? AND result_id=? AND state=?", t.ID, r.ID, Computed).Updates(map[string]any{"state": Ready, "not_before": due, "deadline": gorm.Expr("GREATEST(deadline,?::timestamptz)", due.Add(s.Registry[t.Kind].Lifetime)), "result_id": nil, "generation": gorm.Expr("generation+1"), "commit_deadline": nil, "version": gorm.Expr("version+1"), "updated_at": gorm.Expr("clock_timestamp()")}))
}

// ImportState may only seed a never-executed task; rerunning the import cannot
// overwrite live execution or refresh a consumed budget.
func (s *Store) ImportState(tx *gorm.DB, t Task, state string, body json.RawMessage, reason string) error {
	var current Task
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id=?", t.ID).Error; err != nil {
		return err
	}
	if current.Generation != 0 || current.State != Ready {
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
	return tx.Model(&Task{}).Where("id=?", t.ID).Updates(map[string]any{"state": state, "last_error": reason, "updated_at": gorm.Expr("clock_timestamp()")}).Error
}

// Redo creates one audited child for a stopped operation. Retrying the operator
// command returns that child; the old task and its consumed budgets remain.
func (s *Store) Redo(ctx context.Context, id uuid.UUID, reason string, validate func(Task) error) (Task, error) {
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
		if old.State != NeedsAttention && old.State != Quarantined && old.State != Failed && old.State != Rejected {
			return ErrInvalid
		}
		if len(reason) < 8 || len(reason) > 1024 || validate == nil {
			return ErrInvalid
		}
		if err := validate(old); err != nil {
			return err
		}
		spec := Spec{Kind: old.Kind, SemanticKey: key, OwnerKey: old.OwnerKey, ResourceKey: old.ResourceKey, ShardKey: old.ShardKey, Input: json.RawMessage(old.Input), Trace: json.RawMessage(old.Trace), ParentID: &old.ID}
		var err error
		next, err = s.ensure(tx, spec, "manual:"+reason)
		if err != nil {
			return err
		}
		return tx.Model(&Task{}).Where("id=?", id).Updates(map[string]any{"state": Quarantined, "last_error": "explicit redo: " + next.ID.String() + "; " + reason}).Error
	})
	return next, err
}
