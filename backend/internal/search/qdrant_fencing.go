package search

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

func qdrantContentID(chunk uuid.UUID, epoch int64) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("content:%s:%d", chunk, epoch))).String()
}

// Qdrant 1.15 has no conditional upsert. Deletions are append-only records
// with disjoint IDs, so neither a late upsert nor an older delete can undo one.
// Keep these records until their entire target collection is retired.
func (q *QdrantIndex) writeTombstone(ctx context.Context, target, scope string, subject uuid.UUID, epoch int64) error {
	if err := q.ensureCollection(ctx, target); err != nil {
		return err
	}
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("tombstone:%s:%s:%d", scope, subject, epoch)))
	body := map[string]any{"points": []any{map[string]any{
		"id": id.String(), "vector": make([]float32, q.dimension),
		"payload": map[string]any{"tombstone": true, "deletion_scope": scope, "subject_id": subject.String(), "index_epoch": epoch},
	}}}
	return q.do(ctx, http.MethodPut, "/collections/chunks_"+safeIndexPart(target)+"/points?wait=true&ordering=strong", body, nil)
}

func (q *QdrantIndex) maskDeletedPoints(ctx context.Context, target string, values []ContentProjection) error {
	candidates := make([]RetrievalCandidate, 0, len(values))
	for _, value := range values {
		candidates = append(candidates, RetrievalCandidate{ChunkID: value.ChunkID, ObjectID: value.ObjectID, IndexEpoch: value.IndexEpoch})
	}
	visible, err := q.visibleCandidates(ctx, target, candidates)
	if err != nil {
		return err
	}
	alive := make(map[string]bool, len(visible))
	for _, c := range visible {
		alive[qdrantContentID(c.ChunkID, c.IndexEpoch)] = true
	}
	ids := make([]string, 0)
	for _, c := range candidates {
		id := qdrantContentID(c.ChunkID, c.IndexEpoch)
		if !alive[id] {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return q.do(ctx, http.MethodPost, "/collections/chunks_"+safeIndexPart(target)+"/points/payload?wait=true&ordering=strong", map[string]any{"points": ids, "payload": map[string]any{"tombstone": true}}, nil)
}

func (q *QdrantIndex) maskTombstone(ctx context.Context, target, scope string, subject uuid.UUID, epoch int64) error {
	key := "object_id"
	if scope == "chunk" {
		key = "chunk_id"
	}
	return q.do(ctx, http.MethodPost, "/collections/chunks_"+safeIndexPart(target)+"/points/payload?wait=true&ordering=strong", map[string]any{
		"payload": map[string]any{"tombstone": true}, "filter": map[string]any{"must": []any{
			map[string]any{"key": key, "match": map[string]any{"value": subject.String()}},
			map[string]any{"key": "index_epoch", "range": map[string]any{"lte": epoch}},
		}},
	}, nil)
}

func qdrantContentPoint(value ContentProjection, vector []float32) map[string]any {
	return map[string]any{"id": qdrantContentID(value.ChunkID, value.IndexEpoch), "vector": vector, "payload": map[string]any{
		"chunk_id": value.ChunkID, "object_id": value.ObjectID, "document_version_id": value.DocumentVersionID,
		"index_epoch": value.IndexEpoch, "pipeline_version": value.PipelineVersion, "model_version": value.ModelVersion, "tombstone": false,
	}}
}

func (q *QdrantIndex) visibleCandidates(ctx context.Context, target string, candidates []RetrievalCandidate) ([]RetrievalCandidate, error) {
	if len(candidates) == 0 {
		return candidates, nil
	}
	subjects := make([]string, 0, 2*len(candidates))
	minEpoch := candidates[0].IndexEpoch
	for _, candidate := range candidates {
		subjects = append(subjects, candidate.ObjectID.String(), candidate.ChunkID.String())
		if candidate.IndexEpoch < minEpoch {
			minEpoch = candidate.IndexEpoch
		}
	}
	body := map[string]any{"limit": 256, "with_payload": true, "with_vector": false, "filter": map[string]any{"must": []any{
		map[string]any{"key": "tombstone", "match": map[string]any{"value": true}},
		map[string]any{"key": "subject_id", "match": map[string]any{"any": subjects}},
		map[string]any{"key": "index_epoch", "range": map[string]any{"gte": minEpoch}},
	}}}
	deleted := make(map[string]int64)
	for page := 0; ; page++ {
		if page == 16 {
			return nil, fmt.Errorf("%w: tombstone lookup budget exceeded", ErrUnavailable)
		}
		var response struct {
			Result *struct {
				Points []struct {
					Payload struct {
						Scope   string `json:"deletion_scope"`
						Subject string `json:"subject_id"`
						Epoch   int64  `json:"index_epoch"`
					} `json:"payload"`
				} `json:"points"`
				Next any `json:"next_page_offset"`
			} `json:"result"`
		}
		if err := q.do(ctx, http.MethodPost, "/collections/chunks_"+safeIndexPart(target)+"/points/scroll?consistency=all", body, &response); err != nil {
			return nil, err
		}
		if response.Result == nil {
			return nil, fmt.Errorf("%w: missing tombstone lookup result", ErrUnavailable)
		}
		for _, point := range response.Result.Points {
			key := point.Payload.Scope + ":" + point.Payload.Subject
			if point.Payload.Epoch > deleted[key] {
				deleted[key] = point.Payload.Epoch
			}
		}
		if response.Result.Next == nil {
			break
		}
		body["offset"] = response.Result.Next
	}
	result := make([]RetrievalCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if epoch, found := deleted["object:"+candidate.ObjectID.String()]; found && candidate.IndexEpoch <= epoch {
			continue
		}
		if epoch, found := deleted["chunk:"+candidate.ChunkID.String()]; found && candidate.IndexEpoch <= epoch {
			continue
		}
		result = append(result, candidate)
	}
	return result, nil
}
