package aisystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pbv2"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	corepb "github.com/11bronx11/bx_yunpan/backend/internal/core/pb"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/grpcx"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/gorm"
)

type Query struct {
	DisableAsk bool
	pbv2.UnimplementedAIQueryServiceServer
	DB               *gorm.DB
	Core             corepb.CoreReadServiceClient
	Provider         ai.Provider
	Reranker         ai.Reranker
	Profile          aiprotocol.Profile
	Projector        *Projector
	ScopeLimit       int
	EmbeddingCache   ai.QueryEmbeddingCache
	ShadowProjector  *Projector
	ShadowSampleRate float64
	ShadowRecorder   ai.ShadowRecorder
	RerankCandidates int
	RerankTopK       int
}
type candidate struct {
	ID, ParentID, ContentVersionID, SourceID uuid.UUID
	Content, ParentContent, HeadingPath      string
	Location                                 json.RawMessage
	Score                                    float64
	File                                     *corepb.FileRef   `gorm:"-"`
	Aliases                                  []*corepb.FileRef `gorm:"-"`
}
type activeFile struct {
	Object
	File *corepb.FileRef
}

var errVectorScopeNotReady = errors.New("vector.scope_not_ready")

func (q *Query) scope(ctx context.Context, grant string) (*corepb.ScopeResponse, []activeFile, error) {
	opened, err := q.Core.OpenRetrievalScope(ctx, &corepb.OpenScopeRequest{Grant: grant})
	if err != nil {
		return nil, nil, err
	}
	var files []*corepb.FileRef
	cursor := ""
	for {
		page, err := q.Core.StreamScopeMembers(ctx, &corepb.ScopeMembersRequest{Grant: grant, Cursor: cursor, Limit: 200})
		if err != nil {
			return nil, nil, err
		}
		files = append(files, page.Files...)
		if len(files) > max(q.ScopeLimit, 50) {
			return nil, nil, status.Error(codes.ResourceExhausted, "ai.scope_too_large")
		}
		if !page.HasMore {
			break
		}
		if page.NextCursor == cursor || page.NextCursor == "" {
			return nil, nil, status.Error(codes.Internal, "invalid scope cursor")
		}
		cursor = page.NextCursor
	}
	out := []activeFile{}
	for start := 0; start < len(files); start += 200 {
		batch := files[start:min(start+200, len(files))]
		ids := []string{}
		for _, f := range batch {
			ids = append(ids, f.ObjectId)
		}
		var objects []Object
		if err := q.DB.WithContext(ctx).Table("ai_published_objects").Where("object_id::text=ANY(?::text[])", pq.Array(ids)).Find(&objects).Error; err != nil {
			return nil, nil, status.Error(codes.Unavailable, "ai.unavailable")
		}
		for _, f := range batch {
			for _, o := range objects {
				if o.ObjectID.String() == f.ObjectId && o.ContentGeneration == f.ContentGeneration && o.ActiveContentVersionID != nil {
					out = append(out, activeFile{o, f})
					break
				}
			}
		}
	}
	return opened, out, nil
}

