package aisystem

import (
	"context"
	"fmt"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dblock"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type DirectWorker struct {
	Computer  *Computer
	Publisher *Publisher
}

func (w *DirectWorker) Lookup(ctx context.Context, t asynccontrol.Task) (asynccontrol.Completion, bool, error) {
	return w.Publisher.Lookup(ctx, t)
}

func (w *DirectWorker) Execute(ctx context.Context, t asynccontrol.Task) (asynccontrol.Completion, *time.Time, error) {
	value, err := w.Computer.ComputeValue(ctx, t)
	if err != nil {
		if !IsPermanentComputeError(err) || !contentTask(t.Kind) {
			return asynccontrol.Completion{}, nil, err
		}
		value = &ComputedValue{OutputID: uuid.NewSHA1(t.ID, []byte("output:"+t.InputDigest)), FailureCode: "ai.build.rejected"}
	}
	c, err := w.Publisher.ApplyDirect(ctx, t, value)
	return c, nil, err
}

func receiptCompletion(t asynccontrol.Task, r Receipt) (asynccontrol.Completion, error) {
	if r.ProtocolVersion != 3 || r.TaskID != t.ID || r.InputDigest != t.InputDigest || r.EffectKind != t.Kind || r.ContractVersion != t.ContractVersion {
		return asynccontrol.Completion{}, asynccontrol.ErrConflict
	}
	_, refs, e := asynccontrol.Digest(r.PublishedRefs)
	if e != nil {
		return asynccontrol.Completion{}, e
	}
	c := asynccontrol.Completion{TaskID: t.ID, InputDigest: r.InputDigest, ContractVersion: r.ContractVersion, Outcome: r.Outcome, Refs: refs}
	return c, c.Validate(t)
}

func (p *Publisher) Lookup(ctx context.Context, t asynccontrol.Task) (asynccontrol.Completion, bool, error) {
	var rows []Receipt
	// Check all protocols: an unconverted legacy receipt must never look absent.
	q := p.DB.WithContext(ctx).Where("task_id=?", t.ID).Limit(2).Find(&rows)
	if q.Error != nil {
		return asynccontrol.Completion{}, false, q.Error
	}
	if q.RowsAffected == 0 {
		return asynccontrol.Completion{}, false, nil
	}
	if q.RowsAffected != 1 {
		return asynccontrol.Completion{}, false, asynccontrol.ErrConflict
	}
	c, e := receiptCompletion(t, rows[0])
	return c, e == nil, e
}

