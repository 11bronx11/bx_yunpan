package asyncjobs

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
)

// TransferLegacyMessages is a cutover-only bounded consumer. It never executes
// an old payload, creates TTL retries or purges queues. Every ACK follows a
// durable transfer/quarantine record. Old retry queues are drained first.
func TransferLegacyMessages(ctx context.Context, url string, s *asynccontrol.Store, testPrefix ...string) error {
	queues := []string{}
	for _, q := range []string{"object", "media", "maintenance", "upload", "ai", "search"} {
		for _, delay := range []string{"1000", "5000", "30000", "60000", "300000"} {
			queues = append(queues, q+".retry."+delay)
		}
		queues = append(queues, q+".dlq")
	}
	queues = append(queues, "object", "media", "maintenance", "upload", "ai", "search")
	if len(testPrefix) > 0 {
		for i := range queues {
			queues[i] = testPrefix[0] + queues[i]
		}
	}
	return transferLegacyMessages(ctx, url, s, queues)
}

// TransferLegacyAIMessages handles only the retired AI/search lanes. It does
// not repeat core import or consume shared core result shards.
func TransferLegacyAIMessages(ctx context.Context, url string, s *asynccontrol.Store, prefix string) error {
	queues := []string{}
	for _, q := range []string{"ai", "search"} {
		for _, delay := range []string{"1000", "5000", "30000", "60000", "300000"} {
			queues = append(queues, q+".retry."+delay)
		}
		queues = append(queues, q+".dlq", q, prefix+".work."+q)
	}
	return transferLegacyMessages(ctx, url, s, queues)
}
func transferLegacyMessages(ctx context.Context, url string, s *asynccontrol.Store, queues []string) error {
	conn, err := amqp.DialConfig(url, amqp.Config{Heartbeat: 10 * time.Second, Locale: "en_US"})
	if err != nil {
		return errors.New("legacy broker unavailable")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	total := 0
	for pass := 0; pass < 2; pass++ {
		for _, queue := range queues {
			ch, err := conn.Channel()
			if err != nil {
				return err
			}
			state, err := ch.QueueInspect(queue)
			if err != nil {
				ch.Close()
				var protocol *amqp.Error
				if errors.As(err, &protocol) && protocol.Code == 404 {
					continue
				}
				return err
			}
			if state.Consumers != 0 {
				ch.Close()
				return fmt.Errorf("legacy queue %s still has consumers; stop old authority", queue)
			}
			for i := 0; i < state.Messages; i++ {
				if total >= 100000 {
					ch.Close()
					return errors.New("legacy transfer reached bound; rerun import")
				}
				m, ok, err := ch.Get(queue, false)
				if err != nil {
					ch.Close()
					return err
				}
				if !ok {
					break
				}
				total++
				if len(m.Body) > 1<<20 {
					ch.Close()
					return errors.New("oversized legacy message retained for manual transfer")
				}
				sum := sha256.Sum256(m.Body)
				identity := fmt.Sprintf("legacy-mq:%s:%s:%x", queue, m.MessageId, sum)
				err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
					var accepted int64
					eventID, parseErr := uuid.Parse(strings.SplitN(m.MessageId, ":", 2)[0])
					if parseErr == nil {
						if err := tx.Table("event_inbox").Where("id=?", eventID).Count(&accepted).Error; err != nil {
							return err
						}
					}
					reason := "legacy message transferred to imported event ownership"
					if accepted == 0 {
						reason = "unmapped legacy message; manual review required"
					}
					raw, _ := json.Marshal(map[string]any{"queue": queue, "message_id": m.MessageId, "kind": m.Type, "payload": json.RawMessage(m.Body), "headers": m.Headers})
					if len(raw) == 0 {
						raw, _ = json.Marshal(map[string]any{"queue": queue, "message_id": m.MessageId, "payload_bytes": m.Body})
					}
					if err := tx.Exec(`INSERT INTO quarantines(identity,reason,details) VALUES(?,?,?::jsonb) ON CONFLICT DO NOTHING`, identity, reason, string(raw)).Error; err != nil {
						return err
					}
					retry := 0
					switch v := m.Headers["x-retry-count"].(type) {
					case int32:
						retry = int(v)
					case int64:
						retry = int(v)
					case int:
						retry = v
					}
					return tx.Exec(`UPDATE tasks SET attempts=GREATEST(attempts,?),updated_at=clock_timestamp() WHERE generation=0 AND id IN (SELECT task_id FROM task_sources WHERE source_key=?)`, max(0, retry+1), "legacy-task:"+m.MessageId).Error
				})
				if err != nil {
					ch.Close()
					return err
				}
				if err := m.Ack(false); err != nil {
					ch.Close()
					return err
				}
			}
			ch.Close()
		}
	}
	return nil
}
