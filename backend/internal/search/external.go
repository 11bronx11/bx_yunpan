package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type ElasticsearchIndex struct {
	endpoint string
	username string
	password string
	client   *http.Client
}

// EpochPreparer creates deterministic mappings/collections before a snapshot
// starts. It is optional on the Index interface so existing fake indexes stay
// useful in unit tests.
type EpochPreparer interface {
	PrepareEpoch(context.Context, string, int64) error
}

// BulkItemError identifies one failed item in an Elasticsearch bulk response.
// Successful items are intentionally not returned as a second state: callers
// can safely retry the complete batch because document IDs are stable.
type BulkItemError struct {
	ID         string
	StatusCode int
	Reason     string
	Retryable  bool
}

func NewElasticsearchIndex(endpoint, username, password string, client *http.Client) *ElasticsearchIndex {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &ElasticsearchIndex{endpoint: strings.TrimRight(endpoint, "/"), username: username, password: password, client: client}
}

func (e *ElasticsearchIndex) Health(ctx context.Context) error {
	var value map[string]any
	return e.do(ctx, http.MethodGet, "/_cluster/health", nil, &value)
}

func (e *ElasticsearchIndex) PrepareEpoch(ctx context.Context, target string, dimension int64) error {
	if dimension <= 0 {
		dimension = 1024
	}
	fileMapping := map[string]any{"mappings": map[string]any{"properties": map[string]any{
		"file_entry_id": map[string]any{"type": "keyword"}, "owner_id": map[string]any{"type": "keyword"}, "folder_id": map[string]any{"type": "keyword"}, "object_id": map[string]any{"type": "keyword"}, "file_version": map[string]any{"type": "long"}, "acl_version": map[string]any{"type": "long"}, "name": map[string]any{"type": "text"}, "mime_type": map[string]any{"type": "keyword"}, "index_epoch": map[string]any{"type": "long"}, "tombstone": map[string]any{"type": "boolean"},
	}}}
	chunkMapping := map[string]any{"mappings": map[string]any{"properties": map[string]any{
		"chunk_id": map[string]any{"type": "keyword"}, "object_id": map[string]any{"type": "keyword"}, "document_version_id": map[string]any{"type": "keyword"}, "ordinal": map[string]any{"type": "integer"}, "content": map[string]any{"type": "text"}, "content_normalized": map[string]any{"type": "text"}, "content_search": map[string]any{"type": "text", "analyzer": "whitespace"}, "lexical_version": map[string]any{"type": "keyword"}, "pipeline_version": map[string]any{"type": "keyword"}, "model_version": map[string]any{"type": "keyword"}, "index_epoch": map[string]any{"type": "long"}, "tombstone": map[string]any{"type": "boolean"}, "embedding": map[string]any{"type": "dense_vector", "dims": dimension, "index": true, "similarity": "cosine"},
	}}}
	// Keep index creation order deterministic for operators and resumable
	// backfill evidence; map iteration would otherwise make the request trace
	// nondeterministic.
	for _, item := range []struct {
		name    string
		mapping map[string]any
	}{
		{name: "files_" + safeIndexPart(target), mapping: fileMapping},
		{name: "chunks_" + safeIndexPart(target), mapping: chunkMapping},
	} {
		name, mapping := item.name, item.mapping
		if err := e.do(ctx, http.MethodPut, "/"+name, mapping, nil); err != nil {
			var httpErr *HTTPError
			if errors.As(err, &httpErr) && isExistingIndexError(httpErr) {
				// Existing mappings are immutable during an epoch. Both ES 7 and
				// ES 8 have used conflict-like responses for a repeated PUT; only
				// the explicit resource-already-exists classification is ignored.
				continue
			}
			return err
		}
	}
	return nil
}

func isExistingIndexError(err *HTTPError) bool {
	if err == nil {
		return false
	}
	return err.StatusCode == http.StatusConflict ||
		(err.StatusCode == http.StatusBadRequest && err.ErrorType == "resource_already_exists_exception")
}