func (q *Query) capabilities(ctx context.Context, obj *pbv2.ObjectRef) (*pbv2.Capabilities, error) {
	out := &pbv2.Capabilities{Source: &pbv2.Capability{}, Text: &pbv2.Capability{}, Vector: &pbv2.Capability{}, Summary: &pbv2.Capability{}}
	var o Object
	res := q.DB.WithContext(ctx).Table("ai_published_objects").Where("object_id=? AND content_generation=?", obj.ObjectId, obj.ContentGeneration).Find(&o)
	if res.Error != nil {
		return nil, res.Error
	}
	supported := supportedText(obj.MimeType)
	if res.RowsAffected > 0 {
		supported = supportedText(o.MediaType)
	}
	for _, cap := range []*pbv2.Capability{out.Source, out.Text, out.Vector, out.Summary} {
		cap.Supported = supported
		cap.Freshness = "current"
		cap.Coverage = "none"
		cap.LatestBuild = &pbv2.LatestBuild{State: "pending"}
	}
	out.Vector.Supported = supported && q.Profile.EnableVectors
	out.Summary.Supported = supported && q.Profile.EnableSummary
	if res.RowsAffected == 0 {
		return out, nil
	}
	var statuses []struct {
		Capability                              string
		DesiredSeq, LatestObservedSeq           int64
		LatestBuildState, ErrorCode, ObservedAt string
	}
	if err := q.DB.WithContext(ctx).Table("ai_published_capabilities").Where("object_id=? AND content_generation=?", o.ObjectID, o.ContentGeneration).Find(&statuses).Error; err != nil {
		return nil, err
	}
	for name, c := range map[string]*pbv2.Capability{"source": out.Source, "text": out.Text, "vector": out.Vector, "summary": out.Summary} {
		id := map[string]*uuid.UUID{"source": o.ActiveSourceID, "text": o.ActiveContentVersionID, "vector": o.ActiveEmbeddingSetID, "summary": o.ActiveSummaryID}[name]
		if id != nil {
			c.Available = true
			c.VersionId = id.String()
			c.Coverage = "complete"
			table := map[string]string{"source": "ai_published_sources", "text": "ai_published_content", "summary": "ai_published_summaries"}[name]
			if table != "" {
				if err := q.DB.WithContext(ctx).Table(table).Select("coverage").Where("id=?", *id).Scan(&c.Coverage).Error; err != nil {
					return nil, err
				}
			}
		}
		for _, s := range statuses {
			if s.Capability == name {
				c.LatestBuild = &pbv2.LatestBuild{State: s.LatestBuildState, ErrorCode: s.ErrorCode, ObservedAt: s.ObservedAt}
				if c.Available && s.LatestBuildState != "succeeded" {
					c.Freshness = "previous"
				}
			}
		}
	}
	out.Vector.Supported = supported && q.Profile.EnableVectors
	out.Summary.Supported = supported && q.Profile.EnableSummary
	out.Vector.Available = out.Vector.Available && out.Text.Available
	out.Askable = out.Text.Available
	return out, nil
}

func supportedText(mime string) bool {
	return aiprotocol.SupportedSource(mime)
}

func (q *Query) BatchCapabilities(ctx context.Context, r *pbv2.BatchCapabilitiesRequest) (*pbv2.BatchCapabilitiesResponse, error) {
	if len(r.Objects) > 200 {
		return nil, status.Error(codes.InvalidArgument, "too many objects")
	}
	out := &pbv2.BatchCapabilitiesResponse{}
	for _, o := range r.Objects {
		if _, err := uuid.Parse(o.ObjectId); err != nil || o.ContentGeneration < 1 {
			return nil, status.Error(codes.InvalidArgument, "invalid object")
		}
		caps, err := q.capabilities(ctx, o)
		if err != nil {
			return nil, status.Error(codes.Unavailable, "ai.unavailable")
		}
		out.Items = append(out.Items, &pbv2.ObjectCapabilities{Object: o, Capabilities: caps})
	}
	return out, nil
}

