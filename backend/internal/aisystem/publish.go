package aisystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"strings"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Publisher struct {
	DB      *gorm.DB
	Control *asynccontrol.Store
}

func (p *Publisher) Commit(ctx context.Context, n asynccontrol.Notice) error {
	t, r, err := p.Control.LoadResult(ctx, n)
	if errors.Is(err, asynccontrol.ErrObsolete) {
		return nil
	}
	if err != nil {
		return err
	}
	if t.EffectTarget != asynccontrol.EffectAI || r.EffectTarget != asynccontrol.EffectAI {
		return asynccontrol.ErrConflict
	}
	outcome, err := p.Publish(ctx, t, r)
	if err != nil {
		return err
	}
	return p.Control.Report(ctx, t, r, outcome)
}
func (p *Publisher) Publish(ctx context.Context, t asynccontrol.Task, r asynccontrol.Result) (string, error) {
	outcome := ""
	in, err := decode[aiprotocol.Input](t.Input)
	if err != nil {
		return outcome, err
	}
	out, err := decode[aiprotocol.Output](r.Body)
	if err != nil || out.Validate() != nil || out.TaskID != t.ID || out.Generation != r.Generation || out.InputDigest != t.InputDigest || out.ObjectID != in.Intent.ObjectID || out.ContentGeneration != in.Intent.ContentGeneration {
		return outcome, asynccontrol.ErrConflict
	}
	var gcIDs []uuid.UUID
	var gcVersionsSafe bool
	var gcCursor artifactScanCursor
	if t.Kind == "ai.artifact.gc" {
		gcIDs, gcVersionsSafe, gcCursor, err = p.collectableArtifactBatch(ctx, in.Intent)
		if err != nil {
			return outcome, err
		}
	}
	err = p.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var prior Receipt
		q := tx.Where("result_id=?", r.ID).Find(&prior)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected > 0 {
			if prior.ResultDigest != r.ResultDigest || prior.InputDigest != r.InputDigest || prior.ArtifactHash != out.ArtifactHash {
				return asynccontrol.ErrConflict
			}
			outcome = prior.Outcome
			slog.InfoContext(ctx, "AI publish receipt reused", "task_id", t.ID, "result_id", r.ID, "request_id", in.Intent.RequestID, "object_id", out.ObjectID, "content_generation", out.ContentGeneration, "version_id", out.OutputID)
			return nil
		}
		var artifact Artifact
		if err := tx.Where("artifact_id=?", out.ArtifactID).Take(&artifact).Error; err != nil {
			return err
		}
		hash, canonical, err := asynccontrol.Digest(artifact.Data)
		if err != nil || hash != artifact.ArtifactHash || hash != out.ArtifactHash || artifact.TaskID != t.ID || artifact.Generation != r.Generation || artifact.InputDigest != t.InputDigest || artifact.ObjectID != in.Intent.ObjectID || artifact.ContentGeneration != in.Intent.ContentGeneration || artifact.Kind != t.Kind || !artifact.Complete || artifact.TypedOutputRef != out.OutputID || artifact.RowCount != out.RowCount || artifact.ByteCount != out.ByteCount || int64(len(canonical)) != artifact.ByteCount {
			return asynccontrol.ErrConflict
		}
		i := in.Intent
		if t.Kind == "ai.intent.apply" || t.Kind == "ai.object.tombstone" {
			if err := tx.Exec(`INSERT INTO ai_objects(object_id,content_generation,content_hash,size,media_type,profile_id,profile_revision) VALUES(?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, i.ObjectID, i.ContentGeneration, i.ContentHash, i.Size, i.MediaType, i.ProfileID, i.ProfileRevision).Error; err != nil {
				return err
			}
		}
		var o Object
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("object_id=? AND content_generation=?", i.ObjectID, i.ContentGeneration).Take(&o).Error; err != nil {
			return err
		}
		// Re-check a concurrent duplicate after taking the object serialization lock.
		q = tx.Where("result_id=?", r.ID).Find(&prior)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected > 0 {
			if prior.ResultDigest != r.ResultDigest {
				return asynccontrol.ErrConflict
			}
			outcome = prior.Outcome
			return nil
		}
		outcome = asynccontrol.Succeeded
		if o.ContentHash != i.ContentHash || o.Size != i.Size {
			return asynccontrol.ErrConflict
		}
		if o.TombstonedAt != nil && t.Kind != "ai.object.tombstone" && t.Kind != "ai.artifact.gc" && in.Operation != "delete" {
			outcome = asynccontrol.Superseded
		} else {
			switch t.Kind {
			case "ai.intent.apply":
				if err := tx.Model(&Object{}).Where("object_id=? AND content_generation=? AND tombstoned_at IS NULL", i.ObjectID, i.ContentGeneration).Updates(map[string]any{"desired_source_seq": gorm.Expr("GREATEST(desired_source_seq,?)", i.Seqs.Source), "desired_text_seq": gorm.Expr("GREATEST(desired_text_seq,?)", i.Seqs.Text), "desired_vector_seq": gorm.Expr("GREATEST(desired_vector_seq,?)", i.Seqs.Vector), "desired_summary_seq": gorm.Expr("GREATEST(desired_summary_seq,?)", i.Seqs.Summary), "profile_id": i.ProfileID, "profile_revision": i.ProfileRevision, "updated_at": gorm.Expr("clock_timestamp()")}).Error; err != nil {
					return err
				}
				for cap, s := range map[string]int64{"source": i.Seqs.Source, "text": i.Seqs.Text, "vector": i.Seqs.Vector, "summary": i.Seqs.Summary} {
					if err := capability(tx, i, cap, s, "pending", "", t.ID); err != nil {
						return err
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
				if err := publishContent(tx, t.Kind, in, out, artifact); err != nil {
					return err
				}
				pointer := map[string]string{"source": "active_source_id", "text": "active_content_version_id", "vector": "active_embedding_set_id", "summary": "active_summary_id"}[cap]
				updates := map[string]any{pointer: out.OutputID, "updated_at": gorm.Expr("clock_timestamp()")}
				if cap == "source" {
					updates["media_type"] = i.MediaType
				}
				if cap == "text" {
					updates["active_embedding_set_id"] = nil
				}
				tables := map[string]string{"source": "ai_sources", "text": "ai_content_versions", "vector": "ai_embedding_sets", "summary": "ai_summaries"}
				previous := map[string]*uuid.UUID{"source": o.ActiveSourceID, "text": o.ActiveContentVersionID, "vector": o.ActiveEmbeddingSetID, "summary": o.ActiveSummaryID}[cap]
				if previous != nil && *previous != out.OutputID {
					if err := tx.Table(tables[cap]).Where("id=?", *previous).Update("retired_at", gorm.Expr("COALESCE(retired_at,clock_timestamp())")).Error; err != nil {
						return err
					}
				}
				if cap == "text" && o.ActiveEmbeddingSetID != nil {
					if err := tx.Table("ai_embedding_sets").Where("id=?", *o.ActiveEmbeddingSetID).Update("retired_at", gorm.Expr("COALESCE(retired_at,clock_timestamp())")).Error; err != nil {
						return err
					}
				}
				if err := tx.Model(&Object{}).Where("object_id=? AND content_generation=?", i.ObjectID, i.ContentGeneration).Updates(updates).Error; err != nil {
					return err
				}
				if err := capability(tx, i, cap, seq(in, cap), "succeeded", "", t.ID); err != nil {
					return err
				}
			case "ai.capability.finish":
				if err := capability(tx, i, in.FailureCapability, in.FailureSeq, "failed", in.ErrorCode, t.ID); err != nil {
					return err
				}
			case "ai.object.tombstone":
				for _, table := range []string{"ai_sources", "ai_content_versions", "ai_embedding_sets", "ai_summaries"} {
					if err := tx.Table(table).Where("object_id=? AND content_generation=?", i.ObjectID, i.ContentGeneration).Update("retired_at", gorm.Expr("COALESCE(retired_at,clock_timestamp())")).Error; err != nil {
						return err
					}
				}
				if err := tx.Model(&Object{}).Where("object_id=? AND content_generation=?", i.ObjectID, i.ContentGeneration).Updates(map[string]any{"tombstoned_at": gorm.Expr("COALESCE(tombstoned_at,clock_timestamp())"), "lifecycle_revision": gorm.Expr("GREATEST(lifecycle_revision,?)", i.LifecycleRevision), "active_source_id": nil, "active_content_version_id": nil, "active_embedding_set_id": nil, "active_summary_id": nil}).Error; err != nil {
					return err
				}
			case "ai.project.text", "ai.project.vector":
				if err := publishProjection(tx, in, artifact); err != nil {
					return err
				}
			case "ai.artifact.gc":
				clean, err := p.cleanArtifacts(tx, in.Intent, gcIDs, gcVersionsSafe)
				if err != nil {
					return err
				}
				if o.TombstonedAt != nil && clean {
					outcome = asynccontrol.Superseded
				}
			default:
				return asynccontrol.ErrInvalid
			}
		}
		refs := map[string]any{"output_id": out.OutputID}
		if t.Kind == "ai.artifact.gc" {
			refs["gc_scan_cursor"] = gcCursor
		}
		receipt := Receipt{ResultID: r.ID, TaskID: t.ID, Generation: r.Generation, InputDigest: t.InputDigest, ResultDigest: r.ResultDigest, ArtifactHash: out.ArtifactHash, EffectKind: t.Kind, Outcome: outcome, PublishedRefs: jsonValue(refs), Revision: 1, CommittedAt: time.Now().UTC()}
		return tx.Create(&receipt).Error
	})
	return outcome, err
}
func jsonValue(v any) datatypes.JSON { b, _ := json.Marshal(v); return b }
func stringInt(v int64) string       { return strconv.FormatInt(v, 10) }
func capability(tx *gorm.DB, i aiprotocol.Intent, cap string, seq int64, state, code string, task uuid.UUID) error {
	return tx.Exec(`INSERT INTO ai_capability_status(object_id,content_generation,capability,desired_seq,latest_observed_seq,latest_build_state,error_code,source_task_id) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(object_id,content_generation,capability) DO UPDATE SET desired_seq=GREATEST(ai_capability_status.desired_seq,EXCLUDED.desired_seq),latest_observed_seq=EXCLUDED.latest_observed_seq,latest_build_state=EXCLUDED.latest_build_state,error_code=EXCLUDED.error_code,source_task_id=EXCLUDED.source_task_id,observed_at=clock_timestamp() WHERE EXCLUDED.latest_observed_seq>=ai_capability_status.latest_observed_seq AND EXCLUDED.latest_observed_seq>=ai_capability_status.desired_seq AND NOT (EXCLUDED.latest_observed_seq=ai_capability_status.latest_observed_seq AND ai_capability_status.latest_build_state='succeeded' AND EXCLUDED.latest_build_state<>'succeeded')`, i.ObjectID, i.ContentGeneration, cap, seq, seq, state, code, task).Error
}
func insert(tx *gorm.DB, table string, row map[string]any) error {
	return tx.Table(table).Create(row).Error
}
func publishContent(tx *gorm.DB, kind string, in aiprotocol.Input, out aiprotocol.Output, a Artifact) error {
	i := in.Intent
	common := map[string]any{"id": out.OutputID, "object_id": i.ObjectID, "content_generation": i.ContentGeneration, "input_digest": in.Profile.Digest()}
	switch kind {
	case "ai.source.prepare":
		d, err := decode[sourceData](a.Data)
		if err != nil {
			return err
		}
		if int64(len(d.Parsed.Sections)) != a.RowCount {
			return asynccontrol.ErrConflict
		}
		common["source_seq"] = i.Seqs.Source
		common["coverage"] = d.Coverage
		common["profile_revision"] = i.ProfileRevision
		common["parser_id"] = d.Parsed.ParserID
		common["parser_version"] = d.Parsed.ParserVersion
		common["parser_profile_version"] = in.Profile.Parser
		common["vision_model_version"] = d.Parsed.VisionModelVersion
		common["language"] = d.Parsed.Language
		common["quality"] = jsonValue(d.Parsed.Quality)
		common["warnings"] = jsonValue(d.Parsed.Warnings)
		common["block_count"] = len(d.Parsed.Sections)
		common["source_hash"] = aiprotocol.HashJSON(d.Parsed.Sections)
		if err := insert(tx, "ai_sources", common); err != nil {
			return err
		}
		for n, b := range d.Parsed.Sections {
			loc := b
			loc.Text = ""
			row := map[string]any{"source_id": out.OutputID, "ordinal": n, "block_key": fmt.Sprintf("%s:%d", out.OutputID, n), "text": b.Text, "kind": b.Kind, "heading_path": b.HeadingPath, "location": jsonValue(loc), "source_kind": b.SourceKind, "encoding": b.Encoding}
			if err := insert(tx, "ai_source_blocks", row); err != nil {
				return err
			}
		}
	case "ai.text.build":
		d, err := decode[textData](a.Data)
		if err != nil {
			return err
		}
		if int64(len(d.Chunks.Parents)+len(d.Chunks.Children)) != a.RowCount || len(d.Chunks.Children) == 0 {
			return asynccontrol.ErrConflict
		}
		delete(common, "input_digest")
		common["source_id"] = in.SourceID
		common["text_seq"] = i.Seqs.Text
		common["chunker_version"] = in.Profile.Chunker
		common["tokenizer_id"] = "mixed-script-estimate-v1"
		common["tokenizer_version"] = "v1"
		common["config_digest"] = aiprotocol.HashJSON(in.Profile.Chunker)
		common["parent_count"] = len(d.Chunks.Parents)
		common["child_count"] = len(d.Chunks.Children)
		common["text_hash"] = a.ArtifactHash
		common["coverage"] = d.Chunks.Coverage
		if err := insert(tx, "ai_content_versions", common); err != nil {
			return err
		}
		parents := map[uuid.UUID]string{}
		for _, p := range d.Chunks.Parents {
			parents[p.ID] = p.Content
			loc := p.Location
			loc.Text = ""
			if p.TokenCount > 2400 {
				return asynccontrol.ErrConflict
			}
			if err := insert(tx, "ai_parent_chunks", map[string]any{"id": p.ID, "content_version_id": out.OutputID, "ordinal": p.Ordinal, "parent_key": p.Key, "content": p.Content, "heading_path": p.Heading, "kind": p.Kind, "block_start": p.BlockStart, "block_end": p.BlockEnd, "location": jsonValue(loc), "token_count": p.TokenCount}); err != nil {
				return err
			}
		}
		for _, c := range d.Chunks.Children {
			pr := []rune(parents[c.ParentID])
			if c.ParentStart < 0 || c.ParentEnd > len(pr) || c.ParentStart >= c.ParentEnd || string(pr[c.ParentStart:c.ParentEnd]) != c.Content || c.TokenCount > 600 {
				return asynccontrol.ErrConflict
			}
			loc := c.Location
			loc.Text = ""
			if err := insert(tx, "ai_child_chunks", map[string]any{"id": c.ID, "content_version_id": out.OutputID, "parent_id": c.ParentID, "ordinal": c.Ordinal, "child_key": c.Key, "content": c.Content, "heading_path": c.Heading, "location": jsonValue(loc), "block_start": c.BlockStart, "block_end": c.BlockEnd, "parent_start": c.ParentStart, "parent_end": c.ParentEnd, "token_count": c.TokenCount}); err != nil {
				return err
			}
		}
	case "ai.embedding.generate":
		d, err := decode[vectorData](a.Data)
		if err != nil {
			return err
		}
		var count int64
		if err := tx.Table("ai_child_chunks").Where("content_version_id=?", in.ContentVersionID).Count(&count).Error; err != nil {
			return err
		}
		if int64(len(d.Items)) != count || count != a.RowCount {
			return asynccontrol.ErrConflict
		}
		common["content_version_id"] = in.ContentVersionID
		common["vector_seq"] = i.Seqs.Vector
		common["provider"] = in.Profile.EmbeddingProvider
		common["model"] = in.Profile.EmbeddingModel
		common["revision"] = in.Profile.EmbeddingRevision
		common["dimension"] = 1024
		common["normalization"] = "l2"
		common["vector_count"] = count
		if err := insert(tx, "ai_embedding_sets", common); err != nil {
			return err
		}
		for _, v := range d.Items {
			var content string
			if err := tx.Table("ai_child_chunks").Select("content").Where("id=? AND content_version_id=?", v.ChildID, in.ContentVersionID).Take(&content).Error; err != nil {
				return err
			}
			if aiprotocol.Hash([]byte(content)) != v.TextHash || len(v.Vector) != 1024 {
				return asynccontrol.ErrConflict
			}
			if err := insert(tx, "ai_chunk_vectors", map[string]any{"embedding_set_id": out.OutputID, "child_id": v.ChildID, "content_version_id": in.ContentVersionID, "embedding": vectorLiteral(v.Vector), "input_text_hash": v.TextHash}); err != nil {
				return err
			}
		}
	case "ai.summary.generate":
		d, err := decode[summaryData](a.Data)
		if err != nil {
			return err
		}
		if d.SourceBlocks < 1 || d.ProcessedBlocks != d.SourceBlocks || int64(d.SourceBlocks) != a.RowCount || strings.TrimSpace(d.Insight.Summary) == "" {
			return asynccontrol.ErrConflict
		}
		common["source_id"] = in.SourceID
		common["summary_seq"] = i.Seqs.Summary
		common["provider"] = in.Profile.SummaryProvider
		common["chat_model"] = in.Profile.SummaryModel
		common["revision"] = in.Profile.SummaryRevision
		common["prompt_version"] = in.Profile.Prompt
		common["strategy_version"] = "source-map-reduce-v1"
		common["summary"] = d.Insight.Summary
		common["tags"] = jsonValue(d.Insight.Tags)
		common["language"] = d.Insight.Language
		common["coverage"] = d.Coverage
		common["source_block_count"] = d.SourceBlocks
		common["processed_block_count"] = d.ProcessedBlocks
		if err := insert(tx, "ai_summaries", common); err != nil {
			return err
		}
	}
	return nil
}

func vectorLiteral(v []float32) string {
	b := make([]string, len(v))
	for i, f := range v {
		b[i] = strconv.FormatFloat(float64(f), 'g', -1, 32)
	}
	return "[" + strings.Join(b, ",") + "]"
}
