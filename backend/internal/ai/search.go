package ai

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	searchindex "github.com/11bronx11/bx_yunpan/backend/internal/search"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"go.opentelemetry.io/otel/attribute"
)

type searchCandidate struct {
	FileID            uuid.UUID
	OwnerID           uuid.UUID
	FolderID          uuid.UUID
	ObjectID          uuid.UUID
	DocumentID        uuid.UUID
	DocumentVersionID uuid.UUID
	Name              string
	SizeBytes         int64
	MimeType          string
	Version           int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
	ChunkID           *uuid.UUID
	PageNumber        *int
	Section           *string
	Content           *string
	Score             float64
}

const (
	maxSearchQueryRunes = 1000
	maxQuestionRunes    = 2000
	maxAskFileIDs       = 50
	hybridRecallLimit   = 50
	hybridFusionLimit   = 30
)

// Search 是检索入口。span 开在参数归一化之后，属性才反映真正生效的值。
func (s *Service) Search(ctx context.Context, ownerID uuid.UUID, input SearchInput) ([]SearchHit, error) {
	input.Query = strings.TrimSpace(input.Query)
	if input.Query == "" || len([]rune(input.Query)) > maxSearchQueryRunes || len(input.MimeTypes) > 20 ||
		(input.Mode != "name" && input.Mode != "fulltext" && input.Mode != "semantic" && input.Mode != "hybrid") {
		return nil, ErrInvalid
	}
	if input.Limit <= 0 {
		input.Limit = 20
	}
	if input.Limit > 100 {
		input.Limit = 100
	}
	ctx, span := startSpan(ctx, "ai.search",
		attribute.String("ai.search_mode", input.Mode),
		attribute.Int("ai.limit", input.Limit),
	)
	hits, err := s.search(ctx, ownerID, input)
	if err == nil {
		span.SetAttributes(attribute.Int("ai.hit_count", len(hits)))
	}
	endSpan(span, err)
	return hits, err
}

func (s *Service) search(ctx context.Context, ownerID uuid.UUID, input SearchInput) ([]SearchHit, error) {
	if input.FolderID != nil {
		if _, err := s.drive.Folder(ownerID, *input.FolderID); err != nil {
			return nil, ErrNotFound
		}
	}
	if input.Mode == "name" {
		candidates, err := s.recall(ctx, ownerID, input, "name", "")
		return groupCandidates(candidates, "name", input.Limit), err
	}
	if input.Mode == "fulltext" {
		candidates, err := s.recall(ctx, ownerID, input, "fulltext", "")
		return groupCandidates(candidates, "fulltext", input.Limit), err
	}
	embeddings, err := s.embedQuery(ctx, input.Query)
	if errors.Is(err, errProviderQuota) {
		return nil, ErrQuota
	}
	if err != nil || len(embeddings) != 1 {
		return nil, ErrUnavailable
	}
	vector := vectorLiteral(embeddings[0])
	if input.Mode == "semantic" {
		candidates, err := s.recall(ctx, ownerID, input, "semantic", vector)
		return groupCandidates(candidates, "semantic", input.Limit), err
	}
	// 双路先保留至少 50 个候选，再做最多 30 个 chunk 的融合池；最终
	// response 仍遵守调用方的 limit，Ask 可直接把内部融合池交给 reranker。
	recallInput := input
	if recallInput.Limit < hybridRecallLimit {
		recallInput.Limit = hybridRecallLimit
	}
	var (
		fulltext, semantic []searchCandidate
		fulltextErr        error
		semanticErr        error
		wait               sync.WaitGroup
	)
	wait.Add(2)
	go func() {
		defer wait.Done()
		fulltext, fulltextErr = s.recall(ctx, ownerID, recallInput, "fulltext", "")
	}()
	go func() {
		defer wait.Done()
		semantic, semanticErr = s.recall(ctx, ownerID, recallInput, "semantic", vector)
	}()
	wait.Wait()
	if err := errors.Join(fulltextErr, semanticErr); err != nil {
		return nil, err
	}
	fused := fuse(ctx, fulltext, semantic, max(input.Limit, hybridFusionLimit))
	if len(fused) > input.Limit {
		fused = fused[:input.Limit]
	}
	return fused, nil
}

