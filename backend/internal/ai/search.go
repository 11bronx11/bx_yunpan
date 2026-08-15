package ai

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

type searchCandidate struct {
	FileID     uuid.UUID
	OwnerID    uuid.UUID
	FolderID   uuid.UUID
	ObjectID   uuid.UUID
	Name       string
	SizeBytes  int64
	MimeType   string
	Version    int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ChunkID    *uuid.UUID
	PageNumber *int
	Section    *string
	Content    *string
	Score      float64
}

const (
	maxSearchQueryRunes = 1000
	maxQuestionRunes    = 2000
)

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
	if input.FolderID != nil {
		if _, err := s.drive.Folder(ownerID, *input.FolderID); err != nil {
			return nil, ErrNotFound
		}
	}
	if input.Mode == "name" {
		candidates, err := s.queryCandidates(ctx, ownerID, input, "name", "")
		return groupCandidates(candidates, "name", input.Limit), err
	}
	if input.Mode == "fulltext" {
		candidates, err := s.queryCandidates(ctx, ownerID, input, "fulltext", "")
		return groupCandidates(candidates, "fulltext", input.Limit), err
	}
	embeddings, err := s.provider.Embeddings(ctx, []string{input.Query})
	if errors.Is(err, errProviderQuota) {
		return nil, ErrQuota
	}
	if err != nil || len(embeddings) != 1 {
		return nil, ErrUnavailable
	}
	vector := vectorLiteral(embeddings[0])
	if input.Mode == "semantic" {
		candidates, err := s.queryCandidates(ctx, ownerID, input, "semantic", vector)
		return groupCandidates(candidates, "semantic", input.Limit), err
	}
	fulltext, err := s.queryCandidates(ctx, ownerID, input, "fulltext", "")
	if err != nil {
		return nil, err
	}
	semantic, err := s.queryCandidates(ctx, ownerID, input, "semantic", vector)
	if err != nil {
		return nil, err
	}
	return reciprocalRankFusion(fulltext, semantic, input.Limit), nil
}

