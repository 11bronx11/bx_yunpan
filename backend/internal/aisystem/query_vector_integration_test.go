package aisystem

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pbv2"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"gorm.io/gorm"
)

// Extend a published fixture in bulk so EXPLAIN can exercise a realistic HNSW
// plan without thousands of unrelated task/publication round trips.
func addVectorIndexRows(t *testing.T, db *gorm.DB, in aiprotocol.Input, count int) {
	t.Helper()
	if err := db.Exec(`INSERT INTO ai_child_chunks
		(id,content_version_id,parent_id,ordinal,child_key,content,heading_path,location,
		 block_start,block_end,parent_start,parent_end,location_precision,token_count)
		SELECT md5(c.id::text || n::text)::uuid,c.content_version_id,c.parent_id,n,n::text,
		 c.content,c.heading_path,c.location,c.block_start,c.block_end,c.parent_start,c.parent_end,c.location_precision,c.token_count
		FROM ai_child_chunks c CROSS JOIN generate_series(1,?) n
		WHERE c.content_version_id=? AND c.ordinal=0`, count, in.ContentVersionID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO ai_chunk_vectors
		(embedding_set_id,child_id,content_version_id,embedding,input_text_hash)
		SELECT ?,id,content_version_id,
		 (ARRAY[ordinal::real / ?,sqrt(1-power(ordinal::real / ?,2))::real] || array_fill(0::real,ARRAY[1022]))::vector,
		 'index-fixture'
		FROM ai_child_chunks WHERE content_version_id=? AND ordinal>0`, in.EmbeddingSetID, count+1, count+1, in.ContentVersionID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`UPDATE ai_embedding_sets SET vector_count=? WHERE id=?`, count+1, in.EmbeddingSetID).Error; err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"ai_chunk_vectors", "ai_embedding_sets", "ai_objects"} {
		if err := db.Exec("ANALYZE " + table).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func TestPGVectorUsesHNSWWithPublishedViewPermissions(t *testing.T) {
	db, store, publisher, in, _ := readySmallText(t)
	in = publishRecallVectors(t, db, store, publisher, in, 1)
	addVectorIndexRows(t, db, in, 5000)

	// Production query accounts can read only published views. A fix that
	// accesses the underlying vector table would fail under this role.
	var schema string
	if err := db.Raw(`SELECT current_schema()`).Row().Scan(&schema); err != nil {
		t.Fatal(err)
	}
	role := "vector_query_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := db.Exec("CREATE ROLE " + role).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, statement := range []string{"RESET ROLE", "SET default_transaction_read_only=off", "DROP OWNED BY " + role, "DROP ROLE " + role} {
			if err := db.Exec(statement).Error; err != nil {
				t.Error(err)
			}
		}
	})
	for _, statement := range []string{
		"GRANT USAGE ON SCHEMA " + schema + " TO " + role,
		"GRANT SELECT ON ai_published_vectors TO " + role,
		"SET hnsw.iterative_scan=strict_order",
		"SET hnsw.ef_search=100",
		"SET ROLE " + role,
		"SET default_transaction_read_only=on",
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec(`SELECT embedding FROM ai_chunk_vectors LIMIT 1`).Error; err == nil {
		t.Fatal("query role can read the base table")
	}

	vector := make([]float32, 1024)
	vector[0] = 1
	literal, ids := vectorLiteral(vector), []string{in.EmbeddingSetID.String()}
	var plan string
	if err := db.Raw("EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+pgVectorNearestSQL, literal, pq.Array(ids), literal, 320, 80).Row().Scan(&plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, `"Index Name": "ai_vectors_cosine"`) {
		t.Fatal("published vector query did not use HNSW:", plan)
	}
	var stats []struct {
		ExecutionTime float64 `json:"Execution Time"`
	}
	if err := json.Unmarshal([]byte(plan), &stats); err != nil {
		t.Fatal(err)
	}
	t.Logf("5001 vectors: ai_vectors_cosine used without forcing planner settings; EXPLAIN execution %.3f ms", stats[0].ExecutionTime)
	for _, statement := range []string{"SET hnsw.iterative_scan=off", "SET hnsw.ef_search=40"} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}

	// More than the default ef_search=40 must survive the filtered scan.
	rows, err := (&Query{DB: db}).pgVector(context.Background(), ids, vector, 80)
	if err != nil || len(rows) != 80 || rows[0].Score < 0.999 {
		t.Fatalf("HNSW lost eligible results: count=%d err=%v", len(rows), err)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].Score > rows[i-1].Score {
			t.Fatal("vector results are not ordered by cosine similarity")
		}
	}
	if _, err := (&Query{DB: db}).pgVector(context.Background(), ids, []float32{1}, 80); err == nil {
		t.Fatal("invalid vector dimension did not roll back the request")
	}
	var iterative, ef string
	if err := db.Raw(`SELECT current_setting('hnsw.iterative_scan'),current_setting('hnsw.ef_search')`).Row().Scan(&iterative, &ef); err != nil {
		t.Fatal(err)
	}
	if iterative != "off" || ef != "40" {
		t.Fatal("request settings leaked into the connection pool", iterative, ef)
	}
}

func TestPGVectorShortScanPreservesScopeAndPublication(t *testing.T) {
	db, store, publisher, in, _ := readyRetrievalBlocks(t, []rag.Section{
		{Text: "authorized first", HeadingPath: "A"},
		{Text: "authorized second", HeadingPath: "B"},
		{Text: "authorized third", HeadingPath: "C"},
	})
	in = publishRecallVectors(t, db, store, publisher, in, 0.5)
	ids := []string{in.EmbeddingSetID.String()}
	for _, state := range []string{"outside", "retired", "tombstoned"} {
		_, other, _ := appendRetrievalBlocks(t, db, store, []rag.Section{{Text: state}})
		other = publishRecallVectors(t, db, store, publisher, other, 1)
		switch state {
		case "retired":
			if err := db.Exec(`UPDATE ai_embedding_sets SET publication_state='retired' WHERE id=?`, other.EmbeddingSetID).Error; err != nil {
				t.Fatal(err)
			}
			ids = append(ids, other.EmbeddingSetID.String())
		case "tombstoned":
			if err := db.Exec(`UPDATE ai_objects SET tombstoned_at=clock_timestamp() WHERE object_id=?`, other.Intent.ObjectID).Error; err != nil {
				t.Fatal(err)
			}
			ids = append(ids, other.EmbeddingSetID.String())
		}
	}
	vector := make([]float32, 1024)
	vector[0] = 1
	rows, err := (&Query{DB: db}).pgVector(context.Background(), ids, vector, 10)
	if err != nil || len(rows) != 3 {
		t.Fatalf("short scan lost eligible evidence or included unpublished/outside vectors: rows=%v err=%v", rows, err)
	}
	var allowed []uuid.UUID
	if err := db.Table("ai_published_children").Where("content_version_id=?", in.ContentVersionID).Pluck("id", &allowed).Error; err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		if !slices.Contains(allowed, row.ID) || row.Score > 0.51 || i > 0 && rows[i-1].ID.String() > row.ID.String() {
			t.Fatal("scope, similarity or deterministic tie order changed", rows)
		}
	}
}

func TestQdrantFailureFallsBackToPGVector(t *testing.T) {
	db, store, publisher, in, file := readySmallText(t)
	in = publishRecallVectors(t, db, store, publisher, in, 1)
	projector := projectionBackend(t, db)
	acceptProjection(t, projector, "ai.project.vector", in)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	projector.QD, projector.HTTP = server.URL, server.Client()
	q := &Query{DB: db, Core: &scopeClient{file: file}, Profile: in.Profile, Provider: &retrievalProbe{}, Projector: projector, ScopeLimit: 100}
	out, err := q.Search(context.Background(), &pbv2.SearchRequest{Query: "rollback", Mode: "semantic", Limit: 1})
	if err != nil || len(out.GetHits()) != 1 || calls != 1 || out.GetRetrieval().GetVectorBackend() != "postgres" || !out.GetRetrieval().GetDegraded() || !slices.Contains(out.GetRetrieval().GetReasons(), "vector.projection_unavailable") {
		t.Fatalf("QD failure did not return PG vector evidence: out=%v calls=%d err=%v", out, calls, err)
	}
}