// Ask 是问答入口：一个父 span 下面挂 ai.search（检索）与 ai.llm（生成），
// 一眼能看出"慢在检索还是慢在大模型"。
func (s *Service) Ask(ctx context.Context, ownerID uuid.UUID, question string, folderID *uuid.UUID, fileIDs []uuid.UUID) (string, []Citation, error) {
	question = strings.TrimSpace(question)
	if question == "" || len([]rune(question)) > maxQuestionRunes || len(fileIDs) > maxAskFileIDs {
		return "", nil, ErrInvalid
	}
	ctx, span := startSpan(ctx, "ai.ask",
		attribute.Int("ai.scoped_file_count", len(fileIDs)),
		attribute.Bool("ai.folder_scoped", folderID != nil),
	)
	answerText, citations, err := s.ask(ctx, ownerID, question, folderID, fileIDs)
	if err == nil {
		span.SetAttributes(attribute.Int("ai.citation_count", len(citations)))
	}
	endSpan(span, err)
	return answerText, citations, err
}

func (s *Service) ask(ctx context.Context, ownerID uuid.UUID, question string, folderID *uuid.UUID, fileIDs []uuid.UUID) (string, []Citation, error) {
	hits, err := s.Search(ctx, ownerID, SearchInput{
		Query: question, Mode: "hybrid", FolderID: folderID, IncludeSubfolders: true, FileIDs: fileIDs, Limit: hybridFusionLimit,
	})
	if err != nil {
		return "", nil, err
	}
	candidateLimit := s.config.RerankCandidates
	if candidateLimit <= 0 {
		candidateLimit = hybridFusionLimit
	}
	candidates := make([]searchCandidate, 0, candidateLimit)
	seenChunks := make(map[uuid.UUID]struct{}, candidateLimit)
	for _, hit := range hits {
		for _, candidate := range hit.candidates {
			if candidate.ChunkID == nil {
				continue
			}
			if _, exists := seenChunks[*candidate.ChunkID]; exists {
				continue
			}
			seenChunks[*candidate.ChunkID] = struct{}{}
			candidates = append(candidates, candidate)
			if len(candidates) == candidateLimit {
				break
			}
		}
		if len(candidates) == candidateLimit {
			break
		}
	}
	candidates, err = s.loadFullChunks(ctx, ownerID, candidates)
	if err != nil {
		return "", nil, ErrUnavailable
	}
	if s.reranker != nil && len(candidates) > 1 {
		ordered, rerankErr := s.rerank(ctx, question, candidates)
		if rerankErr == nil {
			candidates = ordered
		}
	}
	maxEvidence := s.config.RerankTopK
	if maxEvidence <= 0 {
		maxEvidence = 8
	}
	evidence := make([]Evidence, 0, maxEvidence)
	citationByID := make(map[string]Citation, 8)
	perFile := make(map[uuid.UUID]int)
	for _, candidate := range candidates {
		if perFile[candidate.FileID] >= 2 || candidate.Content == nil {
			continue
		}
		citation := candidateCitation(candidate)
		if citation.ID == "" {
			continue
		}
		evidence = append(evidence, Evidence{ID: citation.ID, FileName: citation.FileName, Content: *candidate.Content})
		citationByID[citation.ID] = citation
		perFile[candidate.FileID]++
		if len(evidence) == maxEvidence {
			break
		}
	}
	answer, err := s.answer(ctx, question, evidence)
	if errors.Is(err, errProviderQuota) {
		return "", nil, ErrQuota
	}
	if err != nil {
		return "", nil, ErrUnavailable
	}
	citations := make([]Citation, 0, len(answer.CitationIDs))
	for _, id := range answer.CitationIDs {
		if citation, ok := citationByID[id]; ok {
			citations = append(citations, citation)
		}
	}
	return answer.Answer, citations, nil
}

