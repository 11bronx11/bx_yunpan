package asynccontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
)

// ScanExpiry is the only online business scan in the new core. It reads a
// bounded keyset and reliably ensures coordination commands in the control DB.
func (s *Store) ScanExpiry(ctx context.Context, business *gorm.DB, token uuid.UUID, limit int) error {
	name := "upload-expiry"
	if err := s.DB.WithContext(ctx).Exec("INSERT INTO scan_positions(name) VALUES(?) ON CONFLICT DO NOTHING", name).Error; err != nil {
		return err
	}
	var p struct {
		CursorTime time.Time
		CursorID   uuid.UUID
	}
	if err := s.DB.WithContext(ctx).Table("scan_positions").Where("name=?", name).Scan(&p).Error; err != nil {
		return err
	}
	var rows []struct {
		ID, UserID uuid.UUID
		ExpiresAt  time.Time
	}
	if err := business.WithContext(ctx).Raw(`SELECT id,user_id,expires_at FROM upload_sessions WHERE status IN ('initializing','created','uploading') AND expires_at<=clock_timestamp() AND (expires_at,id)>(?,?) ORDER BY expires_at,id LIMIT ?`, p.CursorTime, p.CursorID, limit).Scan(&rows).Error; err != nil {
		return err
	}
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var valid int64
		if err := tx.Raw("SELECT count(*) FROM module_leases WHERE name='expiry-scan' AND token=? AND lease_until>clock_timestamp()", token).Scan(&valid).Error; err != nil {
			return err
		}
		if valid != 1 {
			return ErrLost
		}
		for _, row := range rows {
			input, _ := json.Marshal(asyncprotocol.Input{SessionID: row.ID, OwnerID: row.UserID})
			spec := Spec{Kind: "upload.expire", SemanticKey: "upload.expire:" + row.ID.String(), OwnerKey: "owner:" + row.UserID.String(), ResourceKey: "session:" + row.ID.String(), ShardKey: "owner:" + row.UserID.String(), Input: input, NotBefore: row.ExpiresAt}
			task, err := s.ensure(tx, spec, "scan:"+row.ID.String())
			if err != nil {
				return err
			}
			if Terminal(task.State) {
				if err := s.quarantine(tx, "expiry-contradiction:"+row.ID.String(), &task.ID, "terminal expiry task but session is still eligible", input); err != nil {
					return err
				}
			}
		}
		if len(rows) < limit {
			return owned(tx.Exec("UPDATE scan_positions SET cursor_time='epoch',cursor_id=?,updated_at=clock_timestamp() WHERE name=? AND EXISTS(SELECT 1 FROM module_leases WHERE name='expiry-scan' AND token=? AND lease_until>clock_timestamp())", uuid.Nil, name, token))
		}
		last := rows[len(rows)-1]
		return owned(tx.Exec("UPDATE scan_positions SET cursor_time=?,cursor_id=?,updated_at=clock_timestamp() WHERE name=? AND EXISTS(SELECT 1 FROM module_leases WHERE name='expiry-scan' AND token=? AND lease_until>clock_timestamp())", last.ExpiresAt, last.ID, name, token))
	})
}

func (s *Store) Maintain(ctx context.Context, limit int, retention time.Duration) error {
	if s.ProtocolVersion == 3 {
		return s.maintainDirect(ctx, limit, retention)
	}
	// Keep compact semantic identities, source relationships, budgets, receipts
	// and stops forever. Only completed detail payloads are bounded here.
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`DELETE FROM executions WHERE (task_id,generation) IN (
   SELECT e.task_id,e.generation FROM executions e JOIN tasks t ON t.id=e.task_id
   WHERE e.finished_at<clock_timestamp()-(?*interval '1 millisecond')
   AND t.state IN ('done') ORDER BY e.finished_at LIMIT ?
  )`, retention.Milliseconds(), limit).Error; err != nil {
			return err
		}
		if err := tx.Exec(`UPDATE results SET body='{"archived":true}'::jsonb WHERE id IN (
   SELECT r.id FROM results r JOIN tasks t ON t.id=r.task_id WHERE r.committed_at<clock_timestamp()-(?*interval '1 millisecond')
   AND t.state IN ('done') AND r.body<>'{"archived":true}'::jsonb ORDER BY r.committed_at LIMIT ?
  )`, retention.Milliseconds(), limit).Error; err != nil {
			return err
		}
		if err := tx.Exec(`UPDATE tasks SET input='{"archived":true}'::jsonb,trace='{}'::jsonb WHERE id IN (
 SELECT id FROM tasks WHERE state IN ('done') AND updated_at<clock_timestamp()-(?*interval '1 millisecond') AND input<>'{"archived":true}'::jsonb ORDER BY updated_at LIMIT ?
 )`, retention.Milliseconds(), limit).Error; err != nil {
			return err
		}
		// Keep the event identity, digest, frozen subscription identities and source
		// links. Never archive input needed by pending, stopped or unreported work.
		return tx.Exec(`UPDATE event_inbox SET payload='{"archived":true}'::jsonb,plan=jsonb_build_object('archived',true,'subscriptions',COALESCE((SELECT jsonb_agg(jsonb_build_object('task_id',t.id,'kind',t.kind,'input_digest',t.input_digest)) FROM task_sources s JOIN tasks t ON t.id=s.task_id WHERE s.source_key='event:'||event_inbox.id::text),'[]'::jsonb)) WHERE id IN (
 SELECT e.id FROM event_inbox e WHERE e.status='accepted' AND e.created_at<clock_timestamp()-(?*interval '1 millisecond') AND e.payload<>'{"archived":true}'::jsonb
 AND NOT EXISTS(SELECT 1 FROM task_sources s JOIN tasks t ON t.id=s.task_id WHERE s.source_key='event:'||e.id::text AND t.state NOT IN ('done')) ORDER BY e.created_at LIMIT ?
 )`, retention.Milliseconds(), limit).Error
	})
}

func (s *Store) CheckProtocol(ctx context.Context) error {
	var protocol string
	if err := s.DB.WithContext(ctx).Table("control_settings").Select("value").Where("key='coordination_protocol'").Scan(&protocol).Error; err != nil {
		return err
	}
	if protocol != fmt.Sprintf("v%d", s.ProtocolVersion) {
		return fmt.Errorf("coordination handover incomplete (%s); run async-handover with old writers stopped", protocol)
	}
	return nil
}