func (e *ElasticsearchIndex) UpsertContent(ctx context.Context, value ContentProjection) error {
	index := "chunks_" + safeIndexPart(value.TargetVersion)
	return e.do(ctx, http.MethodPost, "/"+index+"/_update/"+url.PathEscape(value.ChunkID.String())+"?retry_on_conflict=5", esUpdate(esContentDocument(value)), nil)
}

func (e *ElasticsearchIndex) UpsertFile(ctx context.Context, value FileProjection) error {
	index := "files_" + safeIndexPart(value.TargetVersion)
	return e.do(ctx, http.MethodPost, "/"+index+"/_update/"+url.PathEscape(value.FileEntryID.String())+"?retry_on_conflict=5", esUpdate(esFileDocument(value)), nil)
}

// BulkUpsertContent uses the Elasticsearch NDJSON bulk endpoint and preserves
// partial failure information. A non-empty error means at least one item must
// be retried; already accepted items remain idempotent on replay.
func (e *ElasticsearchIndex) BulkUpsertContent(ctx context.Context, values []ContentProjection) []BulkItemError {
	if len(values) == 0 {
		return nil
	}
	lines := make([]string, 0, len(values)*2)
	ids := make([]string, 0, len(values))
	for _, value := range values {
		index := "chunks_" + safeIndexPart(value.TargetVersion)
		meta, _ := json.Marshal(map[string]any{"update": map[string]any{"_index": index, "_id": value.ChunkID.String(), "retry_on_conflict": 5}})
		doc, _ := json.Marshal(esUpdate(esContentDocument(value)))
		lines = append(lines, string(meta), string(doc))
		ids = append(ids, value.ChunkID.String())
	}
	return e.bulk(ctx, strings.Join(lines, "\n")+"\n", ids)
}

// BulkUpsertFile is the file projection equivalent of BulkUpsertContent.
func (e *ElasticsearchIndex) BulkUpsertFile(ctx context.Context, values []FileProjection) []BulkItemError {
	if len(values) == 0 {
		return nil
	}
	lines := make([]string, 0, len(values)*2)
	ids := make([]string, 0, len(values))
	for _, value := range values {
		index := "files_" + safeIndexPart(value.TargetVersion)
		meta, _ := json.Marshal(map[string]any{"update": map[string]any{"_index": index, "_id": value.FileEntryID.String(), "retry_on_conflict": 5}})
		doc, _ := json.Marshal(esUpdate(esFileDocument(value)))
		lines = append(lines, string(meta), string(doc))
		ids = append(ids, value.FileEntryID.String())
	}
	return e.bulk(ctx, strings.Join(lines, "\n")+"\n", ids)
}

func (e *ElasticsearchIndex) TombstoneObjectContent(ctx context.Context, backend string, objectID uuid.UUID, epoch int64) error {
	if err := e.writeObjectTombstone(ctx, backend, objectID, epoch); err != nil {
		return err
	}
	body := map[string]any{"script": map[string]any{"source": "if (ctx._source.index_epoch <= params.epoch) { ctx._source.tombstone=true; ctx._source.index_epoch=params.epoch; } else { ctx.op='noop'; }", "params": map[string]any{"epoch": epoch}}, "query": map[string]any{"term": map[string]any{"object_id": objectID.String()}}}
	var response struct {
		Failures []any `json:"failures"`
		TimedOut bool  `json:"timed_out"`
	}
	if err := e.do(ctx, http.MethodPost, "/chunks_"+safeIndexPart(backend)+"/_update_by_query?conflicts=proceed&refresh=true", body, &response); err != nil {
		return err
	}
	if len(response.Failures) > 0 || response.TimedOut {
		return fmt.Errorf("%w: incomplete object tombstone update", ErrUnavailable)
	}
	return nil
}