type fullChunkRow struct {
	ChunkID    uuid.UUID
	DocumentID uuid.UUID
	FileID     uuid.UUID
	Content    string
}

func (s *Service) loadFullChunks(ctx context.Context, ownerID uuid.UUID, candidates []searchCandidate) ([]searchCandidate, error) {
	if len(candidates) == 0 {
		return candidates, nil
	}
	chunkIDs := make([]string, 0, len(candidates))
	fileIDs := make([]string, 0, len(candidates))
	seenChunks := make(map[uuid.UUID]struct{}, len(candidates))
	seenFiles := make(map[uuid.UUID]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate.ChunkID != nil {
			if _, exists := seenChunks[*candidate.ChunkID]; !exists {
				seenChunks[*candidate.ChunkID] = struct{}{}
				chunkIDs = append(chunkIDs, candidate.ChunkID.String())
			}
		}
		if _, exists := seenFiles[candidate.FileID]; !exists {
			seenFiles[candidate.FileID] = struct{}{}
			fileIDs = append(fileIDs, candidate.FileID.String())
		}
	}
	var rows []fullChunkRow
	err := s.db.WithContext(ctx).Raw(`
		SELECT c.id AS chunk_id, d.id AS document_id, e.id AS file_id, c.content
		FROM ai_chunks c
		JOIN ai_documents d ON d.id = c.document_id
		JOIN ai_document_versions v ON (v.id = d.active_version_id OR (d.active_version_id IS NULL AND v.document_id = d.id)) AND v.status = 'indexed'
			AND (c.document_version_id = v.id OR (c.document_version_id IS NULL AND c.document_id = d.id AND (d.active_version_id IS NULL OR d.updated_at > v.updated_at)))
		JOIN file_entries e ON e.object_id = d.object_id AND e.deleted_at IS NULL
		JOIN file_objects o ON o.id = e.object_id AND o.status = 'ready'
		WHERE c.id = ANY(CAST(@chunk_ids AS uuid[]))
		  AND e.id = ANY(CAST(@file_ids AS uuid[]))
		  AND e.owner_id = @owner`,
		sql.Named("chunk_ids", pq.Array(chunkIDs)), sql.Named("file_ids", pq.Array(fileIDs)), sql.Named("owner", ownerID),
	).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]string, len(rows))
	for _, row := range rows {
		byKey[row.FileID.String()+":"+row.ChunkID.String()] = row.Content
	}
	result := make([]searchCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.ChunkID == nil {
			continue
		}
		content, ok := byKey[candidate.FileID.String()+":"+candidate.ChunkID.String()]
		if !ok {
			continue
		}
		candidate.Content = &content
		result = append(result, candidate)
	}
	return result, nil
}

