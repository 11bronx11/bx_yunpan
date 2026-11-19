package asynccontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func Pause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// RunModule has a separate lease lifecycle for each module. Renewal failure
// cancels the module immediately; all database transitions remain fenced.
func (s *Store) RunModule(ctx context.Context, name, holder string, run func(context.Context, uuid.UUID) error, logger *slog.Logger) error {
	for ctx.Err() == nil {
		token, ok, err := s.AcquireModule(ctx, name, holder)
		if err != nil || !ok {
			if !Pause(ctx, time.Second) {
				break
			}
			continue
		}
		live, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			ticker := time.NewTicker(s.Lease / 3)
			defer ticker.Stop()
			for {
				select {
				case <-live.Done():
					return
				case <-ticker.C:
					if err := s.RenewModule(live, name, token); err != nil {
						cancel()
						return
					}
				}
			}
		}()
		err = run(live, token)
		cancel()
		<-done
		release, c := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		_ = s.DB.WithContext(release).Exec("UPDATE module_leases SET lease_until=clock_timestamp() WHERE name=? AND token=?", name, token).Error
		c()
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Warn("async module restarting", "module", name, "error", err)
		}
		if !Pause(ctx, time.Second) {
			break
		}
	}
	return ctx.Err()
}

func (r *Relay) Run(ctx context.Context, holder string, minPoll, maxPoll time.Duration, logger *slog.Logger) error {
	return r.Control.RunModule(ctx, "relay", holder, func(live context.Context, token uuid.UUID) error {
		delay := minPoll
		for live.Err() == nil {
			if err := r.Control.RenewModule(live, "relay", token); err != nil {
				return err
			}
			read, cancel := context.WithTimeout(live, 5*time.Second)
			n, err := r.Step(read, token)
			cancel()
			if err != nil {
				return err
			}
			if n > 0 {
				delay = minPoll
				continue
			}
			if !Pause(live, time.Duration(float64(delay)*(0.9+rand.Float64()*0.2))) {
				break
			}
			delay = min(delay*2, maxPoll)
		}
		return live.Err()
	}, logger)
}

// RunScheduler is a due-delivery publisher and lease reaper; RabbitMQ owns
// queue distribution. Concurrent publishers fence individual delivery claims.
func (s *Store) RunScheduler(ctx context.Context, b *Broker, holder string, logger *slog.Logger) error {
	for ctx.Err() == nil {
		if err := s.Recover(ctx, 100); err != nil {
			return err
		}
		for _, queue := range WorkQueues() {
			for range 8 {
				t, ok, err := s.Reserve(ctx, queue)
				if err != nil {
					return err
				}
				if !ok {
					break
				}
				publishErr := b.Publish(ctx, b.WorkQueue(queue), t.Notice())
				persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				err = s.Published(persist, t, publishErr)
				cancel()
				if err != nil {
					return err
				}
				if publishErr != nil {
					logger.Warn("async publication deferred", "queue", queue, "error", publishErr)
					break
				}
			}
		}
		if !Pause(ctx, 100*time.Millisecond) {
			break
		}
	}
	return ctx.Err()
}

// Apply owns a domain-local transaction and returns only a durable decision.
// Control acknowledgement always follows that transaction in this runtime.
type Apply func(context.Context, Task, Result) (string, *time.Time, error)

