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

func (s *Store) RunScheduler(ctx context.Context, b *Broker, holder string, logger *slog.Logger) error {
	return s.RunModule(ctx, "scheduler", holder, func(live context.Context, token uuid.UUID) error {
		for live.Err() == nil {
			if err := s.RenewModule(live, "scheduler", token); err != nil {
				return err
			}
			if err := s.Recover(live, 100); err != nil {
				return err
			}
			if err := s.PrepareCommands(live, 100); err != nil {
				return err
			}
			if err := b.Ready(live); err != nil {
				logger.Warn("async dispatch paused; RabbitMQ unavailable")
				if !Pause(live, 2*time.Second) {
					break
				}
				continue
			}
			for _, queue := range []string{"upload", "object", "media", "gc", "ai", "search"} {
				for range 8 {
					if err := s.RenewModule(live, "scheduler", token); err != nil {
						return err
					}
					t, ok, err := s.Reserve(live, queue, token)
					if err != nil {
						return err
					}
					if !ok {
						break
					}
					if err := b.Publish(live, b.WorkQueue(queue), t.Notice()); err != nil {
						return err
					}
				}
			}
			if !Pause(live, 100*time.Millisecond) {
				break
			}
		}
		return live.Err()
	}, logger)
}

func (s *Store) RunResults(ctx context.Context, b *Broker, holder string, logger *slog.Logger) error {
	return s.RunModule(ctx, "results", holder, func(live context.Context, token uuid.UUID) error {
		for live.Err() == nil {
			if err := b.Ready(live); err != nil {
				if !Pause(live, 2*time.Second) {
					break
				}
				continue
			}
			if err := s.RenewModule(live, "results", token); err != nil {
				return err
			}
			t, ok, err := s.NextResult(live)
			if err != nil {
				return err
			}
			if !ok {
				if !Pause(live, 100*time.Millisecond) {
					break
				}
				continue
			}
			if err := b.Publish(live, b.ResultQueue(t.Shard), t.Notice()); err != nil {
				return err
			}
		}
		return live.Err()
	}, logger)
}

func (s *Store) RunWorker(ctx context.Context, b *Broker, queue string, concurrency int, compute func(context.Context, Task) (json.RawMessage, error), permanent func(error) bool, logger *slog.Logger) error {
	delay := time.Second
	for ctx.Err() == nil {
		err := b.ConsumeOnce(ctx, b.WorkQueue(queue), concurrency, func(parent context.Context, d Delivery) error {
			if !d.Notice.validWire() {
				if err := s.QuarantineMessage(parent, d.Raw, "invalid work notice"); err != nil {
					return err
				}
				return d.Ack()
			}
			t, ok, err := s.Claim(parent, d.Notice)
			if err != nil {
				return err
			}
			if !ok {
				return d.Ack()
			}
			traced, span := BeginTrace(parent, t, "compute")
			defer span.End()
			live, cancel := context.WithTimeout(traced, time.Duration(t.TimeoutMS)*time.Millisecond)
			defer cancel()
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
			body, runErr := safeCompute(live, t, compute)
			persist, c := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
			if runErr == nil {
				_, err = s.SaveResult(persist, t, body)
			} else {
				err = s.Fail(persist, t, runErr, permanent(runErr))
			}
			c()
			cancel()
			<-done
			if err != nil && !errors.Is(err, ErrLost) {
				return err
			}
			if runErr != nil {
				logger.Warn("async computation failed", "task_id", t.ID, "semantic_key", t.SemanticKey, "generation", t.Generation, "attempt", t.Attempts, "error", runErr)
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
		err := b.ConsumeOnce(ctx, b.Prefix+".dead", 1, func(ctx context.Context, d Delivery) error {
			if err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				details, _ := json.Marshal(map[string]any{"notice": d.Notice, "body": d.Raw[:min(len(d.Raw), 1<<20)]})
				identity := fmt.Sprintf("transport:%s:%d:%v", d.Notice.TaskID, d.Notice.DispatchGeneration, d.Notice.ResultID)
				if err := s.quarantine(tx, identity, &d.Notice.TaskID, "RabbitMQ delivery protection exhausted", details); err != nil {
					return err
				}
				if d.Notice.Version != 1 {
					return nil
				}
				if d.Notice.ResultID != nil {
					return tx.Model(&Task{}).Where("id=? AND result_id=? AND state IN ?", d.Notice.TaskID, *d.Notice.ResultID, []string{Computed, Converging}).Updates(map[string]any{"state": NeedsAttention, "last_error": "result transport delivery protection exhausted", "stopped_at": gorm.Expr("clock_timestamp()")}).Error
				}
				// A current active execution may still succeed. Prevent redispatch
				// after it ends; normal recovery performs any required convergence.
				return tx.Model(&Task{}).Where("id=? AND dispatch_generation=? AND state IN ?", d.Notice.TaskID, d.Notice.DispatchGeneration, []string{Ready, RetryWait, Dispatched, Running}).Updates(map[string]any{"max_dispatches": gorm.Expr("LEAST(max_dispatches,dispatches)"), "last_error": "work transport delivery protection exhausted"}).Error
			}); err != nil {
				return err
			}
			logger.Error("async transport stopped", "task_id", d.Notice.TaskID)
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