func (s *Service) queryCandidates(ctx context.Context, ownerID uuid.UUID, input SearchInput, mode, vector string) ([]searchCandidate, error) {
	prefix := ""
	folderFilter := "TRUE"
	if input.FolderID != nil && input.IncludeSubfolders {
		prefix = `WITH RECURSIVE folder_scope AS (
			SELECT id FROM folders WHERE id = @folder AND owner_id = @owner AND deleted_at IS NULL
			UNION ALL
			SELECT f.id FROM folders f JOIN folder_scope p ON f.parent_id = p.id
			WHERE f.owner_id = @owner AND f.deleted_at IS NULL
		) `
		folderFilter = "e.folder_id IN (SELECT id FROM folder_scope)"
	} else if input.FolderID != nil {
		folderFilter = "e.folder_id = @folder"
	}
	mimeFilter := "TRUE"
	if len(input.MimeTypes) > 0 {
		mimeFilter = "o.mime_type = ANY(@mime_types)"
	}
	fileFilter := "TRUE"
	if len(input.FileIDs) > 0 {
		fileFilter = "e.id = ANY(CAST(@file_ids AS uuid[]))"
	}
	selectFields := `e.id AS file_id, e.owner_id, e.folder_id, e.object_id, e.name,
		o.size_bytes, o.mime_type, e.version, e.created_at, e.updated_at`
	var query string
	switch mode {
	case "name":
		query = prefix + `SELECT ` + selectFields + `,
			NULL::uuid AS document_id, NULL::uuid AS chunk_id, NULL::integer AS page_number, NULL::text AS section, NULL::text AS content,
			CASE
				WHEN e.name_normalized = lower(@query) THEN 1.0
				WHEN e.name_normalized ILIKE lower(@query) || '%' THEN 0.75 + similarity(e.name_normalized, lower(@query)) * 0.25
				ELSE similarity(e.name_normalized, lower(@query))
			END AS score
		FROM file_entries e JOIN file_objects o ON o.id = e.object_id AND o.status = 'ready'
			WHERE e.owner_id = @owner AND e.deleted_at IS NULL AND ` + fileFilter + ` AND ` + folderFilter + ` AND ` + mimeFilter + `
			AND (e.name_normalized ILIKE '%' || lower(@query) || '%' OR similarity(e.name_normalized, lower(@query)) > 0.1)
		ORDER BY score DESC, e.created_at DESC LIMIT @candidate_limit`
	case "fulltext":
		query = prefix + `SELECT ` + selectFields + `,
			d.id AS document_id, c.id AS chunk_id, c.page_number, c.section, c.content,
			ts_rank_cd(c.content_search_tsv, plainto_tsquery('simple', ai_search_terms(@query, true)), 32)
				+ CASE WHEN strpos(lower(normalize(c.content, NFKC)), lower(normalize(@query, NFKC))) > 0 THEN 1.0 ELSE 0.0 END AS score
		FROM file_entries e
		JOIN file_objects o ON o.id = e.object_id AND o.status = 'ready'
		JOIN ai_documents d ON d.object_id = e.object_id
		JOIN ai_document_versions v ON (v.id = d.active_version_id OR (d.active_version_id IS NULL AND v.document_id = d.id)) AND v.status = 'indexed'
		JOIN ai_chunks c ON c.document_id = d.id AND (c.document_version_id = v.id OR (c.document_version_id IS NULL AND c.document_id = d.id AND (d.active_version_id IS NULL OR d.updated_at > v.updated_at)))
			WHERE e.owner_id = @owner AND e.deleted_at IS NULL AND ` + fileFilter + ` AND ` + folderFilter + ` AND ` + mimeFilter + `
			AND c.content_search_tsv @@ plainto_tsquery('simple', ai_search_terms(@query, true))
		ORDER BY score DESC, e.id, c.ordinal LIMIT @candidate_limit`
	case "semantic":
		query = prefix + `SELECT ` + selectFields + `,
			d.id AS document_id, c.id AS chunk_id, c.page_number, c.section, c.content,
			1 - (c.embedding <=> CAST(@embedding AS vector)) AS score
		FROM file_entries e
		JOIN file_objects o ON o.id = e.object_id AND o.status = 'ready'
		JOIN ai_documents d ON d.object_id = e.object_id
		JOIN ai_document_versions v ON (v.id = d.active_version_id OR (d.active_version_id IS NULL AND v.document_id = d.id)) AND v.status = 'indexed'
		JOIN ai_chunks c ON c.document_id = d.id AND (c.document_version_id = v.id OR (c.document_version_id IS NULL AND c.document_id = d.id AND (d.active_version_id IS NULL OR d.updated_at > v.updated_at)))
			WHERE e.owner_id = @owner AND e.deleted_at IS NULL AND ` + fileFilter + ` AND ` + folderFilter + ` AND ` + mimeFilter + `
			AND d.model_version = @model_version AND d.pipeline_version = @pipeline_version
		ORDER BY c.embedding <=> CAST(@embedding AS vector) LIMIT @candidate_limit`
	default:
		return nil, ErrInvalid
	}
	candidateLimit := max(input.Limit, 40)
	fileIDs := make([]string, 0, len(input.FileIDs))
	for _, fileID := range input.FileIDs {
		fileIDs = append(fileIDs, fileID.String())
	}
	arguments := []any{
		sql.Named("owner", ownerID), sql.Named("query", input.Query), sql.Named("embedding", vector), sql.Named("file_ids", pq.Array(fileIDs)),
		sql.Named("mime_types", pq.Array(input.MimeTypes)), sql.Named("candidate_limit", candidateLimit),
		sql.Named("model_version", s.provider.ModelVersion()),
		sql.Named("pipeline_version", s.pipelineVersion),
	}
	if input.FolderID != nil {
		arguments = append(arguments, sql.Named("folder", *input.FolderID))
	}
	var candidates []searchCandidate
	if err := s.db.WithContext(ctx).Raw(query, arguments...).Scan(&candidates).Error; err != nil {
		return nil, err
	}
	return candidates, nil
}

