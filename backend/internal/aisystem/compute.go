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
	Vision             ai.Provider
	MaxObjectBytes     int64
	Profile            aiprotocol.Profile
	HTTP               *http.Client
	SummaryCalls       int
	Project            func(context.Context, string, aiprotocol.Input) (any, int64, error)
}

// Keep provider response details out of task errors while retaining their
// classification for the worker's retry decision.
type modelBuildError struct {
	stage string
	cause error
}

func (e *modelBuildError) Error() string {
	if ai.IsProviderPermanent(e.cause) {
		return e.stage + ".rejected"
	}
	return e.stage + ".unavailable"
}
func (e *modelBuildError) Unwrap() error { return e.cause }

func IsPermanentComputeError(err error) bool {
	return errors.Is(err, asynccontrol.ErrInvalid) || ai.IsProviderPermanent(err) || ai.IsParsePermanent(err) || ai.IsParseUnsupported(err) || ai.IsParseResourceLimit(err)
}

func (c *Computer) Compute(ctx context.Context, t asynccontrol.Task) (body json.RawMessage, computeErr error) {
	ctx, finishTrace := stageTrace(ctx, t.Kind)
	defer func() { finishTrace(computeErr) }()
	in, err := decode[aiprotocol.Input](t.Input)
	started := time.Now()
	defer func() {
		slog.InfoContext(ctx, "AI computation completed", "task_id", t.ID, "request_id", in.Intent.RequestID, "job_id", in.Intent.JobID, "object_id", in.Intent.ObjectID, "content_generation", in.Intent.ContentGeneration, "kind", t.Kind, "duration_ms", time.Since(started).Milliseconds(), "failed", computeErr != nil)
	}()
	if err != nil || (t.Kind != "ai.artifact.gc" && in.Operation != "retire" && in.Intent.Validate() != nil) || (!computeProfileCompatible(t.Kind, in.Profile, c.Profile, in.Intent.MediaType) && in.Operation != "delete" && in.Operation != "retire") {
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
		if c.Core == nil || (!aiprotocol.VisionImage(in.Intent.MediaType) && c.Parser == nil) {
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
		limit := c.MaxObjectBytes
		if limit <= 0 {
			limit = 64 << 20
		}
		if obj.Size < 0 || obj.Size > limit {
			return nil, &ai.ParseError{Code: "ai.parse.resource_limit", ResourceLimit: true}
		}
		if obj.Size != in.Intent.Size {
			return nil, asynccontrol.ErrInvalid
		}
		bytes, err := io.ReadAll(io.LimitReader(resp.Body, obj.Size+1))
		if err != nil || int64(len(bytes)) != obj.Size || aiprotocol.Hash(bytes) != obj.Sha256 || obj.Sha256 != in.Intent.ContentHash {
			return nil, errors.New("ai.source.hash_mismatch")
		}
		var parsed ai.ParseResult
		if aiprotocol.VisionImage(in.Intent.MediaType) {
			parsed, err = ai.ParseVisionImage(ctx, c.Vision, in.Intent.MediaType, bytes, 40_000_000)
		} else {
			parsed, err = c.Parser.Parse(ctx, ai.ParseRequest{RunID: t.ID, ObjectID: in.Intent.ObjectID, DocumentVersionID: outputID, ContentHash: in.Intent.ContentHash, MediaType: in.Intent.MediaType, ObjectAddress: ai.ObjectReadAddress{Data: bytes, MediaType: in.Intent.MediaType}, ParserProfileVersion: in.Profile.Parser, SizeLimitBytes: limit, PageLimit: 1000, PixelLimit: 40_000_000, ChunkerVersion: in.Profile.Chunker})
		}
		if err != nil {
			return nil, err
		}
		parsed.Chunks = nil // source publication never depends on parser-side chunks.
		if len(parsed.Sections) == 0 || !slices.ContainsFunc(parsed.Sections, func(s ai.Section) bool { return strings.TrimSpace(s.Text) != "" }) {
			return nil, &ai.ParseError{Code: "ai.parse.quality_failed", Permanent: true}
		}
		count = int64(len(parsed.Sections))
		coverage = "complete"
		if parsed.ChunksTruncated || parsed.Quality.EmptyPageRatio > 0 {
			coverage = "partial"
		}
		data = sourceData{Parsed: parsed, Coverage: coverage}
	case "ai.text.build":
		blocks, err := readBlocks(ctx, c.DB, in.SourceID)
		if err != nil {
			return nil, err
		}
		chunks, err := rag.BuildParentChildVersion(outputID, blocks, in.Profile.Chunker)
		if errors.Is(err, rag.ErrParentChildResourceLimit) {
			return nil, &ai.ParseError{Code: "ai.text.resource_limit", ResourceLimit: true}
		}
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
			ID          uuid.UUID
			Content     string
			HeadingPath string
		}
		if err := c.DB.WithContext(ctx).Table("ai_published_children").Where("content_version_id=?", in.ContentVersionID).Order("ordinal").Find(&children).Error; err != nil {
			return nil, err
		}
		if len(children) == 0 {
			return nil, errors.New("ai.vector.input_missing")
		}
		vectors := vectorData{}
		for offset := 0; offset < len(children); offset += ai.EmbeddingBatchSize {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			end := min(offset+ai.EmbeddingBatchSize, len(children))
			texts := []string{}
			for _, ch := range children[offset:end] {
				texts = append(texts, embeddingText(in.Profile.Chunker, ch.HeadingPath, ch.Content))
			}
			vs, err := c.Embedding.Embeddings(ctx, texts)
			if err != nil {
				return nil, &modelBuildError{stage: "embedding", cause: err}
			}
			if len(vs) != len(texts) {
				return nil, asynccontrol.ErrInvalid
			}
			for n, v := range vs {
				if !validVector(v, 1024) {
					return nil, asynccontrol.ErrInvalid
				}
				vectors.Items = append(vectors.Items, vectorItem{children[offset+n].ID, normalizedVector(v), aiprotocol.Hash([]byte(texts[n]))})
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
		insight, summaries, err := c.summarizeTask(ctx, parts, t, in)
		if err != nil {
			return nil, err
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

func computeProfileCompatible(kind string, frozen, current aiprotocol.Profile, mediaType string) bool {
	// Only compare live dependencies this task actually calls. Frozen metadata,
	// published inputs and projection model identities survive unrelated upgrades.
	switch kind {
	case "ai.intent.apply", "ai.project.text", "ai.project.vector", "ai.object.tombstone", "ai.capability.finish", "ai.artifact.gc":
		return true
	case "ai.source.prepare":
		if aiprotocol.VisionImage(mediaType) {
			return frozen.VisionProvider == current.VisionProvider && frozen.VisionModel == current.VisionModel
		}
		return frozen.Parser == current.Parser
	case "ai.text.build":
		// Each retained implementation executes its original IDs and exact ranges.
		return rag.IsParentChildVersion(frozen.Chunker)
	case "ai.embedding.generate":
		return frozen.EmbeddingProvider == current.EmbeddingProvider && frozen.EmbeddingModel == current.EmbeddingModel &&
			frozen.EmbeddingRevision == current.EmbeddingRevision && frozen.Dimension == current.Dimension
	case "ai.summary.generate":
		return frozen.SummaryProvider == current.SummaryProvider && frozen.SummaryModel == current.SummaryModel &&
			frozen.SummaryRevision == current.SummaryRevision && frozen.Prompt == current.Prompt
	default:
		return false
	}
}