func (s *Store) RunWorker(ctx context.Context, b *Broker, queue string, concurrency int, compute func(context.Context, Task) (json.RawMessage, error), permanent func(error) bool, logger *slog.Logger, apply ...Apply) error {
	delay := time.Second
	for ctx.Err() == nil {
		err := b.ConsumeOnce(ctx, b.WorkQueue(queue), concurrency, func(parent context.Context, d Delivery) error {
			if !d.Notice.validWire() {
				if err := s.QuarantineMessage(parent, d.Raw, "invalid v2 work notice"); err != nil {
					return err
				}
				return d.Ack()
			}
			t, ok, err := s.Claim(parent, d.Notice)
			if errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalid) {
				if err := s.QuarantineMessage(parent, d.Raw, err.Error()); err != nil {
					return err
				}
				return d.Ack()
			}
			if err != nil {
				return err
			}
			if !ok {
				return d.Ack()
			}
			traced, span := BeginTrace(parent, t, "execute")
			defer span.End()
			live, cancel := context.WithTimeout(traced, time.Duration(t.TimeoutMS)*time.Millisecond)
			done := make(chan struct{})
			go func() {
				defer close(done)
				ticker := time.NewTicker(s.Lease / 3)
				defer ticker.Stop()
				for {
					select {
					case <-live.Done():
						return
					case <-ticker.C:
						if err := s.Renew(live, t); err != nil {
							cancel()
							return
						}
					}
				}
			}()
			// t is not mutated while the renewer reads it; Renew only needs its token
			// and generation, which stay fixed across checkpoint acceptance.
			current := t
			runErr := s.execute(live, &current, compute, apply)
			cancel()
			<-done
			if runErr != nil {
				persist, c := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
				err = s.Fail(persist, current, runErr, permanent(runErr))
				c()
				if err != nil && !errors.Is(err, ErrLost) {
					return err
				}
				logger.Warn("async task deferred or stopped", "task_id", t.ID, "phase", current.Phase, "error", runErr)
			}
			return d.Ack()
		})
		if ctx.Err() != nil {
			break
		}
		logger.Warn("async worker reconnecting", "queue", queue, "error", err)
		if !Pause(ctx, delay) {
			break
		}
		delay = min(delay*2, 10*time.Second)
	}
	return ctx.Err()
}

func (s *Store) execute(ctx context.Context, t *Task, compute func(context.Context, Task) (json.RawMessage, error), apply []Apply) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("async execution panic: %v", p)
		}
	}()
	var r Result
	if t.ResultID != nil {
		r, err = s.checkpoint(ctx, *t)
	} else if Command(t.Kind) {
		r = CommandResult(*t)
	} else {
		var body json.RawMessage
		body, err = safeCompute(ctx, *t, compute)
		if err == nil {
			r, err = s.SaveResult(ctx, *t, body)
		}
		if err == nil {
			err = s.BeginCommit(ctx, t, r)
		}
	}
	if err != nil {
		return err
	}
	outcome := Succeeded
	var due *time.Time
	if t.EffectTarget != EffectNone {
		if len(apply) != 1 || apply[0] == nil {
			return ErrInvalid
		}
		outcome, due, err = apply[0](ctx, *t, r)
		if err != nil {
			return err
		}
	}
	if due != nil {
		return s.Reschedule(ctx, *t, r, *due)
	}
	return s.Report(ctx, *t, r, outcome)
}

func safeCompute(ctx context.Context, t Task, compute func(context.Context, Task) (json.RawMessage, error)) (body json.RawMessage, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("compute panic: %v", p)
		}
	}()
	return compute(ctx, t)
}

func (s *Store) QuarantineMessage(ctx context.Context, body []byte, reason string) error {
	digest, _, err := Digest(body)
	if err != nil {
		digest = fmt.Sprintf("invalid:%x", body[:min(len(body), 128)])
	}
	details, _ := json.Marshal(map[string]any{"bytes": len(body)})
	return s.quarantine(s.DB.WithContext(ctx), "message:"+digest, nil, reason, details)
}

func (s *Store) RunDeadLetters(ctx context.Context, b *Broker, logger *slog.Logger) error {
	for ctx.Err() == nil {
		err := b.ConsumeOnce(ctx, b.Prefix+".dead", 1, func(c context.Context, d Delivery) error {
			if err := s.HandleDead(c, d.Notice, d.Raw); err != nil {
				return err
			}
			return d.Ack()
		})
		if ctx.Err() != nil {
			break
		}
		logger.Warn("async dead letter consumer reconnecting", "error", err)
		if !Pause(ctx, 2*time.Second) {
			break
		}
	}
	return ctx.Err()
}

func (s *Store) HandleDead(ctx context.Context, n Notice, raw []byte) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.QuarantineMessage(ctx, raw, "transport exhausted"); err != nil {
			return err
		}
		var t Task
		q := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", n.TaskID).Find(&t)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected == 0 || !n.validWire() || !n.matches(t) || n.DispatchGeneration != t.DispatchGeneration || Terminal(t.State) || t.State == Running {
			return nil
		}
		var now time.Time
		if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return err
		}
		if t.Dispatches >= t.MaxDispatches {
			return s.stop(tx, &t, "dead transport exhausted", now)
		}
		return s.retry(tx, &t, now.Add(s.RetryBase), "dead notification recovered")
	})
}