func (e *ElasticsearchIndex) TombstoneFile(ctx context.Context, backend string, fileID uuid.UUID, version int64) error {
	return e.do(ctx, http.MethodPost, "/files_"+safeIndexPart(backend)+"/_update/"+url.PathEscape(fileID.String())+"?retry_on_conflict=5", esUpdate(map[string]any{"file_entry_id": fileID, "tombstone": true, "file_version": version}), nil)
}

func (e *ElasticsearchIndex) SearchText(ctx context.Context, request RetrievalRequest) ([]RetrievalCandidate, error) {
	limit := boundedLimit(request.Limit)
	body := map[string]any{"size": limit, "track_total_hits": false, "timeout": timeoutString(request.Timeout), "query": map[string]any{"bool": map[string]any{"must": esTextQuery(request.Query), "filter": esVisibleFilter(request)}}}
	var response esSearchResponse
	if err := e.do(ctx, http.MethodPost, "/chunks_"+safeIndexPart(request.TargetVersion)+"/_search", body, &response); err != nil {
		return nil, err
	}
	if err := response.complete(); err != nil {
		return nil, err
	}
	result := make([]RetrievalCandidate, 0, len(response.Hits.Hits))
	for _, hit := range response.Hits.Hits {
		if candidate, ok := candidateFromMap(hit.Source, hit.Score, "text"); ok {
			result = append(result, candidate)
		}
	}
	visible, err := e.visibleCandidates(ctx, request.TargetVersion, result)
	if err == nil && len(response.Hits.Hits) >= limit && len(visible) < len(result) {
		return nil, fmt.Errorf("%w: deleted candidates exhausted retrieval budget", ErrUnavailable)
	}
	return visible, err
}

func (e *ElasticsearchIndex) SearchVector(ctx context.Context, request RetrievalRequest, vector []float32) ([]RetrievalCandidate, error) {
	if len(vector) == 0 {
		return nil, &HTTPError{Backend: "elasticsearch", StatusCode: http.StatusBadRequest, Retryable: false}
	}
	limit := boundedLimit(request.Limit)
	numCandidates := limit
	if request.Oversample > 1 && limit <= 200/request.Oversample {
		numCandidates = limit * request.Oversample
	}
	body := map[string]any{"knn": map[string]any{"field": "embedding", "query_vector": vector, "k": limit, "num_candidates": numCandidates, "filter": esVisibleFilter(request)}, "size": limit, "track_total_hits": false, "timeout": timeoutString(request.Timeout)}
	var response esSearchResponse
	if err := e.do(ctx, http.MethodPost, "/chunks_"+safeIndexPart(request.TargetVersion)+"/_search", body, &response); err != nil {
		return nil, err
	}
	if err := response.complete(); err != nil {
		return nil, err
	}
	result := make([]RetrievalCandidate, 0, len(response.Hits.Hits))
	for _, hit := range response.Hits.Hits {
		if candidate, ok := candidateFromMap(hit.Source, hit.Score, "vector"); ok {
			result = append(result, candidate)
		}
	}
	visible, err := e.visibleCandidates(ctx, request.TargetVersion, result)
	if err == nil && len(response.Hits.Hits) >= limit && len(visible) < len(result) {
		return nil, fmt.Errorf("%w: deleted candidates exhausted retrieval budget", ErrUnavailable)
	}
	return visible, err
}

