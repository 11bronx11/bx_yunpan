package asynccontrol

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

func (s *Store) WritePermit(ctx context.Context, shard int) (uuid.UUID, bool, error) {
	token := uuid.New()
	ok := false
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var budget struct {
			ConcurrentLimit, RatePerSecond int
			AvailableTokens                float64
			RefilledAt, PressureUntil      time.Time
		}
		if err := tx.Raw("SELECT * FROM write_budget WHERE singleton FOR UPDATE").Scan(&budget).Error; err != nil {
			return err
		}
		var now time.Time
		if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return err
		}
		rate := float64(budget.RatePerSecond)
		if budget.PressureUntil.After(now) {
			rate = max(1, rate/2)
		} else {
			// Ten healthy seconds are followed by a gradual ten-second recovery.
			// Repeated pressure extends the hold instead of oscillating each tick.
			rate = max(1, rate*min(1, 0.5+now.Sub(budget.PressureUntil).Seconds()/20))
		}
		available := min(rate, budget.AvailableTokens+now.Sub(budget.RefilledAt).Seconds()*rate)
		var active int64
		if err := tx.Table("write_permits").Where("lease_until>clock_timestamp()").Count(&active).Error; err != nil {
			return err
		}
		if active >= int64(budget.ConcurrentLimit) || available < 1 {
			return nil
		}
		if err := tx.Exec("DELETE FROM write_permits WHERE shard=? AND lease_until<=clock_timestamp()", shard).Error; err != nil {
			return err
		}
		q := tx.Exec("INSERT INTO write_permits(token,shard,lease_until) VALUES(?,?,clock_timestamp()+(?*interval '1 millisecond')) ON CONFLICT(shard) DO NOTHING", token, shard, s.Lease.Milliseconds())
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected == 0 {
			return nil
		}
		if err := tx.Exec("UPDATE write_budget SET available_tokens=?,refilled_at=? WHERE singleton", available-1, now).Error; err != nil {
			return err
		}
		ok = true
		return nil
	})
	return token, ok, err
}

func (s *Store) RunCommitters(ctx context.Context, b *Broker, holder string, commit func(context.Context, Notice) error, logger *slog.Logger) error {
	return s.RunCommittersFor(ctx, b, EffectCore, holder, commit, logger)
}

