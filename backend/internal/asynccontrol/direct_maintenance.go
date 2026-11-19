package asynccontrol

import (
	"context"
	"time"

	"gorm.io/gorm"
)

func (s *Store) RecoverLegacyOutputs(ctx context.Context, limit int, safety time.Duration) error {
	if s.ProtocolVersion == 3 {
		return nil
	} // v3 handover must take over every old output first.
	return s.RecoverOutputs(ctx, limit, safety)
}

func (s *Store) maintainDirect(ctx context.Context, limit int, retention time.Duration) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if e := tx.Exec(`UPDATE tasks SET input='{"archived":true}'::jsonb,trace='{}'::jsonb WHERE id IN (
 SELECT id FROM tasks WHERE protocol_version=3 AND state='done' AND updated_at<clock_timestamp()-(?*interval '1 millisecond') AND input<>'{"archived":true}'::jsonb ORDER BY updated_at LIMIT ?
 )`, retention.Milliseconds(), limit).Error; e != nil {
			return e
		}
		return tx.Exec(`UPDATE event_inbox SET payload='{"archived":true}'::jsonb,plan=jsonb_build_object('archived',true) WHERE id IN (
 SELECT e.id FROM event_inbox e WHERE e.status='accepted' AND e.created_at<clock_timestamp()-(?*interval '1 millisecond') AND e.payload<>'{"archived":true}'::jsonb
 AND NOT EXISTS(SELECT 1 FROM task_sources s JOIN tasks t ON t.id=s.task_id WHERE s.source_key='event:'||e.id::text AND t.state<>'done') ORDER BY e.created_at LIMIT ?
 )`, retention.Milliseconds(), limit).Error
	})
}