func (s *Service) Ask(ctx context.Context, ownerID uuid.UUID, question string, folderID *uuid.UUID, fileIDs []uuid.UUID) (string, []Citation, error) {
	question = strings.TrimSpace(question)
	if question == "" || len([]rune(question)) > maxQuestionRunes {
		return "", nil, ErrInvalid
	}
	hits, err := s.Search(ctx, ownerID, SearchInput{
		Query: question, Mode: "hybrid", FolderID: folderID, IncludeSubfolders: true, Limit: 40,
	})
	if err != nil {
		return "", nil, err
	}
	allowed := map[uuid.UUID]bool{}
	for _, fileID := range fileIDs {
		allowed[fileID] = true
	}
	evidence := make([]Evidence, 0, 8)
	citationByID := make(map[string]Citation, 8)
	perFile := make(map[uuid.UUID]int)
	for _, hit := range hits {
		fileID, ok := hit.File["id"].(uuid.UUID)
		if !ok || (len(allowed) > 0 && !allowed[fileID]) {
			continue
		}
		for _, citation := range hit.Citations {
			if perFile[fileID] >= 2 {
				break
			}
			if citation.ID == "" {
				continue
			}
			evidence = append(evidence, Evidence{ID: citation.ID, FileName: citation.FileName, Content: citation.Excerpt})
			citationByID[citation.ID] = citation
			perFile[fileID]++
			if len(evidence) == 8 {
				break
			}
		}
		if len(evidence) == 8 {
			break
		}
	}
	answer, err := s.provider.Answer(ctx, question, evidence)
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
	selectFields := `e.id AS file_id, e.owner_id, e.folder_id, e.object_id, e.name,
		o.size_bytes, o.mime_type, e.version, e.created_at, e.updated_at`
	var query string
	switch mode {
	case "name":
		query = prefix + `SELECT ` + selectFields + `,
			NULL::uuid AS chunk_id, NULL::integer AS page_number, NULL::text AS section, NULL::text AS content,
			CASE
				WHEN e.name_normalized = lower(@query) THEN 1.0
				WHEN e.name_normalized ILIKE lower(@query) || '%' THEN 0.75 + similarity(e.name_normalized, lower(@query)) * 0.25
				ELSE similarity(e.name_normalized, lower(@query))
			END AS score
		FROM file_entries e JOIN file_objects o ON o.id = e.object_id AND o.status = 'ready'
		WHERE e.owner_id = @owner AND e.deleted_at IS NULL AND ` + folderFilter + ` AND ` + mimeFilter + `
			AND (e.name_normalized ILIKE '%' || lower(@query) || '%' OR similarity(e.name_normalized, lower(@query)) > 0.1)
		ORDER BY score DESC, e.created_at DESC LIMIT @candidate_limit`
	case "fulltext":
		query = prefix + `SELECT ` + selectFields + `,
			c.id AS chunk_id, c.page_number, c.section, c.content,
			ts_rank_cd(c.content_tsv, plainto_tsquery('simple', @query)) AS score
		FROM file_entries e
		JOIN file_objects o ON o.id = e.object_id AND o.status = 'ready'
		JOIN ai_documents d ON d.object_id = e.object_id AND d.status = 'indexed'
		JOIN ai_chunks c ON c.document_id = d.id
		WHERE e.owner_id = @owner AND e.deleted_at IS NULL AND ` + folderFilter + ` AND ` + mimeFilter + `
			AND c.content_tsv @@ plainto_tsquery('simple', @query)
		ORDER BY score DESC LIMIT @candidate_limit`
	case "semantic":
		query = prefix + `SELECT ` + selectFields + `,
			c.id AS chunk_id, c.page_number, c.section, c.content,
			1 - (c.embedding <=> CAST(@embedding AS vector)) AS score
		FROM file_entries e
		JOIN file_objects o ON o.id = e.object_id AND o.status = 'ready'
		JOIN ai_documents d ON d.object_id = e.object_id AND d.status = 'indexed'
		JOIN ai_chunks c ON c.document_id = d.id
		WHERE e.owner_id = @owner AND e.deleted_at IS NULL AND ` + folderFilter + ` AND ` + mimeFilter + `
			AND d.model_version = @model_version AND d.pipeline_version = @pipeline_version
		ORDER BY c.embedding <=> CAST(@embedding AS vector) LIMIT @candidate_limit`
	default:
		return nil, ErrInvalid
	}
	arguments := []any{
		sql.Named("owner", ownerID), sql.Named("query", input.Query), sql.Named("embedding", vector),
		sql.Named("mime_types", pq.Array(input.MimeTypes)), sql.Named("candidate_limit", max(input.Limit*4, 40)),
		sql.Named("model_version", s.provider.ModelVersion()),
		sql.Named("pipeline_version", pipelineVersion),
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
				File: candidateFile(candidate), Score: candidate.Score, MatchType: matchType, Citations: []Citation{},
			})
			index = len(hits) - 1
		}
		if candidate.Score > hits[index].Score {
			hits[index].Score = candidate.Score
		}
		if candidate.ChunkID != nil && candidate.Content != nil && len(hits[index].Citations) < 3 {
			hits[index].Citations = append(hits[index].Citations, candidateCitation(candidate))
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	return hits
}

func reciprocalRankFusion(fulltext, semantic []searchCandidate, limit int) []SearchHit {
	type fused struct {
		candidate searchCandidate
		score     float64
		citations []searchCandidate
	}
	items := map[uuid.UUID]*fused{}
	add := func(candidates []searchCandidate) {
		seen := map[uuid.UUID]bool{}
		rank := 0
		for _, candidate := range candidates {
			if seen[candidate.FileID] {
				continue
			}
			seen[candidate.FileID] = true
			rank++
			item := items[candidate.FileID]
			if item == nil {
				item = &fused{candidate: candidate}
				items[candidate.FileID] = item
			}
			item.score += 1 / float64(60+rank)
			if candidate.ChunkID != nil && len(item.citations) < 3 {
				item.citations = append(item.citations, candidate)
			}
		}
	}
	add(fulltext)
	add(semantic)
	ordered := make([]*fused, 0, len(items))
	for _, item := range items {
		ordered = append(ordered, item)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].score > ordered[j].score })
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	hits := make([]SearchHit, 0, len(ordered))
	for _, item := range ordered {
		citations := make([]Citation, 0, len(item.citations))
		seen := map[string]bool{}
		for _, candidate := range item.citations {
			citation := candidateCitation(candidate)
			if !seen[citation.ID] {
				citations = append(citations, citation)
				seen[citation.ID] = true
			}
		}
		hits = append(hits, SearchHit{File: candidateFile(item.candidate), Score: item.score, MatchType: "hybrid", Citations: citations})
	}
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
	id := fmt.Sprintf("%s:%s", value.FileID, value.ChunkID)
	excerpt := ""
	if value.Content != nil {
		excerpt = limitRunes(strings.Join(strings.Fields(*value.Content), " "), 320)
	}
	return Citation{ID: id, FileID: value.FileID, FileName: value.Name, PageNumber: value.PageNumber, Section: value.Section, Excerpt: excerpt}
}
