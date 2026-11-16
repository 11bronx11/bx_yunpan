package aisystem

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/core/pb"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Computer struct {
	DB                 *gorm.DB
	Core               pb.CoreReadServiceClient
	Parser             ai.ParserGateway
	Embedding, Summary ai.Provider
	Profile            aiprotocol.Profile
	HTTP               *http.Client
	SummaryCalls       int
	Project            func(context.Context, string, aiprotocol.Input) (any, int64, error)
}

func (c *Computer) Compute(ctx context.Context, t asynccontrol.Task) (body json.RawMessage, computeErr error) {
	in, err := decode[aiprotocol.Input](t.Input)
	started := time.Now()
	defer func() {
		slog.InfoContext(ctx, "AI computation completed", "task_id", t.ID, "request_id", in.Intent.RequestID, "job_id", in.Intent.JobID, "object_id", in.Intent.ObjectID, "content_generation", in.Intent.ContentGeneration, "kind", t.Kind, "duration_ms", time.Since(started).Milliseconds(), "failed", computeErr != nil)
	}()
	if err != nil || (t.Kind != "ai.artifact.gc" && in.Operation != "retire" && in.Intent.Validate() != nil) || (in.Profile.Digest() != c.Profile.Digest() && t.Kind != "ai.artifact.gc" && t.Kind != "ai.object.tombstone" && t.Kind != "ai.capability.finish" && in.Operation != "delete" && in.Operation != "retire") {
		return nil, asynccontrol.ErrInvalid
	}
	// Targeted rebuilds wait for their immutable prerequisites. They must not
	// create a second source for the same source sequence while initial parsing runs.
	var current Object
	if t.Kind == "ai.intent.apply" && !slices.Contains(in.Intent.Targets, "source") {
		if e := c.DB.WithContext(ctx).Table("ai_published_objects").Where("object_id=? AND content_generation=?", in.Intent.ObjectID, in.Intent.ContentGeneration).Find(&current).Error; e != nil {
			return nil, e
		}
		if current.ActiveSourceID == nil {
			return nil, errors.New("ai.source.pending")
		}
		if (slices.Contains(in.Intent.Targets, "vectors") || slices.Contains(in.Intent.Targets, "projection_text") || slices.Contains(in.Intent.Targets, "projection_vector")) && current.ActiveContentVersionID == nil {
			return nil, errors.New("ai.text.pending")
		}
		if slices.Contains(in.Intent.Targets, "projection_vector") && current.ActiveEmbeddingSetID == nil {
			return nil, errors.New("ai.vector.pending")
		}
	}
	outputID := uuid.NewSHA1(t.ID, []byte("output:"+t.InputDigest))
	var data any
	var count int64
	coverage := "complete"
	switch t.Kind {
	case "ai.intent.apply":
		data = in.Intent
		count = 1
	case "ai.source.prepare":
		if c.Core == nil || c.Parser == nil {
			return nil, errors.New("ai.source.unavailable")
		}
		obj, err := c.Core.GetObjectForProcessing(ctx, &pb.ProcessingRequest{Grant: in.ProcessingGrant, InputIdentity: aiprotocol.HashJSON(in.Intent), IntentId: in.Intent.RequestID.String(), ObjectId: in.Intent.ObjectID.String(), ContentGeneration: in.Intent.ContentGeneration})
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, obj.ReadUrl, nil)
		if err != nil {
			return nil, errors.New("ai.source.unavailable")
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, errors.New("ai.source.unavailable")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, errors.New("ai.source.unavailable")
		}
		// The original object size and hash are immutable inputs authorized by core.
		if obj.Size > 64<<20 {
			return nil, asynccontrol.ErrInvalid
		}
		bytes, err := io.ReadAll(io.LimitReader(resp.Body, obj.Size+1))
		if err != nil || int64(len(bytes)) != obj.Size || aiprotocol.Hash(bytes) != obj.Sha256 || obj.Sha256 != in.Intent.ContentHash {
			return nil, errors.New("ai.source.hash_mismatch")
		}
		parsed, err := c.Parser.Parse(ctx, ai.ParseRequest{RunID: t.ID, ObjectID: in.Intent.ObjectID, DocumentVersionID: outputID, ContentHash: in.Intent.ContentHash, MediaType: in.Intent.MediaType, ObjectAddress: ai.ObjectReadAddress{Data: bytes, MediaType: in.Intent.MediaType}, ParserProfileVersion: in.Profile.Parser, SizeLimitBytes: 64 << 20, PageLimit: 10000, PixelLimit: 100000000, MaxChunks: 100000, ChunkerVersion: rag.ParentChildVersion})
		if err != nil {
			return nil, err
		}
		parsed.Chunks = nil // source publication never depends on parser-side chunks.
		if len(parsed.Sections) == 0 {
			return nil, errors.New("ai.source.empty")
		}
		count = int64(len(parsed.Sections))
		coverage = "complete"
		if parsed.ChunksTruncated {
			coverage = "partial"
		}
		data = sourceData{Parsed: parsed, Coverage: coverage}
	case "ai.text.build":
		blocks, err := readBlocks(ctx, c.DB, in.SourceID)
		if err != nil {
			return nil, err
		}
		chunks, err := rag.BuildParentChild(outputID, blocks)
		if err != nil {
			return nil, err
		}
		if len(chunks.Children) == 0 {
			return nil, errors.New("ai.text.empty")
		}
		count = int64(len(chunks.Parents) + len(chunks.Children))
		var sourceCoverage string
		if err := c.DB.WithContext(ctx).Table("ai_published_sources").Select("coverage").Where("id=?", in.SourceID).Scan(&sourceCoverage).Error; err != nil {
			return nil, err
		}
		if sourceCoverage != "complete" {
			chunks.Coverage = sourceCoverage
		}
		coverage = chunks.Coverage
		data = textData{chunks}
	case "ai.embedding.generate":
		if c.Embedding == nil {
			return nil, errors.New("embedding.unavailable")
		}
		var children []struct {
			ID      uuid.UUID
			Content string
		}
		if err := c.DB.WithContext(ctx).Table("ai_published_children").Where("content_version_id=?", in.ContentVersionID).Order("ordinal").Find(&children).Error; err != nil {
			return nil, err
		}
		if len(children) == 0 {
			return nil, errors.New("ai.vector.input_missing")
		}
		vectors := vectorData{}
		for offset := 0; offset < len(children); offset += 32 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			end := min(offset+32, len(children))
			texts := []string{}
			for _, ch := range children[offset:end] {
				texts = append(texts, ch.Content)
			}
			vs, err := c.Embedding.Embeddings(ctx, texts)
			if err != nil {
				return nil, errors.New("embedding.unavailable")
			}
			if len(vs) != len(texts) {
				return nil, asynccontrol.ErrInvalid
			}
			for n, v := range vs {
				if len(v) != 1024 {
					return nil, asynccontrol.ErrInvalid
				}
				vectors.Items = append(vectors.Items, vectorItem{children[offset+n].ID, v, aiprotocol.Hash([]byte(texts[n]))})
			}
		}
		count = int64(len(vectors.Items))
		data = vectors
	case "ai.summary.generate":
		if c.Summary == nil {
			return nil, errors.New("summary.unavailable")
		}
		blocks, err := readBlocks(ctx, c.DB, in.SourceID)
		if err != nil {
			return nil, err
		}
		parts := rag.SummaryParts(blocks, 6000)
		if len(parts) == 0 {
			return nil, errors.New("ai.summary.empty")
		}
		if len(parts)+1 > max(c.SummaryCalls, 2) {
			return nil, errors.New("ai.summary.call_budget_exceeded")
		}
		summaries := []string{}
		var insight ai.Insight
		for _, part := range parts {
			insight, err = c.Summary.Enrich(ctx, part)
			if err != nil {
				return nil, errors.New("summary.unavailable")
			}
			if strings.TrimSpace(insight.Summary) == "" {
				return nil, errors.New("ai.summary.empty")
			}
			summaries = append(summaries, insight.Summary)
		}
		if len(summaries) > 1 {
			combined := strings.Join(summaries, "\n")
			if rag.EstimateTokens(combined) > 6000 {
				return nil, errors.New("ai.summary.reduce_budget_exceeded")
			}
			insight, err = c.Summary.Enrich(ctx, combined)
			if err != nil {
				return nil, errors.New("summary.unavailable")
			}
			if strings.TrimSpace(insight.Summary) == "" {
				return nil, errors.New("ai.summary.empty")
			}
		}
		var sourceCoverage string
		if err := c.DB.WithContext(ctx).Table("ai_published_sources").Select("coverage").Where("id=?", in.SourceID).Scan(&sourceCoverage).Error; err != nil {
			return nil, err
		}
		count = int64(len(blocks))
		coverage = sourceCoverage
		data = summaryData{Insight: insight, Coverage: sourceCoverage, SourceBlocks: len(blocks), ProcessedBlocks: len(blocks), Parts: summaries}
	case "ai.object.tombstone", "ai.capability.finish", "ai.artifact.gc":
		data = in
		count = 1
	case "ai.project.text", "ai.project.vector":
		if c.Project == nil {
			return nil, errors.New("projection.unavailable")
		}
		data, count, err = c.Project(ctx, t.Kind, in)
		if err != nil {
			return nil, err
		}
	default:
		return nil, asynccontrol.ErrInvalid
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if len(raw) > 128<<20 {
		return nil, errors.New("ai.artifact.too_large")
	}
	hash, raw, err := asynccontrol.Digest(raw)
	if err != nil {
		return nil, err
	}
	artifact := Artifact{ObjectID: in.Intent.ObjectID, ContentGeneration: in.Intent.ContentGeneration, ArtifactID: uuid.NewSHA1(t.ID, []byte("artifact:"+t.InputDigest+":"+stringInt(t.Generation))), TaskID: t.ID, Generation: t.Generation, Kind: t.Kind, InputDigest: t.InputDigest, ArtifactHash: hash, TypedOutputRef: outputID, RowCount: count, ByteCount: int64(len(raw)), Complete: true, Data: raw}
	if err := c.DB.WithContext(ctx).Create(&artifact).Error; err != nil {
		return nil, err
	}
	out := aiprotocol.Output{Version: 2, TaskID: t.ID, Generation: t.Generation, InputDigest: t.InputDigest, ArtifactID: artifact.ArtifactID, ArtifactHash: artifact.ArtifactHash, OutputID: outputID, ObjectID: in.Intent.ObjectID, ContentGeneration: in.Intent.ContentGeneration, RowCount: count, ByteCount: int64(len(raw)), Coverage: coverage, Complete: true}
	if t.Kind == "ai.intent.apply" {
		var object Object
		q := c.DB.WithContext(ctx).Table("ai_published_objects").Where("object_id=? AND content_generation=?", in.Intent.ObjectID, in.Intent.ContentGeneration).Find(&object)
		if q.Error != nil {
			return nil, q.Error
		}
		if object.ActiveSourceID != nil {
			out.SourceID = *object.ActiveSourceID
		}
		if object.ActiveContentVersionID != nil {
			out.ContentVersionID = *object.ActiveContentVersionID
		}
		if object.ActiveEmbeddingSetID != nil {
			out.EmbeddingSetID = *object.ActiveEmbeddingSetID
		}
	}
	out.RequestedSeq = seq(in, map[string]string{"ai.source.prepare": "source", "ai.text.build": "text", "ai.embedding.generate": "vector", "ai.summary.generate": "summary"}[t.Kind])
	return json.Marshal(out)
}
func readBlocks(ctx context.Context, db *gorm.DB, id uuid.UUID) ([]rag.Section, error) {
	var rows []struct {
		Text, Kind, HeadingPath string
		Location                json.RawMessage
	}
	if err := db.WithContext(ctx).Table("ai_published_blocks").Where("source_id=?", id).Order("ordinal").Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("ai.source.input_missing")
	}
	blocks := []rag.Section{}
	for _, r := range rows {
		var b rag.Section
		if err := json.Unmarshal(r.Location, &b); err != nil {
			return nil, err
		}
		b.Text = r.Text
		b.Kind = r.Kind
		b.HeadingPath = r.HeadingPath
		blocks = append(blocks, b)
	}
	return blocks, nil
}