func (q *Query) GetOverview(ctx context.Context, r *pbv2.GetOverviewRequest) (*pbv2.OverviewResponse, error) {
	refs, err := q.Core.BatchGetFileRefs(ctx, &corepb.FileRefsRequest{Grant: r.Grant, FileIds: []string{r.FileId}})
	if err != nil {
		return nil, err
	}
	if len(refs.Files) != 1 {
		return nil, status.Error(codes.NotFound, "file unavailable")
	}
	f := refs.Files[0]
	caps, err := q.capabilities(ctx, &pbv2.ObjectRef{ObjectId: f.ObjectId, ContentGeneration: f.ContentGeneration, MimeType: f.MimeType})
	if err != nil {
		return nil, status.Error(codes.Unavailable, "ai.unavailable")
	}
	out := &pbv2.OverviewResponse{FileId: f.FileId, Capabilities: caps, PreviewKind: "none", PreviewState: "unsupported", Coverage: &pbv2.Coverage{Mode: "source", Exhaustive: false}}
	// Report the versions of these published results, even while a rebuild is
	// pending. The query process's current profile may describe a newer build.
	var models, pipeline []string
	if caps.Source.Available {
		var source struct{ ParserProfileVersion string }
		if err := q.DB.WithContext(ctx).Table("ai_published_sources").Select("parser_profile_version").Where("id=?", caps.Source.VersionId).Take(&source).Error; err != nil {
			return nil, status.Error(codes.Unavailable, "ai.unavailable")
		}
		if source.ParserProfileVersion != "" {
			pipeline = append(pipeline, source.ParserProfileVersion)
		}
	}
	if caps.Text.Available {
		var content struct{ ChunkerVersion string }
		if err := q.DB.WithContext(ctx).Table("ai_published_content").Select("chunker_version").Where("id=?", caps.Text.VersionId).Take(&content).Error; err != nil {
			return nil, status.Error(codes.Unavailable, "ai.unavailable")
		}
		if content.ChunkerVersion != "" {
			pipeline = append(pipeline, content.ChunkerVersion)
		}
	}
	if strings.HasPrefix(f.MimeType, "image/") {
		out.PreviewKind = "image_thumbnail"
		out.PreviewState = f.ThumbnailState
		if out.PreviewState == "" {
			out.PreviewState = "pending"
		}
	}
	if caps.Summary.Supported {
		if out.PreviewKind == "none" {
			out.PreviewKind = "document_summary"
			out.PreviewState = caps.Summary.LatestBuild.State
		}
		if caps.Summary.Available {
			var summary struct {
				Summary, Language, Coverage, ChatModel, StrategyVersion, PromptVersion string
				Tags                                                                   json.RawMessage
			}
			if err := q.DB.WithContext(ctx).Table("ai_published_summaries").Where("id=?", caps.Summary.VersionId).Take(&summary).Error; err != nil {
				return nil, status.Error(codes.Unavailable, "ai.unavailable")
			}
			out.Summary = summary.Summary
			out.Language = summary.Language
			_ = json.Unmarshal(summary.Tags, &out.Tags)
			if out.PreviewKind == "document_summary" {
				out.PreviewState = "available"
			}
			out.Coverage.Exhaustive = summary.Coverage == "complete"
			if summary.ChatModel != "" {
				models = append(models, summary.ChatModel)
			}
			if summary.StrategyVersion != "" {
				pipeline = append(pipeline, summary.StrategyVersion)
			}
			if summary.PromptVersion != "" {
				pipeline = append(pipeline, "prompt:"+summary.PromptVersion)
			}
		}
	}
	if caps.Vector.Available {
		var embedding struct{ Model string }
		if err := q.DB.WithContext(ctx).Table("ai_published_sets").Select("model").Where("id=?", caps.Vector.VersionId).Take(&embedding).Error; err != nil {
			return nil, status.Error(codes.Unavailable, "ai.unavailable")
		}
		if embedding.Model != "" {
			models = append(models, embedding.Model)
		}
	}
	out.ModelVersion = strings.Join(models, "+")
	out.PipelineVersion = strings.Join(pipeline, " · ")
	_, err = q.Core.ValidateEvidence(ctx, &corepb.ValidateEvidenceRequest{Grant: r.Grant, Refs: []*corepb.EvidenceRef{evidenceRef(f)}})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func evidenceRef(f *corepb.FileRef) *corepb.EvidenceRef {
	return &corepb.EvidenceRef{FileId: f.FileId, ObjectId: f.ObjectId, ContentGeneration: f.ContentGeneration, EntryVersion: f.EntryVersion}
}

func (q *Query) validate(ctx context.Context, grant string, candidates []candidate) error {
	refs := []*corepb.EvidenceRef{}
	seen := map[string]bool{}
	for _, c := range candidates {
		for _, f := range candidateFiles(c) {
			if !seen[f.FileId] {
				seen[f.FileId] = true
				refs = append(refs, evidenceRef(f))
			}
		}
	}
	if len(refs) == 0 {
		_, err := q.Core.ValidateEvidence(ctx, &corepb.ValidateEvidenceRequest{Grant: grant})
		return err
	}
	for start := 0; start < len(refs); start += 200 {
		if _, err := q.Core.ValidateEvidence(ctx, &corepb.ValidateEvidenceRequest{Grant: grant, Refs: refs[start:min(start+200, len(refs))]}); err != nil {
			return err
		}
	}
	return nil
}

func uniqueVersions(files []activeFile, vectors bool) ([]string, map[string]*corepb.FileRef) {
	ids := []string{}
	refs := map[string]*corepb.FileRef{}
	for _, f := range files {
		id := f.ActiveContentVersionID
		if vectors {
			id = f.ActiveEmbeddingSetID
		}
		if id != nil {
			ids = append(ids, id.String())
		}
		if f.ActiveContentVersionID != nil {
			if _, exists := refs[f.ActiveContentVersionID.String()]; !exists {
				refs[f.ActiveContentVersionID.String()] = f.File
			}
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids), refs
}

func (q *Query) pgText(ctx context.Context, ids []string, query string, k int) ([]ranked, error) {
	var rows []ranked
	if len(ids) == 0 {
		return rows, nil
	}
	query = strings.TrimSpace(query)
	err := q.DB.WithContext(ctx).Raw(`SELECT id,
		ts_rank_cd(content_tsv,ai_fts_query(?),32)
		+ CASE WHEN strpos(lower(normalize(content,NFKC)),lower(normalize(?,NFKC))) > 0 THEN 1.0 ELSE 0.0 END AS score
		FROM ai_published_children WHERE content_version_id::text=ANY(?::text[])
		AND content_tsv @@ ai_fts_query(?) ORDER BY score DESC,id LIMIT ?`, query, query, pq.Array(ids), query, k).Scan(&rows).Error
	return rows, err
}

// Branch ranks are merged by raw score across all authorized batches before RRF.
// A request embeds its question once, including selected multi-file requests.
func (q *Query) retrieve(ctx context.Context, files []activeFile, query, mode string, k int) ([]candidate, *pbv2.Retrieval, error) {
	return q.retrieveScoped(ctx, files, query, mode, k, false, 1)
}

func mergeRanks(items []ranked, limit int) []ranked {
	scores := map[uuid.UUID]float64{}
	for _, r := range items {
		if v, ok := scores[r.ID]; !ok || r.Score > v {
			scores[r.ID] = r.Score
		}
	}
	out := make([]ranked, 0, len(scores))
	for id, v := range scores {
		out = append(out, ranked{id, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].ID.String() < out[j].ID.String()
		}
		return out[i].Score > out[j].Score
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (q *Query) retrieveScoped(ctx context.Context, files []activeFile, query, mode string, k int, perFile bool, scale int) ([]candidate, *pbv2.Retrieval, error) {
	return q.retrieveWithVector(ctx, files, query, mode, k, perFile, scale, nil)
}

func (q *Query) retrieveWithVector(ctx context.Context, files []activeFile, query, mode string, k int, perFile bool, scale int, queryVector []float32) (candidates []candidate, retrieval *pbv2.Retrieval, retrieveErr error) {
	ctx, finishTrace := stageTrace(ctx, "ai.retrieve")
	defer func() { finishTrace(retrieveErr) }()
	meta := &pbv2.Retrieval{TextBackend: "unavailable", VectorBackend: "unavailable", TokenCountIsEstimate: true}
	texts, refs := uniqueVersions(files, false)
	sets, _ := uniqueVersions(files, true)
	batchSize, branchK, mergeK := 200, k, k
	if mode == "hybrid" {
		// Response top-K must not prune each branch before cross-branch fusion.
		// In particular, a shared rank-2 hit can beat either branch's rank-1 hit.
		branchK, mergeK = max(k, 50), max(k, 50)
	}
	if perFile {
		batchSize, branchK, mergeK = 1, 8*scale, 400*scale
	}
	var text, vector []ranked
	var textErr, vectorErr error
	textExternal := q.Projector != nil && q.Projector.ES != ""
	vectorExternal := q.Projector != nil && q.Projector.QD != ""
	// Coverage is checked for every batch. Any missing proof falls back the whole
	// branch to PG, so partial external coverage never drops eligible members.
	textBranch := func(external bool) ([]ranked, error) {
		var all []ranked
		for n := 0; n < len(texts); n += batchSize {
			ids := texts[n:min(n+batchSize, len(texts))]
			var rows []ranked
			var err error
			if external {
				rows, meta.TextTarget, err = q.Projector.external(ctx, "es", ids, query, nil, branchK)
			} else {
				rows, err = q.pgText(ctx, ids, query, branchK)
			}
			if err != nil {
				return nil, err
			}
			all = append(all, rows...)
		}
		return mergeRanks(all, mergeK), nil
	}
	if mode != "semantic" {
		meta.TextBackend = "postgres"
		if textExternal && len(texts) > 0 {
			text, textErr = textBranch(true)
			if textErr == nil {
				meta.TextBackend = "elasticsearch"
			} else {
				meta.Reasons = append(meta.Reasons, "text.projection_unavailable")
			}
		}
		if meta.TextBackend == "postgres" {
			meta.TextTarget = ""
			text, textErr = textBranch(false)
		}
		if textErr == nil && q.sampleShadow(query, "fulltext") {
			started := time.Now()
			external, err := q.shadowBranch(ctx, "es", texts, query, nil, branchK, batchSize, mergeK)
			q.recordShadow("fulltext", text, external, err, time.Since(started))
		}
	}
	if mode != "fulltext" {
		if !q.Profile.EnableVectors || q.Provider == nil {
			vectorErr = errors.New("vector.not_ready")
		} else if len(sets) == 0 {
			vectorErr = errVectorScopeNotReady
		} else {
			var compatible []string
			err := q.DB.WithContext(ctx).Table("ai_published_sets").Where("id=ANY(?::uuid[]) AND provider=? AND model=? AND revision=? AND dimension=1024", pq.Array(sets), q.Profile.EmbeddingProvider, q.Profile.EmbeddingModel, q.Profile.EmbeddingRevision).Pluck("id::text", &compatible).Error
			if err != nil {
				vectorErr = err
			} else if len(compatible) == 0 {
				vectorErr = errVectorScopeNotReady
			} else {
				if len(compatible) != len(sets) {
					meta.Reasons = append(meta.Reasons, "vector.model_incompatible")
				}
				sets = compatible
				vs := [][]float32{queryVector}
				var err error
				if len(queryVector) == 0 {
					vs, err = q.embedQuery(ctx, query)
				}
				if err != nil || len(vs) != 1 || len(vs[0]) != 1024 {
					vectorErr = errors.New("embedding.unavailable")
					if ai.IsProviderQuota(err) {
						vectorErr = ai.ErrQuota
					}
				} else {
					queryVector = vs[0]
					branch := func(external bool) ([]ranked, error) {
						var all []ranked
						for n := 0; n < len(sets); n += batchSize {
							ids := sets[n:min(n+batchSize, len(sets))]
							var rows []ranked
							var e error
							if external {
								rows, meta.VectorTarget, e = q.Projector.external(ctx, "qd", ids, query, vs[0], branchK)
							} else {
								rows, e = q.pgVector(ctx, ids, vs[0], branchK)
							}
							if e != nil {
								return nil, e
							}
							all = append(all, rows...)
						}
						return mergeRanks(all, mergeK), nil
					}
					meta.VectorBackend = "postgres"
					if vectorExternal {
						vector, vectorErr = branch(true)
						if vectorErr == nil {
							meta.VectorBackend = "qdrant"
						} else {
							meta.Reasons = append(meta.Reasons, "vector.projection_unavailable")
						}
					}
					if meta.VectorBackend == "postgres" {
						meta.VectorTarget = ""
						vector, vectorErr = branch(false)
					}
					if vectorErr == nil && q.sampleShadow(query, "semantic") {
						started := time.Now()
						external, err := q.shadowBranch(ctx, "qd", sets, query, vs[0], branchK, batchSize, mergeK)
						q.recordShadow("semantic", vector, external, err, time.Since(started))
					}
				}
			}
		}
	}
	if mode == "semantic" && vectorErr != nil {
		if errors.Is(vectorErr, ai.ErrQuota) {
			return nil, meta, ai.GRPCStatus(ai.ErrQuota).Err()
		}
		if errors.Is(vectorErr, errVectorScopeNotReady) {
			return nil, meta, grpcx.RequestUnavailable("ai.semantic_unavailable")
		}
		return nil, meta, status.Error(codes.Unavailable, "ai.semantic_unavailable")
	}
	if mode == "fulltext" && textErr != nil || mode == "hybrid" && textErr != nil && vectorErr != nil {
		return nil, meta, status.Error(codes.Unavailable, "ai.unavailable")
	}
	if textErr != nil {
		meta.TextBackend = "unavailable"
		meta.Reasons = append(meta.Reasons, "text.unavailable")
	}
	if vectorErr != nil {
		meta.VectorBackend = "unavailable"
		meta.Reasons = append(meta.Reasons, "vector.unavailable")
		if errors.Is(vectorErr, ai.ErrQuota) {
			meta.Reasons = append(meta.Reasons, "vector.quota_exhausted")
		}
	}
	meta.Degraded = len(meta.Reasons) > 0
	scores := map[uuid.UUID]float64{}
	for _, branch := range [][]ranked{text, vector} {
		for n, r := range branch {
			scores[r.ID] += 1 / float64(61+n)
		}
	}
	ids := []uuid.UUID{}
	for id := range scores {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, meta, nil
	}
	var rows []candidate
	if err := q.DB.WithContext(ctx).Table("ai_published_children c").Select("c.id,c.parent_id,c.content_version_id,c.content,c.heading_path,c.location,p.content AS parent_content,v.source_id").Joins("JOIN ai_published_parents p ON p.id=c.parent_id AND p.content_version_id=c.content_version_id").Joins("JOIN ai_published_content v ON v.id=c.content_version_id").Where("c.id IN ? AND c.content_version_id::text=ANY(?::text[])", ids, pq.Array(texts)).Find(&rows).Error; err != nil {
		return nil, meta, status.Error(codes.Unavailable, "ai.unavailable")
	}
	if len(rows) != len(ids) {
		// An external hit that no longer proves membership cannot consume top-K.
		if textExternal || vectorExternal {
			copy := *q
			copy.Projector = nil
			fresh, m, e := copy.retrieveWithVector(ctx, files, query, mode, k, perFile, scale, queryVector)
			if m != nil {
				m.Degraded = true
				m.Reasons = append(m.Reasons, "projection.stale")
			}
			return fresh, m, e
		}
		return nil, meta, status.Error(codes.Aborted, "ai.scope_changed")
	}
	aliases := map[string][]*corepb.FileRef{}
	for _, f := range files {
		if f.ActiveContentVersionID != nil {
			aliases[f.ActiveContentVersionID.String()] = append(aliases[f.ActiveContentVersionID.String()], f.File)
		}
	}
	for i := range rows {
		rows[i].Score = scores[rows[i].ID]
		rows[i].File = refs[rows[i].ContentVersionID.String()]
		rows[i].Aliases = aliases[rows[i].ContentVersionID.String()]
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Score == rows[j].Score {
			return rows[i].ID.String() < rows[j].ID.String()
		}
		return rows[i].Score > rows[j].Score
	})
	rows = candidatePool(rows, 240*scale, perFile)
	return rows, meta, nil
}

func citation(c candidate, id string) *pbv2.Citation {
	loc, _ := decode[rag.Section](c.Location)
	page := int32(0)
	if loc.PageNumber != nil {
		page = int32(*loc.PageNumber)
	}
	excerpt := []rune(c.Content)
	return &pbv2.Citation{Id: id, FileId: c.File.FileId, FileName: c.File.Name, ContentVersionId: c.ContentVersionID.String(), SourceVersionId: c.SourceID.String(), ParentId: c.ParentID.String(), ChildId: c.ID.String(), ContentGeneration: c.File.ContentGeneration, SourceLocation: &pbv2.SourceLocation{SourceKind: loc.SourceKind, PageStart: page, PageEnd: max(page, int32(loc.PageEnd)), SheetName: loc.SheetName, RowStart: int32(loc.RowStart), RowEnd: int32(loc.RowEnd), ColumnStart: int32(loc.ColumnStart), ColumnEnd: int32(loc.ColumnEnd), HeadingPath: c.HeadingPath, Kind: loc.Kind, SourceStart: loc.SourceStart, SourceEnd: loc.SourceEnd, Encoding: loc.Encoding}, PageNumber: page, Section: c.HeadingPath, Excerpt: string(excerpt[:min(400, len(excerpt))])}
}

func (q *Query) Search(ctx context.Context, r *pbv2.SearchRequest) (response *pbv2.SearchResponse, searchErr error) {
	ctx, finishTrace := stageTrace(ctx, "ai.search")
	defer func() { finishTrace(searchErr) }()
	if strings.TrimSpace(r.Query) == "" || utf8.RuneCountInString(r.Query) > 1000 || len(r.MimeTypes) > 20 || !slices.Contains([]string{"fulltext", "semantic", "hybrid"}, r.Mode) {
		return nil, status.Error(codes.InvalidArgument, "invalid search")
	}
	_, files, err := q.scope(ctx, r.Grant)
	if err != nil {
		return nil, err
	}
	if len(r.MimeTypes) > 0 {
		files = slices.DeleteFunc(files, func(f activeFile) bool { return !slices.Contains(r.MimeTypes, f.File.MimeType) })
	}
	k := int(r.Limit)
	if k <= 0 {
		k = 20
	}
	k = min(k, 100)
	// The response limit counts files. Recall chunks independently so several
	// passages from one document do not consume the entire file result budget.
	rows, meta, err := q.retrieve(ctx, files, r.Query, r.Mode, max(k, 40))
	if err != nil {
		return nil, err
	}
	if err := q.validate(ctx, r.Grant, rows); err != nil {
		return nil, err
	}
	out := &pbv2.SearchResponse{Retrieval: meta, Coverage: &pbv2.Coverage{Mode: "retrieved"}}
	byFile := map[string]*pbv2.SearchHit{}
	for n, c := range rows {
		for _, file := range candidateFiles(c) {
			c.File = file
			hit := byFile[c.File.FileId]
			if hit == nil {
				if len(out.Hits) >= k {
					continue
				}
				hit = &pbv2.SearchHit{File: c.File, Score: c.Score, MatchType: r.Mode}
				byFile[c.File.FileId] = hit
				out.Hits = append(out.Hits, hit)
			}
			if len(hit.Citations) < 5 {
				hit.Citations = append(hit.Citations, citation(c, fmt.Sprintf("c%d", n+1)))
			}
		}
	}
	return out, nil
}

func (q *Query) Ask(ctx context.Context, r *pbv2.AskRequest) (response *pbv2.AskResponse, askErr error) {
	ctx, finishTrace := stageTrace(ctx, "ai.ask")
	defer func() { finishTrace(askErr) }()
	started := time.Now()
	defer func() {
		fields := []any{"request_id", r.RequestId, "duration_ms", time.Since(started).Milliseconds(), "status", status.Code(askErr).String()}
		if response != nil && response.Retrieval != nil {
			fields = append(fields, "text_backend", response.Retrieval.TextBackend, "vector_backend", response.Retrieval.VectorBackend, "degraded", response.Retrieval.Degraded, "reasons", response.Retrieval.Reasons, "evidence_tokens", response.Retrieval.EvidenceTokens, "partial", len(response.Coverage.GetMissingEvidenceFileIds()) > 0)
		}
		slog.InfoContext(ctx, "AI Ask completed", fields...)
	}()
	if q.DisableAsk {
		return nil, status.Error(codes.Unavailable, "ai.answer_unavailable")
	}
	if strings.TrimSpace(r.Question) == "" || utf8.RuneCountInString(r.Question) > 2000 {
		return nil, status.Error(codes.InvalidArgument, "invalid question")
	}
	opened, files, err := q.scope(ctx, r.Grant)
	if err != nil {
		return nil, err
	}
	// Scope is already authenticated by core. Decode only to choose budgets;
	// content access still uses core's verified scope and final evidence check.
	grant, err := scopeBody(r.Grant)
	if err != nil {
		return nil, err
	}
	selected := grant.Scope.Mode == "selected"
	if selected {
		for _, id := range grant.Scope.FileIDs {
			if !slices.ContainsFunc(files, func(f activeFile) bool { return f.File.FileId == id.String() }) {
				st, _ := status.New(codes.FailedPrecondition, "ai.scope_invalid").WithDetails(&errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: id.String(), Description: "text_unavailable"}}})
				return nil, st.Err()
			}
		}
	}
	limits := q.askLimits(selected, len(grant.Scope.FileIDs))
	rows, meta, err := q.retrieveScoped(ctx, files, r.Question, "hybrid", limits.recall, selected && len(grant.Scope.FileIDs) > 1, limits.multiplier)
	if err != nil {
		return nil, err
	}
	rows = candidatePool(rows, limits.candidates, selected && len(grant.Scope.FileIDs) > 1)
	// No candidate text may leave this service before core revalidates it.
	if err := q.validate(ctx, r.Grant, rows); err != nil {
		return nil, err
	}
	if q.Reranker != nil && len(rows) > 0 {
		count := len(rows)
		docs := make([]string, count)
		for n, c := range rows[:count] {
			docs[n] = rag.ContextualText(c.HeadingPath, c.Content, 600)
		}
		rerankCtx, finishRerank := stageTrace(ctx, "ai.rerank")
		scores, e := q.Reranker.Rerank(rerankCtx, r.Question, docs)
		finishRerank(e)
		if e == nil && len(scores) == count {
			for n := 0; n < count; n++ {
				rows[n].Score = scores[n]
			}
			sort.SliceStable(rows[:count], func(i, j int) bool { return rows[i].Score > rows[j].Score })
		} else {
			meta.Degraded = true
			meta.Reasons = append(meta.Reasons, "rerank.unavailable")
		}
	}
	if meta == nil {
		meta = &pbv2.Retrieval{TextBackend: "postgres", VectorBackend: "unavailable", TokenCountIsEstimate: true}
	}
	if err := q.validate(ctx, r.Grant, rows); err != nil {
		return nil, err
	}
	evidence := []ai.Evidence{}
	cites := []*pbv2.Citation{}
	used := []candidate{}
	parents := map[uuid.UUID]bool{}
	represented := map[string]bool{}
	add := func(c candidate) {
		if parents[c.ParentID] || len(evidence) >= limits.parents {
			return
		}
		parentContent := rag.ContextualText(c.HeadingPath, c.ParentContent, 2400)
		tokens := rag.EstimateTokens(parentContent)
		if tokens+int(meta.EvidenceTokens) > limits.evidenceTokens {
			return
		}
		parents[c.ParentID] = true
		id := fmt.Sprintf("c%d", len(cites)+1)
		for _, f := range candidateFiles(c) {
			represented[f.FileId] = true
			alias := c
			alias.File = f
			// One evidence text may support several authorized logical files.
			cites = append(cites, citation(alias, id))
		}
		evidence = append(evidence, ai.Evidence{ID: id, FileName: c.File.Name, Content: parentContent})
		used = append(used, c)
		meta.EvidenceTokens += int32(tokens)
	}
	// A soft first pass offers each relevant file space, then fills by relevance.
	for _, c := range rows {
		if !represented[c.File.FileId] {
			add(c)
		}
	}
	for _, c := range rows {
		add(c)
	}
	coverage := &pbv2.Coverage{Mode: "retrieved", Exhaustive: false}
	if selected {
		for _, id := range grant.Scope.FileIDs {
			if !represented[id.String()] {
				coverage.MissingEvidenceFileIds = append(coverage.MissingEvidenceFileIds, id.String())
			}
		}
	}
	out := &pbv2.AskResponse{Scope: &pbv2.Scope{Mode: grant.Scope.Mode, SnapshotId: opened.SnapshotId}, Retrieval: meta, Coverage: coverage}
	for _, id := range grant.Scope.FileIDs {
		out.Scope.FileIds = append(out.Scope.FileIds, id.String())
	}
	if err := q.validate(ctx, r.Grant, used); err != nil {
		return nil, err
	}
	if len(evidence) == 0 {
		out.Answer = "当前范围内没有找到相关文本证据。"
		return out, nil
	}
	if q.Provider == nil {
		return nil, status.Error(codes.Unavailable, "ai.answer_unavailable")
	}
	if err := q.validate(ctx, r.Grant, used); err != nil {
		return nil, err
	}
	answerCtx, finishAnswer := stageTrace(ctx, "ai.answer")
	options := ai.AnswerOptions{}
	if selected {
		options.MaxTokens = aiprotocol.DefaultAnswerMaxTokens * limits.multiplier
		options.TimeoutMultiplier = limits.multiplier
	}
	answer, err := q.Provider.Answer(answerCtx, r.Question, evidence, options)
	finishAnswer(err)
	if err != nil {
		if ai.IsProviderQuota(err) {
			return nil, ai.GRPCStatus(ai.ErrQuota).Err()
		}
		return nil, status.Error(codes.Unavailable, "ai.answer_unavailable")
	}
	if err := q.validate(ctx, r.Grant, used); err != nil {
		return nil, err
	}
	out.Answer = answer.Answer
	for _, c := range cites {
		if answer.Grounded && slices.Contains(answer.CitationIDs, c.Id) {
			out.Citations = append(out.Citations, c)
		}
	}
	return out, nil
}