// queryExternalCandidates treats ES/Qdrant results as opaque candidates and
// rehydrates them through the same owner/scope/object-ready SQL checks used by
// PostgreSQL search. An index can therefore never authorize a result.
func (s *Service) queryExternalCandidates(ctx context.Context, ownerID uuid.UUID, input SearchInput, mode, vector string) ([]searchCandidate, error) {
	request := searchindex.RetrievalRequest{OwnerID: ownerID, TargetVersion: s.searchConfig.IndexEpoch, IndexEpoch: expectedSearchEpoch(s.searchConfig.IndexEpoch), Query: input.Query, FolderID: input.FolderID, FileIDs: input.FileIDs, Limit: max(input.Limit, hybridRecallLimit), Oversample: s.searchConfig.VectorOversample, Timeout: s.searchConfig.ExternalTimeout}
	var external []searchindex.RetrievalCandidate
	var err error
	if mode == "fulltext" {
		external, err = s.retrieval.SearchText(ctx, request)
	} else {
		raw := strings.Trim(vector, "[]")
		fields := strings.Split(raw, ",")
		parsed := make([]float32, 0, len(fields))
		for _, field := range fields {
			if strings.TrimSpace(field) == "" {
				continue
			}
			value, parseErr := strconv.ParseFloat(strings.TrimSpace(field), 32)
			if parseErr != nil {
				return nil, parseErr
			}
			parsed = append(parsed, float32(value))
		}
		external, err = s.retrieval.SearchVector(ctx, request, parsed)
	}
	if err != nil {
		return nil, err
	}
	chunkIDs := make([]string, 0, len(external))
	scoreByChunk := make(map[uuid.UUID]float64, len(external))
	versionByChunk := make(map[uuid.UUID]uuid.UUID, len(external))
	for _, candidate := range external {
		if candidate.ChunkID == uuid.Nil {
			continue
		}
		expectedEpoch := expectedSearchEpoch(s.searchConfig.IndexEpoch)
		if expectedEpoch > 0 && candidate.IndexEpoch > 0 && expectedEpoch != candidate.IndexEpoch {
			// A candidate from another epoch is not safe to merge: it may be
			// missing a tombstone or carry an incompatible embedding model.
			continue
		}
		if candidate.DocumentVersionID == uuid.Nil {
			// External payloads must identify the indexed document generation;
			// SQL rehydration below then proves it is still active.
			continue
		}
		chunkIDs = append(chunkIDs, candidate.ChunkID.String())
		scoreByChunk[candidate.ChunkID] = candidate.Score
		versionByChunk[candidate.ChunkID] = candidate.DocumentVersionID
	}
	if len(chunkIDs) == 0 {
		return nil, nil
	}
	prefix, folderFilter := "", "TRUE"
	if input.FolderID != nil && input.IncludeSubfolders {
		prefix = `WITH RECURSIVE folder_scope AS (SELECT id FROM folders WHERE id = @folder AND owner_id = @owner AND deleted_at IS NULL UNION ALL SELECT f.id FROM folders f JOIN folder_scope p ON f.parent_id = p.id WHERE f.owner_id = @owner AND f.deleted_at IS NULL) `
		folderFilter = "e.folder_id IN (SELECT id FROM folder_scope)"
	} else if input.FolderID != nil {
		folderFilter = "e.folder_id = @folder"
	}
	fileFilter := "TRUE"
	fileIDs := make([]string, 0, len(input.FileIDs))
	for _, id := range input.FileIDs {
		fileIDs = append(fileIDs, id.String())
	}
	if len(fileIDs) > 0 {
		fileFilter = "e.id = ANY(CAST(@file_ids AS uuid[]))"
	}
	mimeFilter, modelFilter := "TRUE", "TRUE"
	if len(input.MimeTypes) > 0 {
		mimeFilter = "o.mime_type = ANY(@mime_types)"
	}
	if mode == "semantic" {
		modelFilter = "v.model_version = @model_version AND v.pipeline_version = @pipeline_version"
	}
	query := prefix + `SELECT e.id AS file_id, e.owner_id, e.folder_id, e.object_id, e.name, o.size_bytes, o.mime_type, e.version, e.created_at, e.updated_at, d.id AS document_id, v.id AS document_version_id, c.id AS chunk_id, c.page_number, c.section, c.content FROM file_entries e JOIN file_objects o ON o.id = e.object_id AND o.status = 'ready' JOIN ai_documents d ON d.object_id = e.object_id JOIN ai_document_versions v ON v.id = d.active_version_id AND v.status = 'indexed' JOIN ai_chunks c ON c.document_id = d.id AND c.document_version_id = v.id WHERE e.owner_id = @owner AND e.deleted_at IS NULL AND ` + fileFilter + ` AND ` + folderFilter + ` AND ` + mimeFilter + ` AND ` + modelFilter + ` AND c.id = ANY(CAST(@chunk_ids AS uuid[]))`
	args := []any{sql.Named("owner", ownerID), sql.Named("chunk_ids", pq.Array(chunkIDs)), sql.Named("file_ids", pq.Array(fileIDs))}
	args = append(args, sql.Named("mime_types", pq.Array(input.MimeTypes)))
	if mode == "semantic" {
		args = append(args, sql.Named("model_version", s.provider.ModelVersion()), sql.Named("pipeline_version", s.pipelineVersion))
	}
	if input.FolderID != nil {
		args = append(args, sql.Named("folder", *input.FolderID))
	}
	var rows []searchCandidate
	if err := s.db.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]searchCandidate, 0, len(rows))
	for _, row := range rows {
		if row.ChunkID == nil {
			continue
		}
		if expected := versionByChunk[*row.ChunkID]; expected != uuid.Nil && row.DocumentVersionID != expected {
			continue
		}
		row.Score = scoreByChunk[*row.ChunkID]
		result = append(result, row)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Score == result[j].Score {
			if result[i].FileID == result[j].FileID {
				return result[i].ChunkID.String() < result[j].ChunkID.String()
			}
			return result[i].FileID.String() < result[j].FileID.String()
		}
		return result[i].Score > result[j].Score
	})
	return result, nil
}

