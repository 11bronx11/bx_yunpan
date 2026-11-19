package asynccontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// HandoverBatch is resumable. A transferred row and its original snapshot,
// tightened budgets and first delivery are committed atomically.
func (s *Store) HandoverBatch(ctx context.Context, limit int) (count int, err error) {
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var live int64
		if err := tx.Model(&Task{}).Where("protocol_version=1 AND state='running' AND lease_until>clock_timestamp()").Count(&live).Error; err != nil {
			return err
		}
		if live > 0 {
			return fmt.Errorf("%d legacy executions still hold live leases", live)
		}
		if err := tx.Table("module_leases").Where("lease_until>clock_timestamp()").Count(&live).Error; err != nil {
			return err
		}
		if live > 0 {
			return fmt.Errorf("%d legacy module leases remain live; stop writers and wait for expiry", live)
		}
		if err := tx.Exec("UPDATE control_settings SET value='handover' WHERE key='coordination_protocol' AND value='v1'").Error; err != nil {
			return err
		}
		var rows []Task
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("protocol_version=1").Order("id").Limit(limit).Find(&rows).Error; err != nil {
			return err
		}
		var now time.Time
		if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return err
		}
		for _, t := range rows {
			snapshot, _ := json.Marshal(t)
			old := t.State
			needsConvergence := old == Exhausted
			t.ProtocolVersion = 2
			t.Outcome = ""
			t.State = Pending
			t.Phase = "compute"
			if Command(t.Kind) {
				t.Phase = "command"
				t.Queue = "core-command"
			}
			if d, known := s.Registry[t.Kind]; !known {
				t.State = Stopped
				t.Outcome = Quarantined
				t.LastError = "unknown legacy contract"
			} else {
				t.Queue = d.Queue
			}
			if t.Queue == "ai" || t.Queue == "search" {
				t.State = Stopped
				t.Outcome = Quarantined
				t.LastError = "retired compatibility queue requires explicit domain redo"
			}
			switch old {
			case Succeeded, Superseded, Rejected, Failed:
				t.State = Done
				t.Outcome = old
			case Quarantined, NeedsAttention:
				t.State = Stopped
				t.Outcome = old
			case Exhausted:
				t.State = Stopped
				t.Outcome = NeedsAttention
				t.LastError = "legacy exhaustion requires failure convergence"
			}
			if t.ResultID != nil && !Terminal(t.State) {
				var r Result
				checkpointErr := tx.Where("id=? AND task_id=? AND generation=? AND input_digest=? AND effect_target=?", *t.ResultID, t.ID, t.Generation, t.InputDigest, t.EffectTarget).Take(&r).Error
				if checkpointErr != nil && !errors.Is(checkpointErr, gorm.ErrRecordNotFound) {
					return checkpointErr
				}
				digest, _, digestErr := Digest(r.Body)
				if checkpointErr != nil || digestErr != nil || digest != r.ResultDigest {
					t.State = Stopped
					t.Outcome = Quarantined
					t.LastError = "legacy checkpoint cannot be proven"
				} else {
					t.Phase = "commit"
					if t.CommitDeadline == nil {
						t.State = Stopped
						t.Outcome = NeedsAttention
						t.LastError = "legacy checkpoint has no commit deadline"
					}
				}
			} else if old == Computed || old == Converging {
				t.State = Stopped
				t.Outcome = Quarantined
				t.LastError = "legacy computed task has no checkpoint"
			}
			// Never grant a historical task more automatic opportunities than either
			// of its old remaining execution/recovery/result-notification budgets.
			if !Terminal(t.State) && t.ResultID == nil && !Command(t.Kind) && (t.Attempts >= t.MaxAttempts || t.Recoveries >= t.MaxRecoveries || t.Dispatches >= t.MaxDispatches) {
				t.State, t.Outcome, t.LastError = Stopped, NeedsAttention, "legacy automatic budget exhausted"
				needsConvergence = true
			}
			t.MaxAttempts = min(t.MaxAttempts, max(1, t.Attempts+max(0, t.MaxRecoveries-t.Recoveries)))
			t.MaxCommitAttempts = max(1, min(t.MaxCommitAttempts, max(0, t.MaxResultSends-t.ResultSends)))
			if t.ResultID != nil && t.ResultSends >= t.MaxResultSends && !Terminal(t.State) {
				t.Phase = "reconcile"
			}
			t.Token = nil
			t.LeaseUntil = nil
			if err := tx.Model(&Task{}).Where("id=? AND protocol_version=1", t.ID).Updates(map[string]any{"protocol_version": 2, "state": t.State, "outcome": t.Outcome, "phase": t.Phase, "queue": t.Queue, "max_attempts": t.MaxAttempts, "max_commit_attempts": t.MaxCommitAttempts, "token": nil, "lease_until": nil, "last_error": t.LastError, "updated_at": now}).Error; err != nil {
				return err
			}
			if err := tx.Exec("INSERT INTO coordination_handover(task_id,snapshot,disposition) VALUES(?,?::jsonb,?) ON CONFLICT DO NOTHING", t.ID, string(snapshot), t.State).Error; err != nil {
				return err
			}
			if !Terminal(t.State) {
				if err := s.delivery(tx, &t, t.NotBefore); err != nil {
					return err
				}
			}
			if needsConvergence {
				t.State = Pending
				if err := s.stop(tx, &t, t.LastError, now); err != nil {
					return err
				}
			}
			count++
		}
		return nil
	})
	return
}

