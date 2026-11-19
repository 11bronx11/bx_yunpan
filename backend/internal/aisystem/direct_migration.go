package aisystem

import (
	"context"
	"fmt"
	"strings"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DirectMigrator is offline tooling. It receives no business connection.
type DirectMigrator struct {
	DB      *gorm.DB
	Control *asynccontrol.Store
}

func (m *DirectMigrator) stopped(ctx context.Context) error {
	var count int64
	if e := m.Control.DB.WithContext(ctx).Raw(`SELECT (SELECT count(*) FROM tasks WHERE protocol_version<>3)+(SELECT count(*) FROM tasks WHERE state='running' AND lease_until>clock_timestamp())+(SELECT count(*) FROM module_leases WHERE lease_until>clock_timestamp())`).Scan(&count).Error; e != nil {
		return e
	}
	if count > 0 {
		return fmt.Errorf("%d legacy tasks or live writers remain; finish control transfer first", count)
	}
	var protocol string
	if e := m.Control.DB.WithContext(ctx).Table("control_settings").Select("value").Where("key='coordination_protocol'").Scan(&protocol).Error; e != nil {
		return e
	}
	if protocol != "handover-v3" && protocol != "v3" {
		return fmt.Errorf("control v3 handover has not started")
	}
	return nil
}

func (m *DirectMigrator) TransferNodes(ctx context.Context) (copied, retained int, err error) {
	if err = m.stopped(ctx); err != nil {
		return
	}
	var online bool
	if err = m.DB.WithContext(ctx).Raw("SELECT to_regclass('ai_artifact_manifests') IS NOT NULL").Scan(&online).Error; err != nil || !online {
		return
	}
	cursor := uuid.Nil
	for {
		var rows []Artifact
		if err = m.DB.WithContext(ctx).Where("artifact_id>? AND kind LIKE 'ai.summary.checkpoint.v1:%'", cursor).Order("artifact_id").Limit(100).Find(&rows).Error; err != nil {
			return
		}
		if len(rows) == 0 {
			return
		}
		for _, a := range rows {
			cursor = a.ArtifactID
			hash, raw, e := asynccontrol.Digest(a.Data)
			node, e2 := decode[summaryCheckpoint](a.Data)
			if e != nil || e2 != nil || hash != a.ArtifactHash || int64(len(raw)) != a.ByteCount || len(raw) > 1<<20 || !a.Complete || a.RowCount != 1 || a.TypedOutputRef != a.ArtifactID || strings.TrimSpace(node.Insight.Summary) == "" {
				err = fmt.Errorf("summary node %s cannot be verified; retained", a.ArtifactID)
				return
			}
			var t asynccontrol.Task
			if err = m.Control.DB.WithContext(ctx).First(&t, "id=?", a.TaskID).Error; err != nil {
				return
			}
			in, e := decode[aiprotocol.Input](t.Input)
			digest, _, e2 := asynccontrol.Digest(t.Input)
			if e != nil || e2 != nil || digest != a.InputDigest || in.SourceID != node.SourceID {
				if t.State == asynccontrol.Done {
					retained++
					continue
				}
				err = fmt.Errorf("summary task %s has no verifiable frozen input; retained", t.ID)
				return
			}
			if in.Intent.ObjectID != a.ObjectID || in.Intent.ContentGeneration != a.ContentGeneration || uuid.NewSHA1(t.ID, []byte(t.InputDigest+":"+a.Kind)) != a.ArtifactID {
				err = asynccontrol.ErrConflict
				return
			}
			row := summaryNode{NodeID: a.ArtifactID, TaskID: a.TaskID, ObjectID: a.ObjectID, SourceID: node.SourceID, InputDigest: a.InputDigest, ProfileDigest: in.Profile.Digest(), Algorithm: "source-map-reduce-v1", InputHash: node.InputHash, DataHash: hash, ContentGeneration: a.ContentGeneration, Level: -1, Position: -1, Data: raw, CreatedAt: a.CreatedAt}
			// Original node ID and opaque position identity are preserved. Old artifacts
			// hashed level/position, so an inverse reconstruction is not invented here.
			if err = m.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
				return
			}
			var saved summaryNode
			if err = m.DB.WithContext(ctx).Where("node_id=?", row.NodeID).Take(&saved).Error; err != nil {
				return
			}
			if saved.DataHash != row.DataHash || saved.InputDigest != row.InputDigest {
				err = asynccontrol.ErrConflict
				return
			}
			copied++
		}
	}
}

func (m *DirectMigrator) ActivateObjects(ctx context.Context, limit int) (count int64, err error) {
	if err = m.stopped(ctx); err != nil {
		return
	}
	q := m.DB.WithContext(ctx).Exec(`UPDATE ai_objects SET writer_protocol=3 WHERE (object_id,content_generation) IN (SELECT object_id,content_generation FROM ai_objects WHERE writer_protocol=2 ORDER BY object_id,content_generation LIMIT ?)`, limit)
	return q.RowsAffected, q.Error
}

func (m *DirectMigrator) Archive(ctx context.Context, sql string) error {
	if e := m.stopped(ctx); e != nil {
		return e
	}
	var exists bool
	if e := m.Control.DB.WithContext(ctx).Raw("SELECT to_regclass('results') IS NOT NULL").Scan(&exists).Error; e != nil {
		return e
	}
	if exists {
		var pending int64
		if e := m.Control.DB.WithContext(ctx).Table("results").Where("committed_at IS NULL").Count(&pending).Error; e != nil {
			return e
		}
		if pending > 0 {
			return fmt.Errorf("%d accepted legacy results still need explicit reconciliation", pending)
		}
	}
	return m.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Generic artifacts are evidence until their original committed receipt or
		// an exact migrated summary node accounts for them. Unaccepted/unknown
		// external writes need explicit reconciliation; age alone is no proof.
		var unresolved int64
		if e := tx.Raw(`SELECT count(*) FROM ai_artifact_manifests a WHERE NOT (
 (a.kind LIKE 'ai.summary.checkpoint.v1:%' AND EXISTS(SELECT 1 FROM ai_summary_nodes n WHERE n.node_id=a.artifact_id AND n.task_id=a.task_id AND n.input_digest=a.input_digest AND n.data_hash=a.artifact_hash))
 OR (a.kind NOT LIKE 'ai.summary.checkpoint.v1:%' AND EXISTS(SELECT 1 FROM ai_commit_receipts r WHERE r.task_id=a.task_id AND r.generation=a.generation AND r.input_digest=a.input_digest AND r.artifact_hash=a.artifact_hash)))`).Scan(&unresolved).Error; e != nil {
			return e
		}
		if unresolved > 0 {
			return fmt.Errorf("%d legacy artifacts lack a committed receipt or verified node; retained", unresolved)
		}
		if e := tx.Exec("SET LOCAL bx.ai_archive_verified='true'").Error; e != nil {
			return e
		}
		return tx.Exec(sql).Error
	})
}
