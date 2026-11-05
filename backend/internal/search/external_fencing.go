package search

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/11bronx11/bx_yunpan/backend/internal/textsearch"
	"github.com/google/uuid"
)

// A tombstone wins a tie. File versions, ACL versions and index epochs are
// independent watermarks; advancing one must not roll another back.
const esProjectionScript = `
def d = params.doc;
if ((d.containsKey('file_version') && ctx._source.containsKey('file_version') && ctx._source.file_version > d.file_version) ||
    (d.containsKey('index_epoch') && ctx._source.containsKey('index_epoch') && ctx._source.index_epoch > d.index_epoch) ||
    (d.containsKey('acl_version') && ctx._source.containsKey('acl_version') && ctx._source.acl_version > d.acl_version)) {
  ctx.op = 'noop';
} else if (ctx._source.tombstone == true && d.tombstone != true &&
    (d.containsKey('file_version') ? ctx._source.file_version >= d.file_version : ctx._source.index_epoch >= d.index_epoch)) {
  ctx.op = 'noop';
} else {
  ctx._source.putAll(d);
}`

func esUpdate(doc map[string]any) map[string]any {
	return map[string]any{"scripted_upsert": true, "script": map[string]any{"lang": "painless", "source": esProjectionScript, "params": map[string]any{"doc": doc}}, "upsert": map[string]any{}}
}

func esContentDocument(value ContentProjection) map[string]any {
	doc := map[string]any{"chunk_id": value.ChunkID, "object_id": value.ObjectID, "document_version_id": value.DocumentVersionID, "ordinal": value.Ordinal, "content": value.Content, "pipeline_version": value.PipelineVersion, "model_version": value.ModelVersion, "index_epoch": value.IndexEpoch, "tombstone": value.Tombstone}
	doc["content_normalized"] = textsearch.Normalize(value.Content)
	doc["content_search"] = textsearch.IndexText(value.Content)
	doc["lexical_version"] = textsearch.Version
	if vector, err := parseVectorRef(value.VectorRef); err == nil && len(vector) > 0 {
		doc["embedding"] = vector
	}
	return doc
}

func esFileDocument(value FileProjection) map[string]any {
	if value.ACLVersion <= 0 {
		value.ACLVersion = 1
	}
	return map[string]any{"file_entry_id": value.FileEntryID, "owner_id": value.OwnerID, "folder_id": value.FolderID, "object_id": value.ObjectID, "file_version": value.FileVersion, "acl_version": value.ACLVersion, "name": value.Name, "mime_type": value.MIMEType, "index_epoch": value.IndexEpoch, "tombstone": value.Tombstone}
}

func objectTombstoneID(id uuid.UUID) string { return "object-tombstone-" + id.String() }

// Keep an object-level watermark even if deletion precedes the first chunk.
// The marker has a different ID from content and cannot be replaced by upsert.
func (e *ElasticsearchIndex) writeObjectTombstone(ctx context.Context, target string, id uuid.UUID, epoch int64) error {
	doc := map[string]any{"object_id": id, "index_epoch": epoch, "tombstone": true}
	return e.do(ctx, http.MethodPost, "/chunks_"+safeIndexPart(target)+"/_update/"+url.PathEscape(objectTombstoneID(id))+"?retry_on_conflict=5", esUpdate(doc), nil)
}

func (e *ElasticsearchIndex) visibleCandidates(ctx context.Context, target string, candidates []RetrievalCandidate) ([]RetrievalCandidate, error) {
	if len(candidates) == 0 {
		return candidates, nil
	}
	ids := make([]string, 0, len(candidates))
	seen := make(map[uuid.UUID]bool)
	for _, candidate := range candidates {
		if !seen[candidate.ObjectID] {
			ids = append(ids, objectTombstoneID(candidate.ObjectID))
			seen[candidate.ObjectID] = true
		}
	}
	var response struct {
		Docs []struct {
			Found  bool `json:"found"`
			Error  any  `json:"error"`
			Source struct {
				ObjectID uuid.UUID `json:"object_id"`
				Epoch    int64     `json:"index_epoch"`
			} `json:"_source"`
		} `json:"docs"`
	}
	// _mget is realtime; search refresh lag must not hide a completed delete.
	if err := e.do(ctx, http.MethodPost, "/chunks_"+safeIndexPart(target)+"/_mget", map[string]any{"ids": ids}, &response); err != nil {
		return nil, err
	}
	if len(response.Docs) != len(ids) {
		return nil, fmt.Errorf("%w: incomplete tombstone lookup", ErrUnavailable)
	}
	deleted := make(map[uuid.UUID]int64)
	for _, doc := range response.Docs {
		if doc.Error != nil {
			return nil, fmt.Errorf("%w: tombstone lookup failed", ErrUnavailable)
		}
		if doc.Found {
			deleted[doc.Source.ObjectID] = doc.Source.Epoch
		}
	}
	result := candidates[:0]
	for _, candidate := range candidates {
		if epoch, found := deleted[candidate.ObjectID]; !found || candidate.IndexEpoch > epoch {
			result = append(result, candidate)
		}
	}
	return result, nil
}

func esVisibleFilter(request RetrievalRequest) map[string]any {
	filter := []any{map[string]any{"exists": map[string]any{"field": "chunk_id"}}}
	if request.IndexEpoch > 0 {
		filter = append(filter, map[string]any{"term": map[string]any{"index_epoch": request.IndexEpoch}})
	}
	return map[string]any{"bool": map[string]any{"filter": filter, "must_not": []any{map[string]any{"term": map[string]any{"tombstone": true}}}}}
}
