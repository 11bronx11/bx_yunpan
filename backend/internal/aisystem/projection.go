package aisystem

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"gorm.io/gorm"
)

type Projector struct {
	DB                                *gorm.DB
	ES, QD, ESUser, ESPassword, QDKey string
	HTTP                              *http.Client
	Epoch                             int64
}
type projectionData struct {
	Backend          string
	TargetID         uuid.UUID
	TargetVersion    string
	Epoch            int64
	ContentID, SetID uuid.UUID
	Count            int
	Hash             string
	ConfigDigest     string
	Operation        string
}
type indexedChild struct {
	ID, ParentID uuid.UUID
	Content      string
	Embedding    string
	Ordinal      int
}

func (p *Projector) request(ctx context.Context, method, url string, body any, backend string, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(raw))
	if err != nil {
		return errors.New("projection.invalid_endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	if backend == "es" && p.ESUser != "" {
		req.SetBasicAuth(p.ESUser, p.ESPassword)
	}
	if backend == "qd" && p.QDKey != "" {
		req.Header.Set("api-key", p.QDKey)
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return errors.New("projection.unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound && (strings.Contains(url, "/_delete_by_query?") || strings.Contains(url, "/points/delete?")) {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("projection.unavailable")
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
	}
	return nil
}
func (p *Projector) Project(ctx context.Context, kind string, in aiprotocol.Input) (any, int64, error) {
	backend := "es"
	if kind == "ai.project.vector" {
		backend = "qd"
	}
	if in.Operation == "delete" || in.Operation == "retire" {
		return p.projectDelete(ctx, backend, in)
	}
	endpoint := p.ES
	if backend == "qd" {
		endpoint = p.QD
	}
	if endpoint == "" {
		return nil, 0, errors.New("projection.not_configured")
	}
	targetID, target, configDigest, err := projectionIdentity(backend, in)
	if err != nil {
		return nil, 0, err
	}
	d := projectionData{Backend: backend, TargetID: targetID, TargetVersion: target, Epoch: in.TargetEpoch, ContentID: in.ContentVersionID, SetID: in.EmbeddingSetID, Operation: "upsert"}
	var rows []indexedChild
	q := p.DB.WithContext(ctx).Table("ai_published_children c").Select("c.id,c.parent_id,c.content,c.ordinal").Where("c.content_version_id=?", in.ContentVersionID).Order("c.ordinal")
	if backend == "qd" {
		q = q.Select("c.id,c.parent_id,c.content,c.ordinal,v.embedding::text AS embedding").Joins("JOIN ai_published_vectors v ON v.child_id=c.id AND v.content_version_id=c.content_version_id AND v.embedding_set_id=?", in.EmbeddingSetID)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	if len(rows) == 0 {
		return nil, 0, errors.New("projection.input_missing")
	}
	if backend == "es" {
		// Index creation is idempotent: an existing mapping is independently checked
		// by every write's immutable version fields and completion manifest.
		_ = p.request(ctx, "PUT", strings.TrimRight(endpoint, "/")+"/"+target, map[string]any{"mappings": map[string]any{"properties": map[string]any{"content": map[string]any{"type": "text"}, "content_id": map[string]any{"type": "keyword"}, "child_id": map[string]any{"type": "keyword"}}}}, backend, nil)
		for _, r := range rows {
			doc := map[string]any{"content": r.Content, "content_id": in.ContentVersionID.String(), "child_id": r.ID.String(), "parent_id": r.ParentID.String()}
			id := in.ContentVersionID.String() + ":" + r.ID.String()
			if err := p.request(ctx, "PUT", strings.TrimRight(endpoint, "/")+"/"+target+"/_doc/"+id+"?refresh=wait_for", doc, backend, nil); err != nil {
				return nil, 0, err
			}
		}
	} else {
		_ = p.request(ctx, "PUT", strings.TrimRight(endpoint, "/")+"/collections/"+target, map[string]any{"vectors": map[string]any{"size": 1024, "distance": "Cosine"}}, backend, nil)
		for offset := 0; offset < len(rows); offset += 64 {
			points := []any{}
			for _, r := range rows[offset:min(offset+64, len(rows))] {
				var vector []float32
				if json.Unmarshal([]byte(r.Embedding), &vector) != nil || len(vector) != 1024 {
					return nil, 0, errors.New("projection.invalid_vector")
				}
				points = append(points, map[string]any{"id": uuid.NewSHA1(in.EmbeddingSetID, []byte(r.ID.String())).String(), "vector": vector, "payload": map[string]any{"set_id": in.EmbeddingSetID.String(), "content_id": in.ContentVersionID.String(), "child_id": r.ID.String()}})
			}
			if err := p.request(ctx, "PUT", strings.TrimRight(endpoint, "/")+"/collections/"+target+"/points?wait=true", map[string]any{"points": points}, backend, nil); err != nil {
				return nil, 0, err
			}
		}
	}
	d.ConfigDigest = configDigest
	d.Count = len(rows)
	d.Hash = aiprotocol.HashJSON(rows)
	return d, int64(len(rows)), nil
}
func publishProjection(tx *gorm.DB, in aiprotocol.Input, a Artifact) error {
	if in.Operation == "delete" || in.Operation == "retire" {
		return publishDelete(tx, in, a)
	}
	d, err := decode[projectionData](a.Data)
	if err != nil || d.Count != int(a.RowCount) || d.Count < 1 || d.ContentID != in.ContentVersionID || (d.Backend == "qd" && d.SetID != in.EmbeddingSetID) || len(d.ConfigDigest) != 64 {
		return errors.New("projection.invalid_manifest")
	}
	expectedBackend := "es"
	if a.Kind == "ai.project.vector" {
		expectedBackend = "qd"
	}
	id, name, digest, identityErr := projectionIdentity(expectedBackend, in)
	if identityErr != nil || d.Backend != expectedBackend || d.TargetID != id || d.TargetVersion != name || d.ConfigDigest != digest || d.Epoch != in.TargetEpoch || d.Operation != "upsert" {
		return errors.New("projection.invalid_target")
	}
	var rows []indexedChild
	queryRows := tx.Table("ai_child_chunks c").Select("c.id,c.parent_id,c.content,c.ordinal").Where("c.content_version_id=?", in.ContentVersionID).Order("c.ordinal")
	if d.Backend == "qd" {
		queryRows = queryRows.Select("c.id,c.parent_id,c.content,c.ordinal,v.embedding::text AS embedding").Joins("JOIN ai_chunk_vectors v ON v.child_id=c.id AND v.content_version_id=c.content_version_id AND v.embedding_set_id=?", in.EmbeddingSetID)
	}
	if err := queryRows.Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) != d.Count || aiprotocol.HashJSON(rows) != d.Hash {
		return errors.New("projection.incomplete_manifest")
	}
	// Serialize coverage checks with other publishers before examining their
	// committed completion rows, so concurrent last batches cannot strand a target.
	if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?,0))", "ai-target:"+d.Backend).Error; err != nil {
		return err
	}
	kind := "text"
	artifact := d.ContentID
	if d.Backend == "qd" {
		kind = "vector"
		artifact = d.SetID
	}
	if err := tx.Exec(`INSERT INTO ai_projection_targets(id,backend,projection_kind,target_version,index_epoch,config_digest,state,activated_at) VALUES(?,?,?,?,?,?,'building',NULL) ON CONFLICT(id) DO NOTHING`, d.TargetID, d.Backend, kind, d.TargetVersion, d.Epoch, d.ConfigDigest).Error; err != nil {
		return err
	}
	var set any
	if d.Backend == "qd" {
		set = d.SetID
	}
	err = tx.Exec(`INSERT INTO ai_projection_states(backend,target_id,object_id,content_generation,content_version_id,embedding_set_id,artifact_version_id,operation,source_revision,artifact_hash,expected_count,applied_count,state,published_at) VALUES(?,?,?,?,?,?,?,'upsert',?,?,?,?,'complete',clock_timestamp()) ON CONFLICT(target_id,artifact_version_id,operation) DO UPDATE SET applied_count=EXCLUDED.applied_count,state='complete',published_at=clock_timestamp() WHERE ai_projection_states.artifact_hash=EXCLUDED.artifact_hash`, d.Backend, d.TargetID, in.Intent.ObjectID, in.Intent.ContentGeneration, d.ContentID, set, artifact, d.Epoch, d.Hash, d.Count, d.Count).Error
	if err != nil {
		return err
	}
	var pending int64
	column := "active_content_version_id"
	if d.Backend == "qd" {
		column = "active_embedding_set_id"
	}
	query := "SELECT count(*) FROM ai_objects o WHERE o.tombstoned_at IS NULL AND o." + column + " IS NOT NULL AND NOT EXISTS(SELECT 1 FROM ai_projection_states p WHERE p.target_id=? AND p.artifact_version_id=o." + column + " AND p.operation='upsert' AND p.state='complete')"
	if err := tx.Raw(query, d.TargetID).Scan(&pending).Error; err != nil {
		return err
	}
	var obsolete bool
	if err := tx.Raw("SELECT EXISTS(SELECT 1 FROM ai_projection_targets WHERE backend=? AND index_epoch>?) OR EXISTS(SELECT 1 FROM ai_projection_targets WHERE id=? AND state='retired')", d.Backend, d.Epoch, d.TargetID).Scan(&obsolete).Error; err != nil {
		return err
	}
	if pending == 0 && !obsolete {
		if err := tx.Exec("UPDATE ai_projection_targets SET state='retired' WHERE backend=? AND state='active' AND id<>?", d.Backend, d.TargetID).Error; err != nil {
			return err
		}
		return tx.Exec("UPDATE ai_projection_targets SET state='active',activated_at=COALESCE(activated_at,clock_timestamp()) WHERE id=?", d.TargetID).Error
	}
	return nil
}