// TransferMessage records durable proof before its caller ACKs the legacy MQ
// notification. Repeated drains never change task budgets or checkpoint IDs.
func (s *Store) TransferMessage(ctx context.Context, queue string, body []byte) error {
	if len(body) > 1<<20 {
		return fmt.Errorf("legacy message exceeds 1 MiB; retained unacknowledged in %s for manual review", queue)
	}
	sum := sha256.Sum256(append([]byte(queue+":"), body...))
	key := hex.EncodeToString(sum[:])
	var n Notice
	valid := len(body) <= 1<<20 && json.Unmarshal(body, &n) == nil && n.Version == 1
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		disposition := "quarantined"
		if valid {
			var t Task
			q := tx.Where("id=?", n.TaskID).Find(&t)
			if q.Error != nil {
				return q.Error
			}
			if q.RowsAffected == 1 && t.ProtocolVersion == 2 && n.SemanticKey == t.SemanticKey && n.InputDigest == t.InputDigest && n.ShardKey == t.ShardKey && n.Shard == t.Shard && n.LayoutVersion == t.LayoutVersion && (n.EffectTarget == t.EffectTarget || n.EffectTarget == "" && t.EffectTarget != EffectAI) {
				if n.ResultID == nil || t.ResultID != nil && *n.ResultID == *t.ResultID {
					disposition = "transferred"
				}
			}
		}
		details, _ := json.Marshal(map[string]any{"notice": n, "queue": queue, "raw_base64": body, "bytes": len(body), "sha256": key})
		if disposition == "quarantined" {
			if err := s.quarantine(tx, "handover-message:"+key, nil, "legacy notification identity cannot be proven", details); err != nil {
				return err
			}
		}
		return tx.Exec("INSERT INTO coordination_messages(identity,notice,disposition) VALUES(?,?::jsonb,?) ON CONFLICT DO NOTHING", key, string(details), disposition).Error
	})
}

func LegacyQueues(prefix string) []string {
	queues := []string{prefix + ".dead"}
	for _, q := range []string{"upload", "object", "media", "gc", "ai", "search", "ai-content", "ai-embedding", "ai-summary", "ai-project-text", "ai-project-vector", "ai-maintenance"} {
		queues = append(queues, prefix+".work."+q)
	}
	for shard := range 32 {
		queues = append(queues, fmt.Sprintf("%s.result.%02d", prefix, shard), fmt.Sprintf("%s.result.ai.%02d", prefix, shard))
	}
	return queues
}

func (s *Store) DrainLegacy(ctx context.Context, url, prefix string) error {
	transport := NewBroker(url, prefix)
	conn, socket, err := transport.dial(ctx)
	if err != nil {
		return fmt.Errorf("legacy RabbitMQ connection failed")
	}
	defer socket.Close()
	stop := context.AfterFunc(ctx, func() { _ = socket.Close() })
	defer stop()
	defer conn.Close()
	for _, queue := range LegacyQueues(prefix) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ch, err := conn.Channel()
		if err != nil {
			return err
		}
		info, err := ch.QueueInspect(queue)
		if err != nil {
			ch.Close()
			var ae *amqp.Error
			if errorsAsAMQP(err, &ae) && ae.Code == 404 {
				continue
			}
			return err
		}
		if info.Consumers > 0 {
			ch.Close()
			return fmt.Errorf("legacy queue %s still has consumers", queue)
		}
		for {
			msg, ok, err := ch.Get(queue, false)
			if err != nil {
				ch.Close()
				return err
			}
			if !ok {
				break
			}
			if err := s.TransferMessage(ctx, queue, msg.Body); err != nil {
				ch.Close()
				return err
			}
			if err := msg.Ack(false); err != nil {
				ch.Close()
				return err
			}
		}
		ch.Close()
	}
	return nil
}
func errorsAsAMQP(err error, target **amqp.Error) bool { return errors.As(err, target) }
func (s *Store) Activate(ctx context.Context) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&Task{}).Where("protocol_version<>2").Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("%d tasks still need handover", count)
		}
		return tx.Exec("UPDATE control_settings SET value='v2' WHERE key='coordination_protocol' AND value IN ('handover','v1','v2')").Error
	})
}
