package asynccontrol

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

func (s *Store) RunDirectWorker(ctx context.Context, b *Broker, queue string, concurrency int, h DirectHandler, permanent func(error) bool, logger *slog.Logger) error {
	if s.ProtocolVersion != 3 || h == nil {
		return ErrInvalid
	}
	for ctx.Err() == nil {
		err := b.ConsumeOnce(ctx, b.WorkQueue(queue), concurrency, func(parent context.Context, d Delivery) error {
			if !d.Notice.validWire() || d.Notice.Version != 3 {
				if e := s.QuarantineMessage(parent, d.Raw, "invalid v3 notice"); e != nil {
					return e
				}
				return d.Ack()
			}
			t, claimed, err := s.Claim(parent, d.Notice)
			if errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalid) {
				if e := s.QuarantineMessage(parent, d.Raw, err.Error()); e != nil {
					return e
				}
				return d.Ack()
			}
			if err != nil {
				return err
			}
			if !claimed {
				return d.Ack()
			}
			traced, span := BeginTrace(parent, t, "direct_execute")
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
						if s.Renew(live, t) != nil {
							cancel()
							return
						}
					}
				}
			}()
			runErr := s.executeDirect(live, t, h)
			cancel()
			<-done
			if runErr != nil {
				persist, c := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
				err = s.Fail(persist, t, runErr, permanent(runErr))
				c()
				if err != nil && !errors.Is(err, ErrLost) {
					return err
				}
				logger.Warn("async direct task deferred or stopped", "task_id", t.ID, "error", runErr)
			}
			return d.Ack()
		})
		if ctx.Err() != nil {
			break
		}
		logger.Warn("async direct worker reconnecting", "queue", queue, "error", err)
		if !Pause(ctx, time.Second) {
			break
		}
	}
	return ctx.Err()
}
