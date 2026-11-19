package asynccontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// HandoverDirectBatch intentionally refuses accepted results and unknown command
// commits. Drain those with a compatible v2 build first; never recompute them.
func (s *Store) HandoverDirectBatch(ctx context.Context, limit int) (count int, err error) {
	if s.ProtocolVersion != 3 || limit < 1 || limit > 1000 {
		return 0, ErrInvalid
	}
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var protocol string
		if e := tx.Table("control_settings").Select("value").Where("key='coordination_protocol'").Scan(&protocol).Error; e != nil {
			return e
		}
		if protocol == "v3" {
			// A contracted v3 database no longer has legacy evidence tables.
			return nil
		}
		var live int64
		if e := tx.Raw(`SELECT (SELECT count(*) FROM tasks WHERE state='running' AND lease_until>clock_timestamp())+(SELECT count(*) FROM module_leases WHERE lease_until>clock_timestamp())`).Scan(&live).Error; e != nil {
			return e
		}
		if live > 0 {
			return fmt.Errorf("%d live execution/module leases: stop old writers and wait", live)
		}
		var blocked int64
		if e := tx.Raw(`SELECT count(*) FROM tasks WHERE protocol_version<>3 AND state NOT IN ('done','stopped') AND (protocol_version<>2 OR result_id IS NOT NULL OR commit_attempts>0 OR phase='reconcile')`).Scan(&blocked).Error; e != nil {
			return e
		}
		if blocked > 0 {
			return fmt.Errorf("%d legacy tasks retain accepted results or uncertain command commits; finish with compatible v2 workers before transfer", blocked)
		}
		if e := tx.Table("task_continuations").Where("state='blocked'").Count(&blocked).Error; e != nil {
			return e
		}
		if blocked > 0 {
			return fmt.Errorf("%d frozen continuations still require their original domain conclusion", blocked)
		}
		if e := tx.Exec("UPDATE control_settings SET value='handover-v3' WHERE key='coordination_protocol' AND value IN ('v2','handover-v3')").Error; e != nil {
			return e
		}
		var rows []Task
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("protocol_version=2").Order("id").Limit(limit).Find(&rows).Error; e != nil {
			return e
		}
		var now time.Time
		if e := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; e != nil {
			return e
		}
		for _, t := range rows {
			snapshot, _ := json.Marshal(t)
			// Existing v1 evidence is preserved, with this handover added as a new key.
			if e := tx.Exec(`INSERT INTO coordination_handover(task_id,snapshot,disposition) VALUES(?,jsonb_build_object('v3_handover',?::jsonb),'v3') ON CONFLICT(task_id) DO UPDATE SET snapshot=coordination_handover.snapshot||EXCLUDED.snapshot,disposition='v3'`, t.ID, string(snapshot)).Error; e != nil {
				return e
			}
			t.ProtocolVersion = 3
			if e := s.abandonLegacyOutputRows(tx, t); e != nil {
				return e
			}
			if !Terminal(t.State) {
				// Expired v2 runners have no live credentials after transfer. They
				// must become claimable, rather than running with a NULL lease.
				t.State = Pending
				if t.Attempts >= t.MaxAttempts || t.Dispatches >= t.MaxDispatches || !now.Before(t.Deadline) {
					t.State = Stopped
					t.Outcome = NeedsAttention
					t.LastError = "legacy automatic budget exhausted"
				}
			}
			// Preserve every consumed counter and original deadline. No new commit window.
			t.Phase = "execute"
			if e := tx.Model(&Task{}).Where("id=? AND protocol_version=2", t.ID).Updates(map[string]any{"protocol_version": 3, "phase": t.Phase, "state": t.State, "outcome": t.Outcome, "last_error": t.LastError, "token": nil, "lease_until": nil, "publish_token": nil, "publish_lease_until": nil, "updated_at": now}).Error; e != nil {
				return e
			}
			if !Terminal(t.State) {
				if e := s.delivery(tx, &t, t.NotBefore); e != nil {
					return e
				}
			} else {
				if e := tx.Model(&Task{}).Where("id=?", t.ID).Update("dispatch_state", "stopped").Error; e != nil {
					return e
				}
			}
			count++
		}
		return nil
	})
	return
}

func (s *Store) abandonLegacyOutputRows(tx *gorm.DB, t Task) error {
	// Historical addresses are used verbatim. They are never inferred from the
	// current key formula and are durably handed off before details can be archived.
	var rows []struct {
		Generation int64
		Outputs    json.RawMessage
	}
	if e := tx.Table("executions").Select("generation,outputs").Where("task_id=? AND outputs<>'[]'::jsonb AND outcome<>'cleanup_scheduled'", t.ID).Find(&rows).Error; e != nil {
		return e
	}
	for _, row := range rows {
		var outputs []ExternalOutput
		if e := json.Unmarshal(row.Outputs, &outputs); e != nil {
			return e
		}
		for _, out := range outputs {
			raw, _ := json.Marshal(out)
			_, e := s.ensure(tx, Spec{Kind: "output.delete", SemanticKey: fmt.Sprintf("output.delete:handover-v3:%s:%d:%s", t.ID, row.Generation, out.Key), OwnerKey: t.OwnerKey, ResourceKey: t.ResourceKey, ShardKey: t.ShardKey, Input: raw, NotBefore: time.Now().Add(8 * 24 * time.Hour)}, "legacy-output:"+t.ID.String())
			if e != nil {
				return e
			}
		}
		if e := tx.Table("executions").Where("task_id=? AND generation=?", t.ID, row.Generation).Update("outcome", "cleanup_scheduled").Error; e != nil {
			return e
		}
	}
	return nil
}

func (s *Store) ActivateDirect(ctx context.Context) error {
	if s.ProtocolVersion != 3 {
		return ErrInvalid
	}
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var count int64
		if e := tx.Model(&Task{}).Where("protocol_version<>3").Count(&count).Error; e != nil {
			return e
		}
		if count > 0 {
			return fmt.Errorf("%d legacy tasks remain", count)
		}
		var protocol string
		if e := tx.Table("control_settings").Select("value").Where("key='coordination_protocol'").Scan(&protocol).Error; e != nil {
			return e
		}
		if protocol == "v3" {
			return nil
		}
		if e := tx.Raw(`SELECT count(*) FROM ai_job_observers o LEFT JOIN ai_job_aggregates a ON a.job_id=o.job_id WHERE a.finish_task_id IS NULL`).Scan(&count).Error; e != nil {
			return e
		}
		if count > 0 {
			return fmt.Errorf("%d historical job observers lack a finished closure", count)
		}
		return owned(tx.Exec("UPDATE control_settings SET value='v3' WHERE key='coordination_protocol' AND value IN ('v2','handover-v3','v3')"))
	})
}