func (e *ElasticsearchIndex) bulk(ctx context.Context, payload string, ids []string) []BulkItemError {
	if strings.TrimSpace(e.endpoint) == "" {
		return []BulkItemError{{Reason: "elasticsearch endpoint is required", Retryable: false}}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint+"/_bulk", strings.NewReader(payload))
	if err != nil {
		return []BulkItemError{{Reason: "create bulk request failed", Retryable: true}}
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if e.username != "" {
		req.SetBasicAuth(e.username, e.password)
	}
	response, err := e.client.Do(req)
	if err != nil {
		return []BulkItemError{{Reason: "elasticsearch request failed", Retryable: true}}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return []BulkItemError{{StatusCode: response.StatusCode, Reason: "elasticsearch bulk request rejected", Retryable: response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500}}
	}
	var result struct {
		Items []map[string]struct {
			ID     string `json:"_id"`
			Status int    `json:"status"`
			Error  any    `json:"error"`
		} `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&result); err != nil {
		return []BulkItemError{{Reason: "invalid elasticsearch bulk response", Retryable: true}}
	}
	if len(result.Items) != len(ids) {
		return []BulkItemError{{Reason: "incomplete elasticsearch bulk response", Retryable: true}}
	}
	errorsFound := make([]BulkItemError, 0)
	for index, item := range result.Items {
		if len(item) != 1 {
			return []BulkItemError{{Reason: "invalid elasticsearch bulk item", Retryable: true}}
		}
		for _, update := range item {
			if update.Status >= 200 && update.Status < 300 && update.Error == nil {
				continue
			}
			id := update.ID
			if id == "" && index < len(ids) {
				id = ids[index]
			}
			errorsFound = append(errorsFound, BulkItemError{ID: id, StatusCode: update.Status, Reason: fmt.Sprint(update.Error), Retryable: update.Status == http.StatusConflict || update.Status == http.StatusRequestTimeout || update.Status == http.StatusTooManyRequests || update.Status >= 500})
		}
	}
	return errorsFound
}

type QdrantIndex struct {
	endpoint  string
	apiKey    string
	dimension int
	client    *http.Client
}

func NewQdrantIndex(endpoint, apiKey string, dimension int, client *http.Client) *QdrantIndex {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if dimension <= 0 {
		dimension = 1024
	}
	return &QdrantIndex{endpoint: strings.TrimRight(endpoint, "/"), apiKey: apiKey, dimension: dimension, client: client}
}

func (q *QdrantIndex) Health(ctx context.Context) error {
	var value map[string]any
	return q.do(ctx, http.MethodGet, "/collections", nil, &value)
}

func (q *QdrantIndex) PrepareEpoch(ctx context.Context, target string, _ int64) error {
	return q.ensureCollection(ctx, target)
}
func (q *QdrantIndex) UpsertContent(ctx context.Context, value ContentProjection) error {
	if value.Tombstone {
		if err := q.writeTombstone(ctx, value.TargetVersion, "chunk", value.ChunkID, value.IndexEpoch); err != nil {
			return err
		}
		return q.maskTombstone(ctx, value.TargetVersion, "chunk", value.ChunkID, value.IndexEpoch)
	}
	vector, err := parseVectorRef(value.VectorRef)
	if err != nil {
		return err
	}
	if len(vector) != q.dimension {
		return fmt.Errorf("qdrant: vector dimension %d, want %d", len(vector), q.dimension)
	}
	if err := q.ensureCollection(ctx, value.TargetVersion); err != nil {
		return err
	}
	body := map[string]any{"points": []any{qdrantContentPoint(value, vector)}}
	if err := q.do(ctx, http.MethodPut, "/collections/chunks_"+safeIndexPart(value.TargetVersion)+"/points?wait=true&ordering=strong", body, nil); err != nil {
		return err
	}
	return q.maskDeletedPoints(ctx, value.TargetVersion, []ContentProjection{value})
}

// BulkUpsertContent maps a batch to one Qdrant points request. Validation is
// performed before the network call, and a transport/HTTP failure is reported
// for every item so callers can retain per-item retry state.
func (q *QdrantIndex) BulkUpsertContent(ctx context.Context, values []ContentProjection) []BulkItemError {
	if len(values) == 0 {
		return nil
	}
	points := make([]any, 0, len(values))
	ids := make([]string, 0, len(values))
	validIDs := make([]string, 0, len(values))
	validValues := make([]ContentProjection, 0, len(values))
	failed := make([]BulkItemError, 0)
	for _, value := range values {
		if value.TargetVersion != values[0].TargetVersion {
			failed = append(failed, BulkItemError{ID: value.ChunkID.String(), Reason: "mixed target versions in batch"})
			continue
		}
		if value.Tombstone {
			if err := q.UpsertContent(ctx, value); err != nil {
				failed = append(failed, BulkItemError{ID: value.ChunkID.String(), Reason: limitErrorText(err), Retryable: errors.Is(err, ErrUnavailable)})
			}
			continue
		}
		vector, err := parseVectorRef(value.VectorRef)
		id := value.ChunkID.String()
		ids = append(ids, id)
		if err != nil || len(vector) != q.dimension {
			reason := "invalid vector"
			if err != nil {
				reason = err.Error()
			}
			failed = append(failed, BulkItemError{ID: id, Reason: reason})
			continue
		}
		points = append(points, qdrantContentPoint(value, vector))
		validIDs = append(validIDs, id)
		validValues = append(validValues, value)
	}
	if len(points) == 0 {
		return failed
	}
	if err := q.ensureCollection(ctx, values[0].TargetVersion); err != nil {
		for _, id := range validIDs {
			failed = append(failed, BulkItemError{ID: id, Reason: limitErrorText(err), Retryable: true})
		}
		return failed
	}
	err := q.do(ctx, http.MethodPut, "/collections/chunks_"+safeIndexPart(values[0].TargetVersion)+"/points?wait=true&ordering=strong", map[string]any{"points": points}, nil)
	if err == nil {
		err = q.maskDeletedPoints(ctx, values[0].TargetVersion, validValues)
	}
	if err != nil {
		for _, id := range validIDs {
			failed = append(failed, BulkItemError{ID: id, Reason: limitErrorText(err), Retryable: true})
		}
	}
	return failed
}
func (q *QdrantIndex) UpsertFile(context.Context, FileProjection) error { return nil }
func (q *QdrantIndex) TombstoneObjectContent(ctx context.Context, backend string, objectID uuid.UUID, epoch int64) error {
	if err := q.writeTombstone(ctx, backend, "object", objectID, epoch); err != nil {
		return err
	}
	return q.maskTombstone(ctx, backend, "object", objectID, epoch)
}
func (q *QdrantIndex) TombstoneFile(context.Context, string, uuid.UUID, int64) error { return nil }
func (q *QdrantIndex) SearchText(context.Context, RetrievalRequest) ([]RetrievalCandidate, error) {
	return nil, &HTTPError{Backend: "qdrant", StatusCode: http.StatusNotImplemented}
}
func (q *QdrantIndex) SearchVector(ctx context.Context, request RetrievalRequest, vector []float32) ([]RetrievalCandidate, error) {
	if len(vector) != q.dimension {
		return nil, fmt.Errorf("qdrant: vector dimension %d, want %d", len(vector), q.dimension)
	}
	body := map[string]any{"vector": vector, "limit": boundedLimit(request.Limit), "with_payload": true, "with_vectors": false}
	filter := map[string]any{"must_not": []any{map[string]any{"key": "tombstone", "match": map[string]any{"value": true}}}}
	if request.IndexEpoch > 0 {
		filter["must"] = []any{map[string]any{"key": "index_epoch", "match": map[string]any{"value": request.IndexEpoch}}}
	}
	body["filter"] = filter
	if request.Oversample > 1 {
		body["limit"] = boundedLimit(request.Limit * request.Oversample)
	}
	var response struct {
		Result []struct {
			Score   float64        `json:"score"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}
	if err := q.do(ctx, http.MethodPost, "/collections/chunks_"+safeIndexPart(request.TargetVersion)+"/points/search?consistency=all", body, &response); err != nil {
		return nil, err
	}
	result := make([]RetrievalCandidate, 0, len(response.Result))
	for _, hit := range response.Result {
		if candidate, ok := candidateFromMap(hit.Payload, hit.Score, "vector"); ok {
			result = append(result, candidate)
		}
	}
	visible, err := q.visibleCandidates(ctx, request.TargetVersion, result)
	if err == nil && len(response.Result) >= body["limit"].(int) && len(visible) < boundedLimit(request.Limit) {
		return nil, fmt.Errorf("%w: deleted candidates exhausted retrieval budget", ErrUnavailable)
	}
	return visible, err
}

func (q *QdrantIndex) ensureCollection(ctx context.Context, target string) error {
	body := map[string]any{"vectors": map[string]any{"size": q.dimension, "distance": "Cosine"}}
	err := q.do(ctx, http.MethodPut, "/collections/chunks_"+safeIndexPart(target), body, nil)
	var httpErr *HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusConflict {
		return nil
	}
	return err
}

type externalIndex struct{ indexes []Index }

func NewExternalIndex(indexes ...Index) Index { return &externalIndex{indexes: indexes} }

// Backends is consumed by the backfill control plane to create only the
// version rows that have an actual writer attached.
func (m *externalIndex) Backends() []string {
	result := make([]string, 0, len(m.indexes))
	for _, index := range m.indexes {
		switch index.(type) {
		case *ElasticsearchIndex:
			result = append(result, BackendES)
		case *QdrantIndex:
			result = append(result, BackendQdrant)
		}
	}
	return result
}

func (m *externalIndex) PrepareEpoch(ctx context.Context, target string, dimension int64) error {
	for _, index := range m.indexes {
		if preparer, ok := index.(EpochPreparer); ok {
			if err := preparer.PrepareEpoch(ctx, target, dimension); err != nil {
				return err
			}
		}
	}
	return nil
}
func (m *externalIndex) Health(ctx context.Context) error {
	for _, index := range m.indexes {
		if index != nil {
			if err := index.Health(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}
func (m *externalIndex) UpsertContent(ctx context.Context, value ContentProjection) error {
	return m.each(ctx, func(index Index) error { return index.UpsertContent(ctx, value) })
}
func (m *externalIndex) UpsertFile(ctx context.Context, value FileProjection) error {
	return m.each(ctx, func(index Index) error { return index.UpsertFile(ctx, value) })
}
func (m *externalIndex) TombstoneObjectContent(ctx context.Context, backend string, id uuid.UUID, epoch int64) error {
	return m.each(ctx, func(index Index) error { return index.TombstoneObjectContent(ctx, backend, id, epoch) })
}
func (m *externalIndex) TombstoneFile(ctx context.Context, backend string, id uuid.UUID, version int64) error {
	return m.each(ctx, func(index Index) error { return index.TombstoneFile(ctx, backend, id, version) })
}
func (m *externalIndex) each(ctx context.Context, fn func(Index) error) error {
	for _, index := range m.indexes {
		if index != nil {
			if err := fn(index); err != nil {
				return err
			}
		}
	}
	return nil
}

type esSearchResponse struct {
	TimedOut bool `json:"timed_out"`
	Shards   struct {
		Failed int `json:"failed"`
	} `json:"_shards"`
	Hits *struct {
		Hits []struct {
			Score  float64        `json:"_score"`
			Source map[string]any `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
}

func (r esSearchResponse) complete() error {
	if r.TimedOut || r.Shards.Failed > 0 || r.Hits == nil {
		return fmt.Errorf("%w: incomplete elasticsearch search response", ErrUnavailable)
	}
	return nil
}

func (e *ElasticsearchIndex) do(ctx context.Context, method, path string, body any, result any) error {
	return doJSON(ctx, e.client, e.endpoint, e.username, e.password, method, path, body, result, "elasticsearch")
}
func (q *QdrantIndex) do(ctx context.Context, method, path string, body any, result any) error {
	return doJSON(ctx, q.client, q.endpoint, "", q.apiKey, method, path, body, result, "qdrant")
}
func doJSON(ctx context.Context, client *http.Client, endpoint, username, password, method, path string, body any, result any, backend string) error {
	if strings.TrimSpace(endpoint) == "" {
		return &HTTPError{Backend: backend, Retryable: false}
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(endpoint, "/")+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if username != "" {
		req.SetBasicAuth(username, password)
	}
	if backend == "qdrant" && password != "" {
		req.Header.Set("api-key", password)
	}
	response, err := client.Do(req)
	if err != nil {
		return &HTTPError{Backend: backend, Retryable: true}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		httpErr := &HTTPError{Backend: backend, StatusCode: response.StatusCode, Retryable: response.StatusCode == 408 || response.StatusCode == 429 || response.StatusCode >= 500}
		var payload struct {
			Error struct {
				Type      string `json:"type"`
				Reason    string `json:"reason"`
				RootCause []struct {
					Type   string `json:"type"`
					Reason string `json:"reason"`
				} `json:"root_cause"`
			} `json:"error"`
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&payload); err == nil {
			httpErr.ErrorType = payload.Error.Type
			httpErr.Reason = payload.Error.Reason
			if httpErr.ErrorType == "" && len(payload.Error.RootCause) > 0 {
				httpErr.ErrorType = payload.Error.RootCause[0].Type
			}
			if httpErr.Reason == "" && len(payload.Error.RootCause) > 0 {
				httpErr.Reason = payload.Error.RootCause[0].Reason
			}
		}
		if backend == "elasticsearch" && response.StatusCode == http.StatusConflict && httpErr.ErrorType == "version_conflict_engine_exception" {
			httpErr.Retryable = true
		}
		return httpErr
	}
	if result == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(result)
}

func safeIndexPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "v1"
	}
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}
func boundedLimit(value int) int {
	if value <= 0 {
		return 50
	}
	if value > 200 {
		return 200
	}
	return value
}
func timeoutString(value time.Duration) string {
	if value <= 0 {
		return "2s"
	}
	return value.String()
}
func parseVectorRef(value *string) ([]float32, error) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil, errors.New("qdrant: vector is required")
	}
	raw := strings.TrimSpace(*value)
	raw = strings.TrimPrefix(raw, "[")
	raw = strings.TrimSuffix(raw, "]")
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "(")
	raw = strings.TrimSuffix(strings.TrimSpace(raw), ")")
	if raw == "" {
		return nil, errors.New("qdrant: vector is empty")
	}
	fields := strings.Split(raw, ",")
	result := make([]float32, len(fields))
	for i, field := range fields {
		number, err := strconv.ParseFloat(strings.TrimSpace(field), 32)
		if err != nil {
			return nil, errors.New("qdrant: invalid vector")
		}
		result[i] = float32(number)
	}
	return result, nil
}

func limitErrorText(err error) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	if len(value) > 500 {
		return value[:500]
	}
	return value
}
func candidateFromMap(source map[string]any, score float64, matchType string) (RetrievalCandidate, bool) {
	if deleted, _ := source["tombstone"].(bool); deleted {
		return RetrievalCandidate{}, false
	}
	parse := func(key string) uuid.UUID { value, _ := source[key].(string); id, _ := uuid.Parse(value); return id }
	candidate := RetrievalCandidate{ChunkID: parse("chunk_id"), ObjectID: parse("object_id"), DocumentVersionID: parse("document_version_id"), Score: score, MatchType: matchType}
	if value, ok := source["index_epoch"].(float64); ok {
		candidate.IndexEpoch = int64(value)
	}
	if value, ok := source["pipeline_version"].(string); ok {
		candidate.PipelineVersion = value
	}
	if value, ok := source["model_version"].(string); ok {
		candidate.ModelVersion = value
	}
	if value, ok := source["file_entry_id"].(string); ok {
		id, err := uuid.Parse(value)
		if err == nil {
			candidate.FileEntryID = &id
		}
	}
	if candidate.ChunkID == uuid.Nil || candidate.ObjectID == uuid.Nil {
		return RetrievalCandidate{}, false
	}
	return candidate, true
}

var _ RetrievalBackend = (*ElasticsearchIndex)(nil)
var _ RetrievalBackend = (*QdrantIndex)(nil)
