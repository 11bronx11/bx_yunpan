package aisystem

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"gorm.io/gorm"
)

// Cross-store inspection happens before the local publication transaction.
// Accepted results are immutable and their generations cannot be reopened.
func (p *Publisher) collectableArtifacts(ctx context.Context, i aiprotocol.Intent) ([]uuid.UUID, bool, error) {
	var rows []Artifact
	if err := p.DB.WithContext(ctx).Select("artifact_id,task_id,generation").Where("object_id=? AND content_generation=? AND created_at<?", i.ObjectID, i.ContentGeneration, time.Now().Add(-8*24*time.Hour)).Order("created_at").Limit(100).Find(&rows).Error; err != nil {
		return nil, false, err
	}
	if p.Control == nil {
		return nil, false, errors.New("control proof required for artifact GC")
	}
	out := []uuid.UUID{}
	for _, a := range rows {
		var task struct {
			ResourceKey, State string
			Generation         int64
			Pending            bool
		}
		res := p.Control.DB.WithContext(ctx).Raw(`SELECT t.resource_key,t.state,t.generation,EXISTS(SELECT 1 FROM results r WHERE r.task_id=t.id AND r.generation=? AND r.committed_at IS NULL) AS pending FROM tasks t WHERE t.id=?`, a.Generation, a.TaskID).Scan(&task)
		if res.Error != nil {
			return nil, false, res.Error
		}
		if res.RowsAffected != 1 || task.ResourceKey != fmt.Sprintf("object:%s:%d", i.ObjectID, i.ContentGeneration) || task.Pending {
			continue
		}
		if task.Generation > a.Generation || task.State == "succeeded" || task.State == "superseded" || task.State == "rejected" || task.State == "failed" {
			out = append(out, a.ArtifactID)
		}
	}
	var pending int64
	// Finish cross-store inspection before opening the local transaction. The
	// retirement grace exceeds the maximum processing authorization lifetime;
	// accepted results are retained until their receipt has been acknowledged.
	if err := p.Control.DB.WithContext(ctx).Raw(`SELECT count(*) FROM tasks t WHERE t.resource_key=? AND t.kind<>'ai.artifact.gc' AND (t.state NOT IN ('succeeded','superseded','rejected','failed','quarantined','needs_attention') OR EXISTS(SELECT 1 FROM results r WHERE r.task_id=t.id AND r.committed_at IS NULL))`, fmt.Sprintf("object:%s:%d", i.ObjectID, i.ContentGeneration)).Scan(&pending).Error; err != nil {
		return nil, false, err
	}
	return out, pending == 0, nil
}
func (p *Publisher) cleanArtifacts(tx *gorm.DB, i aiprotocol.Intent, ids []uuid.UUID, versionsSafe bool) (bool, error) {
	if len(ids) > 0 {
		keep := []uuid.UUID{}
		var artifacts []Artifact
		if err := tx.Where("artifact_id IN ? AND kind IN ('ai.project.text','ai.project.vector')", ids).Find(&artifacts).Error; err != nil {
			return false, err
		}
		if versionsSafe {
			for _, artifact := range artifacts {
				d, err := decode[projectionData](artifact.Data)
				if err != nil || d.Operation != "upsert" || (d.Backend != "es" && d.Backend != "qd") {
					continue
				}
				table, column, version := "ai_content_versions", "active_content_version_id", d.ContentID
				if d.Backend == "qd" {
					table, column, version = "ai_embedding_sets", "active_embedding_set_id", d.SetID
				}
				// Eight days exceeds every processing grant. A late unaccepted
				// physical write cannot become a new active version after this proof.
				query := `INSERT INTO ai_orphan_projections(backend,target_id,target_version,object_id,content_generation,artifact_version_id) SELECT ?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM ` + table + ` v JOIN ai_objects o USING(object_id,content_generation) WHERE v.id=? AND o.object_id=? AND o.content_generation=? AND v.retired_at<clock_timestamp()-interval '8 days' AND v.id IS DISTINCT FROM o.` + column + `) AND NOT EXISTS(SELECT 1 FROM ai_projection_states WHERE target_id=? AND artifact_version_id=?) ON CONFLICT DO NOTHING`
				if err := tx.Exec(query, d.Backend, d.TargetID, d.TargetVersion, i.ObjectID, i.ContentGeneration, version, version, i.ObjectID, i.ContentGeneration, d.TargetID, version).Error; err != nil {
					return false, err
				}
				var recorded int64
				if err := tx.Raw(`SELECT (SELECT count(*) FROM ai_projection_states WHERE target_id=? AND artifact_version_id=?)+(SELECT count(*) FROM ai_orphan_projections WHERE target_id=? AND artifact_version_id=?)`, d.TargetID, version, d.TargetID, version).Scan(&recorded).Error; err != nil {
					return false, err
				}
				if recorded == 0 {
					keep = append(keep, artifact.ArtifactID)
				}
			}
		} else {
			for _, artifact := range artifacts {
				keep = append(keep, artifact.ArtifactID)
			}
		}
		deletable := tx.Where("artifact_id IN ?", ids)
		if len(keep) > 0 {
			deletable = deletable.Where("artifact_id NOT IN ?", keep)
		}
		if err := deletable.Delete(&Artifact{}).Error; err != nil {
			return false, err
		}
	}
	if !versionsSafe {
		return false, nil
	}
	// Old versions are removed in dependency order. Active source/content/set/
	// summary pointers, same-generation tombstones and compact receipts remain.
	queries := []string{
		`DELETE FROM ai_chunk_vectors WHERE embedding_set_id IN (SELECT s.id FROM ai_embedding_sets s JOIN ai_objects o USING(object_id,content_generation) WHERE o.object_id=? AND o.content_generation=? AND s.retired_at<clock_timestamp()-interval '8 days' AND s.id IS DISTINCT FROM o.active_embedding_set_id AND NOT EXISTS(SELECT 1 FROM ai_projection_states p WHERE p.artifact_version_id=s.id AND p.operation='upsert' AND p.state<>'deleted'))`,
		`DELETE FROM ai_embedding_sets s USING ai_objects o WHERE s.object_id=o.object_id AND s.content_generation=o.content_generation AND o.object_id=? AND o.content_generation=? AND s.retired_at<clock_timestamp()-interval '8 days' AND s.id IS DISTINCT FROM o.active_embedding_set_id AND NOT EXISTS(SELECT 1 FROM ai_projection_states p WHERE p.artifact_version_id=s.id AND p.operation='upsert' AND p.state<>'deleted') AND NOT EXISTS(SELECT 1 FROM ai_chunk_vectors v WHERE v.embedding_set_id=s.id)`,
		`DELETE FROM ai_child_chunks WHERE content_version_id IN (SELECT c.id FROM ai_content_versions c JOIN ai_objects o USING(object_id,content_generation) WHERE o.object_id=? AND o.content_generation=? AND c.retired_at<clock_timestamp()-interval '8 days' AND c.id IS DISTINCT FROM o.active_content_version_id AND NOT EXISTS(SELECT 1 FROM ai_projection_states p WHERE p.artifact_version_id=c.id AND p.operation='upsert' AND p.state<>'deleted') AND NOT EXISTS(SELECT 1 FROM ai_embedding_sets s WHERE s.content_version_id=c.id))`,
		`DELETE FROM ai_parent_chunks WHERE content_version_id IN (SELECT c.id FROM ai_content_versions c JOIN ai_objects o USING(object_id,content_generation) WHERE o.object_id=? AND o.content_generation=? AND c.retired_at<clock_timestamp()-interval '8 days' AND c.id IS DISTINCT FROM o.active_content_version_id AND NOT EXISTS(SELECT 1 FROM ai_projection_states p WHERE p.artifact_version_id=c.id AND p.operation='upsert' AND p.state<>'deleted') AND NOT EXISTS(SELECT 1 FROM ai_child_chunks s WHERE s.content_version_id=c.id))`,
		`DELETE FROM ai_content_versions c USING ai_objects o WHERE c.object_id=o.object_id AND c.content_generation=o.content_generation AND o.object_id=? AND o.content_generation=? AND c.retired_at<clock_timestamp()-interval '8 days' AND c.id IS DISTINCT FROM o.active_content_version_id AND NOT EXISTS(SELECT 1 FROM ai_projection_states p WHERE p.artifact_version_id=c.id AND p.operation='upsert' AND p.state<>'deleted') AND NOT EXISTS(SELECT 1 FROM ai_parent_chunks p WHERE p.content_version_id=c.id) AND NOT EXISTS(SELECT 1 FROM ai_embedding_sets v WHERE v.content_version_id=c.id)`,
		`DELETE FROM ai_summaries s USING ai_objects o WHERE s.object_id=o.object_id AND s.content_generation=o.content_generation AND o.object_id=? AND o.content_generation=? AND s.retired_at<clock_timestamp()-interval '8 days' AND s.id IS DISTINCT FROM o.active_summary_id`,
		`DELETE FROM ai_source_blocks WHERE source_id IN (SELECT s.id FROM ai_sources s JOIN ai_objects o USING(object_id,content_generation) WHERE o.object_id=? AND o.content_generation=? AND s.retired_at<clock_timestamp()-interval '8 days' AND s.id IS DISTINCT FROM o.active_source_id AND NOT EXISTS(SELECT 1 FROM ai_content_versions c WHERE c.source_id=s.id) AND NOT EXISTS(SELECT 1 FROM ai_summaries m WHERE m.source_id=s.id))`,
		`DELETE FROM ai_sources s USING ai_objects o WHERE s.object_id=o.object_id AND s.content_generation=o.content_generation AND o.object_id=? AND o.content_generation=? AND s.retired_at<clock_timestamp()-interval '8 days' AND s.id IS DISTINCT FROM o.active_source_id AND NOT EXISTS(SELECT 1 FROM ai_source_blocks b WHERE b.source_id=s.id) AND NOT EXISTS(SELECT 1 FROM ai_content_versions c WHERE c.source_id=s.id) AND NOT EXISTS(SELECT 1 FROM ai_summaries m WHERE m.source_id=s.id)`,
	}
	for _, query := range queries {
		if err := tx.Exec(query, i.ObjectID, i.ContentGeneration).Error; err != nil {
			return false, err
		}
	}
	var remains int64
	if err := tx.Raw(`SELECT (SELECT count(*) FROM ai_sources WHERE object_id=? AND content_generation=?)+(SELECT count(*) FROM ai_projection_states WHERE object_id=? AND content_generation=? AND operation='upsert' AND state<>'deleted')`, i.ObjectID, i.ContentGeneration, i.ObjectID, i.ContentGeneration).Scan(&remains).Error; err != nil {
		return false, err
	}
	return remains == 0, nil
}