func (s *Store) RunCommittersFor(ctx context.Context, b *Broker, target, holder string, commit func(context.Context, Notice) error, logger *slog.Logger) error {
	if target != EffectCore && target != EffectAI {
		return ErrInvalid
	}
	type shardRun struct {
		cancel context.CancelFunc
		done   chan struct{}
	}
	defer func() {
		release, c := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer c()
		_ = s.DB.WithContext(release).Exec("UPDATE committer_members SET lease_until=clock_timestamp() WHERE id=? AND effect_target=?", holder, target).Error
	}()
	active := map[int]shardRun{}
	var mu sync.Mutex
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, run := range active {
			run.cancel()
		}
		for _, run := range active {
			<-run.done
		}
	}()
	for ctx.Err() == nil {
		q := s.DB.WithContext(ctx).Exec(`INSERT INTO committer_members(id,effect_target,lease_until) VALUES(?,?,clock_timestamp()+(?*interval '1 millisecond')) ON CONFLICT(id,effect_target) DO UPDATE SET lease_until=EXCLUDED.lease_until,updated_at=clock_timestamp()`, holder, target, s.Lease.Milliseconds())
		if q.Error != nil {
			mu.Lock()
			for shard, run := range active {
				run.cancel()
				<-run.done
				delete(active, shard)
			}
			mu.Unlock()
			if !Pause(ctx, time.Second) {
				break
			}
			continue
		}
		var members []string
		if err := s.DB.WithContext(ctx).Table("committer_members").Where("lease_until>clock_timestamp() AND effect_target=?", target).Order("id").Pluck("id", &members).Error; err != nil {
			return err
		}
		index := slices.Index(members, holder)
		if index < 0 {
			return ErrLost
		}
		mu.Lock()
		for shard, run := range active {
			select {
			case <-run.done:
				delete(active, shard)
				continue
			default:
			}
			if shard%len(members) != index {
				run.cancel()
				<-run.done
				delete(active, shard)
			}
		}
		for shard := 0; shard < s.Shards; shard++ {
			if shard%len(members) != index {
				continue
			}
			if _, ok := active[shard]; ok {
				continue
			}
			live, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			active[shard] = shardRun{cancel, done}
			go func(shard int) {
				defer close(done)
				_ = s.RunModule(live, fmt.Sprintf("committer:%s:%d", target, shard), holder, func(ownedCtx context.Context, token uuid.UUID) error {
					logger.Info("async result shard acquired", "shard", shard, "holder", holder)
					return b.ConsumeBatchesOnce(ownedCtx, b.ResultQueueFor(target, shard), max(1, s.BatchRows), max(1024, s.BatchBytes), s.BatchWait, func(ctx context.Context, deliveries []Delivery) error {
						var notices []Notice
						var accepted []Delivery
						for _, d := range deliveries {
							if !d.Notice.validWire() || d.Notice.ResultID == nil || d.Notice.Shard != shard || (target == EffectAI && d.Notice.EffectTarget != target) || (target == EffectCore && d.Notice.EffectTarget == EffectAI) {
								if err := s.QuarantineMessage(ctx, d.Raw, "invalid result notice"); err != nil {
									return err
								}
								if err := d.Ack(); err != nil {
									return err
								}
								continue
							}
							notices = append(notices, d.Notice)
							accepted = append(accepted, d)
						}
						if len(notices) == 0 {
							return nil
						}
						var permit uuid.UUID
						for ctx.Err() == nil {
							p, ok, err := s.WritePermit(ctx, shard+map[string]int{EffectAI: s.Shards}[target])
							if err != nil {
								return err
							}
							if ok {
								permit = p
								break
							}
							if !Pause(ctx, 20*time.Millisecond) {
								return ctx.Err()
							}
						}
						if permit == uuid.Nil {
							return ctx.Err()
						}
						commitCtx, stopCommit := context.WithCancel(ctx)
						renewed := make(chan struct{})
						go func() {
							defer close(renewed)
							ticker := time.NewTicker(s.Lease / 3)
							defer ticker.Stop()
							for {
								select {
								case <-commitCtx.Done():
									return
								case <-ticker.C:
									bounded, cancel := context.WithTimeout(commitCtx, s.Lease/6)
									err := owned(s.critical().WithContext(bounded).Exec("UPDATE write_permits SET lease_until=clock_timestamp()+(?*interval '1 millisecond') WHERE token=? AND lease_until>clock_timestamp()", s.Lease.Milliseconds(), permit))
									cancel()
									if err != nil {
										stopCommit()
										return
									}
								}
							}
						}()
						started := time.Now()
						var errs []error
						if s.BatchCommit != nil {
							errs = s.BatchCommit(commitCtx, notices)
						} else {
							for _, n := range notices {
								errs = append(errs, commit(commitCtx, n))
							}
						}
						stopCommit()
						<-renewed
						var commitErr error
						for _, e := range errs {
							if e != nil {
								commitErr = e
								break
							}
						}
						persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
						if commitErr != nil || time.Since(started) > time.Second {
							_ = s.DB.WithContext(persist).Exec("UPDATE write_budget SET pressure_until=clock_timestamp()+interval '10 seconds' WHERE singleton").Error
							_ = s.DB.WithContext(persist).Exec("UPDATE queue_budgets SET paused_until=clock_timestamp()+interval '10 seconds' WHERE queue IN ('upload','object','media','ai','search')").Error
						}
						_ = s.DB.WithContext(persist).Exec("DELETE FROM write_permits WHERE token=?", permit).Error
						cancel()
						for i, e := range errs {
							if e == nil {
								if err := accepted[i].Ack(); err != nil {
									return err
								}
							}
						}
						return commitErr
					})
				}, logger)
			}(shard)
		}
		mu.Unlock()
		if !Pause(ctx, 2*time.Second) {
			break
		}
	}
	return ctx.Err()
}