func projectionIdentity(backend string, in aiprotocol.Input) (uuid.UUID, string, string, error) {
	if in.TargetEpoch < 1 || (backend != "es" && backend != "qd") {
		return uuid.Nil, "", "", errors.New("projection.invalid_target")
	}
	digest := aiprotocol.HashJSON("text-schema-v1")
	if backend == "qd" {
		digest = aiprotocol.HashJSON([]any{in.Profile.EmbeddingProvider, in.Profile.EmbeddingModel, in.Profile.EmbeddingRevision, in.Profile.Dimension})
	}
	namespace := in.TargetNamespace
	if namespace == "" {
		namespace = "bx-ai"
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9_-]{1,62}$`).MatchString(namespace) {
		return uuid.Nil, "", "", errors.New("projection.invalid_namespace")
	}
	name := fmt.Sprintf("%s-%s-%d-%s", namespace, backend, in.TargetEpoch, digest[:12])
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(name)), name, digest, nil
}

type ranked struct {
	ID    uuid.UUID
	Score float64
}

func (p *Projector) external(ctx context.Context, backend string, versions []string, query string, vector []float32, limit int) ([]ranked, string, error) {
	var target struct {
		ID            uuid.UUID
		TargetVersion string
	}
	if err := p.DB.WithContext(ctx).Table("ai_published_targets").Where("backend=?", backend).Take(&target).Error; err != nil {
		return nil, "", errors.New("projection.incomplete")
	}
	var covered int64
	if err := p.DB.WithContext(ctx).Table("ai_published_projections").Where("target_id=? AND artifact_version_id::text=ANY(?::text[]) AND operation='upsert'", target.ID, pq.Array(versions)).Count(&covered).Error; err != nil {
		return nil, "", err
	}
	if covered != int64(len(versions)) {
		return nil, "", errors.New("projection.incomplete")
	}
	out := []ranked{}
	if backend == "es" {
		var resp struct {
			Hits struct {
				Hits []struct {
					Score  float64 `json:"_score"`
					Source struct {
						ChildID string `json:"child_id"`
					} `json:"_source"`
				} `json:"hits"`
			} `json:"hits"`
		}
		body := map[string]any{"size": limit, "query": map[string]any{"bool": map[string]any{"must": []any{map[string]any{"match": map[string]any{"content": query}}}, "filter": []any{map[string]any{"terms": map[string]any{"content_id": versions}}}}}}
		if err := p.request(ctx, "POST", strings.TrimRight(p.ES, "/")+"/"+target.TargetVersion+"/_search", body, backend, &resp); err != nil {
			return nil, "", err
		}
		for _, h := range resp.Hits.Hits {
			id, e := uuid.Parse(h.Source.ChildID)
			if e != nil {
				return nil, "", e
			}
			out = append(out, ranked{id, h.Score})
		}
	} else {
		var resp struct {
			Result struct {
				Points []struct {
					Score   float64 `json:"score"`
					Payload struct {
						ChildID string `json:"child_id"`
					} `json:"payload"`
				} `json:"points"`
			} `json:"result"`
		}
		body := map[string]any{"query": vector, "limit": limit, "with_payload": true, "filter": map[string]any{"must": []any{map[string]any{"key": "set_id", "match": map[string]any{"any": versions}}}}}
		if err := p.request(ctx, "POST", strings.TrimRight(p.QD, "/")+"/collections/"+target.TargetVersion+"/points/query", body, backend, &resp); err != nil {
			return nil, "", err
		}
		for _, h := range resp.Result.Points {
			id, e := uuid.Parse(h.Payload.ChildID)
			if e != nil {
				return nil, "", e
			}
			out = append(out, ranked{id, h.Score})
		}
	}
	return out, target.TargetVersion, nil
}

var _ = strconv.Itoa
var _ = time.Second