func expectedSearchEpoch(value string) int64 {
	value = strings.TrimSpace(value)
	if len(value) > 1 && (value[0] == 'v' || value[0] == 'V') {
		value = value[1:]
	}
	epoch, _ := strconv.ParseInt(value, 10, 64)
	return epoch
}

func groupCandidates(candidates []searchCandidate, matchType string, limit int) []SearchHit {
	hits := make([]SearchHit, 0, limit)
	indexes := map[uuid.UUID]int{}
	for _, candidate := range candidates {
		index, exists := indexes[candidate.FileID]
		if !exists {
			if len(hits) == limit {
				continue
			}
			indexes[candidate.FileID] = len(hits)
			hits = append(hits, SearchHit{
				File: candidateFile(candidate), Score: candidate.Score, MatchType: matchType, Citations: []Citation{}, candidates: []searchCandidate{},
			})
			index = len(hits) - 1
		}
		if candidate.Score > hits[index].Score {
			hits[index].Score = candidate.Score
		}
		if candidate.ChunkID != nil && candidate.Content != nil && len(hits[index].Citations) < 3 {
			hits[index].Citations = append(hits[index].Citations, candidateCitation(candidate))
			hits[index].candidates = append(hits[index].candidates, candidate)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	return hits
}

func reciprocalRankFusion(fulltext, semantic []searchCandidate, limit int) []SearchHit {
	type fused struct {
		candidate searchCandidate
		score     float64
	}
	// Hybrid retrieval is chunk-level. A file can contribute several distinct
	// chunks to the RRF pool, which is the candidate set the Ask reranker sees.
	items := map[string]*fused{}
	add := func(candidates []searchCandidate) {
		for rank, candidate := range candidates {
			key := candidate.FileID.String()
			if candidate.ChunkID != nil {
				key += ":" + candidate.ChunkID.String()
			}
			item := items[key]
			if item == nil {
				item = &fused{candidate: candidate}
				items[key] = item
			}
			item.score += 1 / float64(60+rank+1)
			if candidate.Score > item.candidate.Score {
				item.candidate = candidate
			}
		}
	}
	add(fulltext)
	add(semantic)
	ordered := make([]*fused, 0, len(items))
	for _, item := range items {
		item.candidate.Score = item.score
		ordered = append(ordered, item)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].score == ordered[j].score {
			left, right := "", ""
			if ordered[i].candidate.ChunkID != nil {
				left = ordered[i].candidate.ChunkID.String()
			}
			if ordered[j].candidate.ChunkID != nil {
				right = ordered[j].candidate.ChunkID.String()
			}
			return left < right
		}
		return ordered[i].score > ordered[j].score
	})
	if limit > 0 && len(ordered) > limit {
		ordered = ordered[:limit]
	}

	// Keep the public Search contract file-oriented while carrying all fused
	// chunk candidates privately for Ask/reranking.
	hitByFile := make(map[uuid.UUID]int)
	hits := make([]SearchHit, 0, len(ordered))
	for _, item := range ordered {
		candidate := item.candidate
		index, exists := hitByFile[candidate.FileID]
		if !exists {
			hitByFile[candidate.FileID] = len(hits)
			hits = append(hits, SearchHit{
				File: candidateFile(candidate), Score: item.score, MatchType: "hybrid",
				Citations: []Citation{}, candidates: []searchCandidate{},
			})
			index = len(hits) - 1
		}
		if item.score > hits[index].Score {
			hits[index].Score = item.score
			hits[index].File = candidateFile(candidate)
		}
		hits[index].candidates = append(hits[index].candidates, candidate)
		if candidate.ChunkID != nil && len(hits[index].Citations) < 3 {
			hits[index].Citations = append(hits[index].Citations, candidateCitation(candidate))
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	return hits
}

func candidateFile(value searchCandidate) map[string]any {
	return map[string]any{
		"type": "file", "id": value.FileID, "owner_id": value.OwnerID, "folder_id": value.FolderID,
		"object_id": value.ObjectID, "name": value.Name, "size_bytes": value.SizeBytes, "mime_type": value.MimeType,
		"version": value.Version, "created_at": value.CreatedAt, "updated_at": value.UpdatedAt,
	}
}

func candidateCitation(value searchCandidate) Citation {
	id := ""
	if value.ChunkID != nil {
		id = value.FileID.String() + ":" + value.ChunkID.String()
	}
	excerpt := ""
	if value.Content != nil {
		excerpt = limitRunes(strings.Join(strings.Fields(*value.Content), " "), 320)
	}
	return Citation{ID: id, FileID: value.FileID, FileName: value.Name, PageNumber: value.PageNumber, Section: value.Section, Excerpt: excerpt}
}