func (p *Publisher) ApplyDirect(ctx context.Context, t asynccontrol.Task, v *ComputedValue) (c asynccontrol.Completion, err error) {
	ctx, finishTrace := stageTrace(ctx, "ai.publish")
	defer func() { finishTrace(err) }()
	if t.ProtocolVersion != 3 || t.Phase == "reconcile" || v == nil {
		return c, asynccontrol.ErrInvalid
	}
	in, err := decode[aiprotocol.Input](t.Input)
	if err != nil {
		return c, err
	}
	digest, _, err := asynccontrol.Digest(t.Input)
	if err != nil || digest != t.InputDigest {
		return c, asynccontrol.ErrConflict
	}
	if t.ContractVersion != 1 {
		return c, asynccontrol.ErrInvalid
	}
	// The maintenance proof is fetched before entering AI's local transaction.
	versionsSafe := false
	if t.Kind == "ai.artifact.gc" {
		if p.Control == nil {
			return c, asynccontrol.ErrInvalid
		}
		var pending int64
		err = p.Control.DB.WithContext(ctx).Model(&asynccontrol.Task{}).Where("resource_key=? AND id<>? AND kind<>'ai.artifact.gc' AND (state<>'done' OR protocol_version<>3)", t.ResourceKey, t.ID).Count(&pending).Error
		if err != nil {
			return c, err
		}
		versionsSafe = pending == 0
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err = p.DB.WithContext(bounded).Transaction(func(tx *gorm.DB) error {
		if e := tx.Exec("SET LOCAL lock_timeout='2s'; SET LOCAL statement_timeout='25s'").Error; e != nil {
			return e
		}
		i := in.Intent
		// This lock also serializes object creation, duplicate intent and tombstones.
		if e := dblock.Transaction(tx, fmt.Sprintf("ai-object:%s:%d", i.ObjectID, i.ContentGeneration)); e != nil {
			return e
		}
		if t.Kind == "ai.intent.apply" || t.Kind == "ai.object.tombstone" {
			if e := tx.Exec(`INSERT INTO ai_objects(object_id,content_generation,content_hash,size,media_type,profile_id,profile_revision,writer_protocol) VALUES(?,?,?,?,?,?,?,3) ON CONFLICT DO NOTHING`, i.ObjectID, i.ContentGeneration, i.ContentHash, i.Size, i.MediaType, i.ProfileID, i.ProfileRevision).Error; e != nil {
				return e
			}
		}
		var o Object
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("object_id=? AND content_generation=?", i.ObjectID, i.ContentGeneration).Take(&o).Error; e != nil {
			return e
		}
		if o.WriterProtocol != 3 {
			return asynccontrol.ErrConflict
		}
		var prior []Receipt
		q := tx.Where("task_id=?", t.ID).Limit(2).Find(&prior)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected > 0 {
			if q.RowsAffected != 1 {
				return asynccontrol.ErrConflict
			}
			var e error
			c, e = receiptCompletion(t, prior[0])
			return e
		}
		if o.ContentHash != i.ContentHash || o.Size != i.Size {
			return asynccontrol.ErrConflict
		}
		var now time.Time
		if e := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; e != nil {
			return e
		}
		if !now.Before(t.Deadline) {
			return asynccontrol.ErrLost
		}
		refs := aiprotocol.Published{OutputID: v.OutputID, ObjectID: i.ObjectID, ContentGeneration: i.ContentGeneration, RowCount: v.RowCount, Coverage: v.Coverage}
		outcome := asynccontrol.Succeeded
		if o.TombstonedAt != nil && t.Kind != "ai.object.tombstone" && t.Kind != "ai.artifact.gc" && in.Operation != "delete" {
			outcome = asynccontrol.Superseded
		} else if v.FailureCode != "" {
			if !contentTask(t.Kind) {
				return asynccontrol.ErrInvalid
			}
			outcome = asynccontrol.Failed
			failed := map[string]string{"ai.source.prepare": "source", "ai.text.build": "text", "ai.embedding.generate": "vector", "ai.summary.generate": "summary"}[t.Kind]
			caps := []string{failed}
			if failed == "source" {
				caps = []string{"source", "text", "vector", "summary"}
			} else if failed == "text" {
				caps = []string{"text", "vector"}
			}
			for _, cap := range caps {
				if e := capability(tx, i, cap, seq(in, cap), "failed", v.FailureCode, t.ID); e != nil {
					return e
				}
			}
		} else {
			switch t.Kind {
			case "ai.intent.apply":
				if e := tx.Model(&Object{}).Where("object_id=? AND content_generation=?", i.ObjectID, i.ContentGeneration).Updates(map[string]any{"desired_source_seq": gorm.Expr("GREATEST(desired_source_seq,?)", i.Seqs.Source), "desired_text_seq": gorm.Expr("GREATEST(desired_text_seq,?)", i.Seqs.Text), "desired_vector_seq": gorm.Expr("GREATEST(desired_vector_seq,?)", i.Seqs.Vector), "desired_summary_seq": gorm.Expr("GREATEST(desired_summary_seq,?)", i.Seqs.Summary), "updated_at": now}).Error; e != nil {
					return e
				}
				if o.ActiveSourceID != nil {
					refs.SourceID = *o.ActiveSourceID
				}
				if o.ActiveContentVersionID != nil {
					refs.ContentVersionID = *o.ActiveContentVersionID
				}
				if o.ActiveEmbeddingSetID != nil {
					refs.EmbeddingSetID = *o.ActiveEmbeddingSetID
				}
				for cap, seq := range map[string]int64{"source": i.Seqs.Source, "text": i.Seqs.Text, "vector": i.Seqs.Vector, "summary": i.Seqs.Summary} {
					if e := capability(tx, i, cap, seq, "pending", "", t.ID); e != nil {
						return e
					}
				}
			case "ai.source.prepare", "ai.text.build", "ai.embedding.generate", "ai.summary.generate":
				cap := map[string]string{"ai.source.prepare": "source", "ai.text.build": "text", "ai.embedding.generate": "vector", "ai.summary.generate": "summary"}[t.Kind]
				desired := map[string]int64{"source": o.DesiredSourceSeq, "text": o.DesiredTextSeq, "vector": o.DesiredVectorSeq, "summary": o.DesiredSummarySeq}[cap]
				compatible := true
				if cap == "text" || cap == "summary" {
					compatible = o.ActiveSourceID != nil && *o.ActiveSourceID == in.SourceID
				}
				if cap == "vector" {
					compatible = o.ActiveContentVersionID != nil && *o.ActiveContentVersionID == in.ContentVersionID
				}
				if seq(in, cap) != desired || !compatible {
					outcome = asynccontrol.Superseded
					break
				}
				if e := publishTypedContent(tx, t.Kind, in, v.OutputID, v.Value, v.RowCount, typedValueHash(v.Value)); e != nil {
					return e
				}
				if e := switchDirectPointer(tx, o, in, cap, v.OutputID); e != nil {
					return e
				}
				if e := capability(tx, i, cap, seq(in, cap), "succeeded", "", t.ID); e != nil {
					return e
				}
			case "ai.object.tombstone":
				for _, table := range []string{"ai_sources", "ai_content_versions", "ai_embedding_sets", "ai_summaries"} {
					if e := tx.Table(table).Where("object_id=? AND content_generation=?", i.ObjectID, i.ContentGeneration).Update("retired_at", gorm.Expr("COALESCE(retired_at,clock_timestamp())")).Error; e != nil {
						return e
					}
				}
				if e := tx.Model(&Object{}).Where("object_id=? AND content_generation=?", i.ObjectID, i.ContentGeneration).Updates(map[string]any{"tombstoned_at": gorm.Expr("COALESCE(tombstoned_at,clock_timestamp())"), "lifecycle_revision": gorm.Expr("GREATEST(lifecycle_revision,?)", i.LifecycleRevision), "active_source_id": nil, "active_content_version_id": nil, "active_embedding_set_id": nil, "active_summary_id": nil}).Error; e != nil {
					return e
				}
			case "ai.capability.finish":
				// New automatic stops do not fabricate a domain failure. Explicit domain
				// finalization is rejected until its original-task identity is supplied.
				return asynccontrol.ErrInvalid
			case "ai.project.text", "ai.project.vector":
				if in.Operation != "delete" && in.Operation != "retire" && (o.ActiveContentVersionID == nil || *o.ActiveContentVersionID != in.ContentVersionID || t.Kind == "ai.project.vector" && (o.ActiveEmbeddingSetID == nil || *o.ActiveEmbeddingSetID != in.EmbeddingSetID)) {
					outcome = asynccontrol.Superseded
					break
				}
				if e := publishTypedProjection(tx, in, v.Value); e != nil {
					return e
				}
				if in.Operation != "delete" && in.Operation != "retire" {
					d, ok := v.Value.(projectionData)
					if !ok {
						return asynccontrol.ErrInvalid
					}
					version := d.ContentID
					if d.Backend == "qd" {
						version = d.SetID
					}
					if e := tx.Table("ai_orphan_projections").Where("backend=? AND target_id=? AND artifact_version_id=?", d.Backend, d.TargetID, version).Delete(nil).Error; e != nil {
						return e
					}
				}
			case "ai.artifact.gc":
				if versionsSafe {
					if e := tx.Exec(`DELETE FROM ai_summary_nodes WHERE node_id IN (
 SELECT node_id FROM ai_summary_nodes WHERE object_id=? AND content_generation=? AND created_at<clock_timestamp()-interval '8 days' ORDER BY created_at,node_id LIMIT 1000)`, i.ObjectID, i.ContentGeneration).Error; e != nil {
						return e
					}
				}
				if _, e := p.cleanArtifacts(tx, i, nil, versionsSafe); e != nil {
					return e
				}
			default:
				return asynccontrol.ErrInvalid
			}
		}
		raw := jsonValue(refs)
		r := Receipt{ProtocolVersion: 3, ObjectID: &i.ObjectID, ContentGeneration: i.ContentGeneration, ContractVersion: t.ContractVersion, ResultID: uuid.NewSHA1(t.ID, []byte("domain-receipt-v3:"+t.InputDigest)), TaskID: t.ID, Generation: t.Generation, InputDigest: t.InputDigest, ResultDigest: aiprotocol.Hash(raw), EffectKind: t.Kind, Outcome: outcome, PublishedRefs: raw, Revision: 1, CommittedAt: now}
		if e := tx.Create(&r).Error; e != nil {
			return e
		}
		c, err = receiptCompletion(t, r)
		return err
	})
	return
}

// Only immutable content metadata is hashed; full vectors/text are not encoded
// into a generic JSON payload before inserting their normalized rows.
func typedValueHash(value any) string {
	switch d := value.(type) {
	case textData:
		return aiprotocol.HashJSON(d.Chunks.Children)
	case sourceData:
		return aiprotocol.HashJSON(d.Parsed.Sections)
	}
	return ""
}

func switchDirectPointer(tx *gorm.DB, o Object, in aiprotocol.Input, cap string, id uuid.UUID) error {
	pointer := map[string]string{"source": "active_source_id", "text": "active_content_version_id", "vector": "active_embedding_set_id", "summary": "active_summary_id"}[cap]
	table := map[string]string{"source": "ai_sources", "text": "ai_content_versions", "vector": "ai_embedding_sets", "summary": "ai_summaries"}[cap]
	previous := map[string]*uuid.UUID{"source": o.ActiveSourceID, "text": o.ActiveContentVersionID, "vector": o.ActiveEmbeddingSetID, "summary": o.ActiveSummaryID}[cap]
	if previous != nil && *previous != id {
		if e := tx.Table(table).Where("id=?", *previous).Update("retired_at", gorm.Expr("COALESCE(retired_at,clock_timestamp())")).Error; e != nil {
			return e
		}
	}
	updates := map[string]any{pointer: id, "updated_at": gorm.Expr("clock_timestamp()")}
	if cap == "text" {
		updates["active_embedding_set_id"] = nil
		if o.ActiveEmbeddingSetID != nil {
			if e := tx.Table("ai_embedding_sets").Where("id=?", *o.ActiveEmbeddingSetID).Update("retired_at", gorm.Expr("COALESCE(retired_at,clock_timestamp())")).Error; e != nil {
				return e
			}
		}
	}
	return tx.Model(&Object{}).Where("object_id=? AND content_generation=?", in.Intent.ObjectID, in.Intent.ContentGeneration).Updates(updates).Error
}

func contentTask(kind string) bool {
	return kind == "ai.source.prepare" || kind == "ai.text.build" || kind == "ai.embedding.generate" || kind == "ai.summary.generate"
}