type cleanupTarget struct {
	ID       uuid.UUID
	Name     string
	Versions []string
}
type cleanupData struct {
	Backend string
	Targets []cleanupTarget
}

func (p *Projector) projectDelete(ctx context.Context, backend string, in aiprotocol.Input) (any, int64, error) {
	var rows []struct {
		TargetID          uuid.UUID
		TargetVersion     string
		ArtifactVersionID uuid.UUID
	}
	q := p.DB.WithContext(ctx).Table("ai_published_projections s").Select("s.target_id,t.target_version,s.artifact_version_id").Joins("JOIN ai_projection_catalog t ON t.id=s.target_id").Where("s.object_id=? AND s.content_generation=? AND s.backend=? AND s.operation='upsert'", in.Intent.ObjectID, in.Intent.ContentGeneration, backend)
	if in.Operation == "retire" {
		table, column := "ai_published_content", "active_content_version_id"
		if backend == "qd" {
			table, column = "ai_published_sets", "active_embedding_set_id"
		}
		q = q.Joins("JOIN " + table + " v ON v.id=s.artifact_version_id").Joins("JOIN ai_published_objects o ON o.object_id=s.object_id AND o.content_generation=s.content_generation").Where("v.retired_at<clock_timestamp()-interval '8 days' AND v.id IS DISTINCT FROM o." + column)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	var orphans []struct {
		TargetID          uuid.UUID
		TargetVersion     string
		ArtifactVersionID uuid.UUID
	}
	if err := p.DB.WithContext(ctx).Table("ai_published_orphan_projections").Select("target_id,target_version,artifact_version_id").Where("object_id=? AND content_generation=? AND backend=?", in.Intent.ObjectID, in.Intent.ContentGeneration, backend).Find(&orphans).Error; err != nil {
		return nil, 0, err
	}
	rows = append(rows, orphans...)
	d := cleanupData{Backend: backend}
	indices := map[uuid.UUID]int{}
	for _, r := range rows {
		n, ok := indices[r.TargetID]
		if !ok {
			n = len(d.Targets)
			indices[r.TargetID] = n
			d.Targets = append(d.Targets, cleanupTarget{ID: r.TargetID, Name: r.TargetVersion})
		}
		d.Targets[n].Versions = append(d.Targets[n].Versions, r.ArtifactVersionID.String())
	}
	for _, target := range d.Targets {
		if backend == "es" {
			body := map[string]any{"query": map[string]any{"terms": map[string]any{"content_id": target.Versions}}}
			if err := p.request(ctx, "POST", strings.TrimRight(p.ES, "/")+"/"+target.Name+"/_delete_by_query?conflicts=proceed&refresh=true", body, backend, nil); err != nil {
				return nil, 0, err
			}
		} else {
			body := map[string]any{"filter": map[string]any{"must": []any{map[string]any{"key": "set_id", "match": map[string]any{"any": target.Versions}}}}}
			if err := p.request(ctx, "POST", strings.TrimRight(p.QD, "/")+"/collections/"+target.Name+"/points/delete?wait=true", body, backend, nil); err != nil {
				return nil, 0, err
			}
		}
	}
	return d, int64(len(rows)), nil
}
func publishDelete(tx *gorm.DB, in aiprotocol.Input, a Artifact) error {
	d, err := decode[cleanupData](a.Data)
	if err != nil {
		return err
	}
	if d.Backend != "es" && d.Backend != "qd" {
		return errors.New("invalid cleanup backend")
	}
	for _, target := range d.Targets {
		if err := tx.Exec(`INSERT INTO ai_projection_states(backend,target_id,object_id,content_generation,content_version_id,embedding_set_id,artifact_version_id,operation,source_revision,artifact_hash,expected_count,applied_count,state,published_at) SELECT backend,target_id,object_id,content_generation,content_version_id,embedding_set_id,artifact_version_id,'delete',source_revision,artifact_hash,0,0,'complete',clock_timestamp() FROM ai_projection_states WHERE target_id=? AND object_id=? AND content_generation=? AND artifact_version_id::text=ANY(?::text[]) AND operation='upsert' ON CONFLICT DO NOTHING`, target.ID, in.Intent.ObjectID, in.Intent.ContentGeneration, pq.Array(target.Versions)).Error; err != nil {
			return err
		}
		if err := tx.Exec("UPDATE ai_projection_states SET state='deleted' WHERE target_id=? AND object_id=? AND content_generation=? AND artifact_version_id::text=ANY(?::text[]) AND operation='upsert'", target.ID, in.Intent.ObjectID, in.Intent.ContentGeneration, pq.Array(target.Versions)).Error; err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM ai_orphan_projections WHERE backend=? AND target_id=? AND object_id=? AND content_generation=? AND artifact_version_id::text=ANY(?::text[])", d.Backend, target.ID, in.Intent.ObjectID, in.Intent.ContentGeneration, pq.Array(target.Versions)).Error; err != nil {
			return err
		}
	}
	return nil
}
