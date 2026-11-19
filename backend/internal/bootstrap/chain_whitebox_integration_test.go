package bootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func runShareLifecycleRequestChecks(t *testing.T, fileID, shareID, shareKey, importedID, ownerToken, importerToken string, setToken, setShareToken func(string), request func(string, string, any, int) map[string]any) {
	t.Helper()
	if os.Getenv("TEST_CHAIN_WHITEBOX") != "true" {
		return
	}
	request("GET", "/api/v1/public/shares/content", nil, 200)
	request("DELETE", "/api/v1/shares/"+shareID, nil, 204)
	request("POST", "/api/v1/public/shares/resolve", map[string]any{"share_key": shareKey}, 404)
	request("GET", "/api/v1/public/shares/content", nil, 404)
	setToken(importerToken)
	request("GET", "/api/v1/files/"+importedID, nil, 200)
	setToken(ownerToken)
	created := request("POST", "/api/v1/shares", map[string]any{"file_id": fileID}, 201)
	id := created["share"].(map[string]any)["id"].(string)
	access := request("POST", "/api/v1/public/shares/resolve", map[string]any{"share_key": created["share_key"]}, 200)["share_access_token"].(string)
	setShareToken(access)
	expires := time.Now().Add(2 * time.Second).UTC()
	updated := request("PATCH", "/api/v1/shares/"+id, map[string]any{"version": 1, "expires_at": expires}, 200)
	if updated["version"] != float64(2) {
		t.Fatal("share optimistic version did not advance", updated)
	}
	request("PATCH", "/api/v1/shares/"+id, map[string]any{"version": 1, "expires_at": nil}, 409)
	request("GET", "/api/v1/public/shares/content", nil, 200)
	for {
		state := request("GET", "/api/v1/shares/"+id, nil, 200)["status"]
		if state == "expired" {
			break
		}
		if time.Now().After(expires.Add(3 * time.Second)) {
			t.Fatal("share expiry was not observable", state)
		}
		time.Sleep(50 * time.Millisecond)
	}
	request("GET", "/api/v1/public/shares/content", nil, 404)
	request("POST", "/api/v1/public/shares/resolve", map[string]any{"share_key": created["share_key"]}, 404)
	request("PATCH", "/api/v1/shares/"+id, map[string]any{"version": 2, "expires_at": nil}, 409)
	t.Log("share revoke invalidated existing public access; imported file stayed owned; expiry edit advanced version; stale edit=409; real 2s expiry closed existing token/resolve; expired share cannot reopen")
}

func runMultipartRequestChecks(t *testing.T, ctx context.Context, root, ownerToken string, setToken func(string), request func(string, string, any, int) map[string]any) {
	t.Helper()
	if os.Getenv("TEST_CHAIN_WHITEBOX") != "true" {
		return
	}
	func() {
		content := bytes.Repeat([]byte{0x17, 0x39, 0x91, 0xac}, (20*1024*1024+124)/4)
		copy(content, uuid.NewString())
		hash := sha256.Sum256(content)
		body := map[string]any{"folder_id": root, "filename": "three-parts.bin", "sha256": hex.EncodeToString(hash[:]), "size_bytes": len(content), "mime_type": "application/octet-stream"}
		created := request("POST", "/api/v1/uploads", body, 201)["upload"].(map[string]any)
		id := created["id"].(string)
		wait := func(label string, fn func() bool) {
			t.Helper()
			for until := time.Now().Add(40 * time.Second); time.Now().Before(until) && ctx.Err() == nil; {
				if fn() {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Fatal("multipart request timeout", label)
		}
		var session map[string]any
		wait("initialize", func() bool {
			session = request("GET", "/api/v1/uploads/"+id, nil, 200)
			return session["status"] == "created"
		})
		if session["part_count"] != float64(3) {
			t.Fatal("expected three real parts", session["part_count"])
		}
		partSize := int(session["part_size"].(float64))
		var first map[string]any
		for _, n := range []int{3, 1, 2} {
			signed := request("POST", "/api/v1/uploads/"+id+"/parts/presign", map[string]any{"part_numbers": []int{n}}, 200)["parts"].([]any)[0].(map[string]any)["url"].(string)
			part := content[(n-1)*partSize : min(n*partSize, len(content))]
			req, _ := http.NewRequestWithContext(ctx, "PUT", signed, bytes.NewReader(part))
			res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatal("real part upload", n, res.StatusCode)
			}
			ack := map[string]any{"part_number": n, "etag": strings.Trim(res.Header.Get("ETag"), "\""), "size_bytes": len(part)}
			request("POST", "/api/v1/uploads/"+id+"/parts/confirm?view=delta", map[string]any{"parts": []any{ack}}, 200)
			if n == 3 {
				request("POST", "/api/v1/uploads/"+id+"/complete", nil, 409)
			}
			if n == 1 {
				first = ack
				for range 3 {
					request("POST", "/api/v1/uploads/"+id+"/parts/confirm?view=delta", map[string]any{"parts": []any{ack}}, 200)
				}
				recovered := request("GET", "/api/v1/uploads/"+id, nil, 200)["confirmed_parts"].([]any)
				if len(recovered) != 2 || recovered[0].(map[string]any)["part_number"] != float64(1) || recovered[1].(map[string]any)["part_number"] != float64(3) {
					t.Fatal("resume snapshot lost acknowledged parts", recovered)
				}
			}
		}
		conflicting := map[string]any{"part_number": 1, "etag": "conflicting-etag", "size_bytes": first["size_bytes"]}
		request("POST", "/api/v1/uploads/"+id+"/parts/confirm", map[string]any{"parts": []any{conflicting}}, 409)
		request("POST", "/api/v1/uploads/"+id+"/complete", nil, 202)
		request("POST", "/api/v1/uploads/"+id+"/complete", nil, 202)
		wait("completion", func() bool {
			session = request("GET", "/api/v1/uploads/"+id, nil, 200)
			return session["status"] == "completed"
		})
		file := session["completed_entry_id"].(string)
		signed := request("GET", "/api/v1/files/"+file+"/download-url", nil, 200)["url"].(string)
		req, _ := http.NewRequestWithContext(ctx, "GET", signed, nil)
		res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		downloaded, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil || res.StatusCode != 200 || sha256.Sum256(downloaded) != hash {
			t.Fatal("three-part merged download differs", err)
		}
		body["filename"] = "instant-three-parts.bin"
		// One active object per owner is intentional. A different owner must
		// upload bytes to prove possession before canonical deduplication.
		request("POST", "/api/v1/uploads", body, 409)
		other := request("POST", "/api/v1/auth/register", map[string]any{"username": "multipart_" + uuid.NewString()[:8], "password": "multipart-test-password"}, 201)
		otherToken := other["access_token"].(string)
		setToken(otherToken)
		defer setToken(ownerToken)
		body["folder_id"] = request("GET", "/api/v1/folders/root", nil, 200)["id"]
		otherCreate := request("POST", "/api/v1/uploads", body, 201)
		if otherCreate["mode"] != "multipart" {
			t.Fatal("cross-owner hash claim bypassed possession proof", otherCreate["mode"])
		}
		otherID := otherCreate["upload"].(map[string]any)["id"].(string)
		var otherDone map[string]any
		wait("other owner initialization", func() bool {
			return request("GET", "/api/v1/uploads/"+otherID, nil, 200)["status"] == "created"
		})
		for n := 1; n <= 3; n++ {
			signed := request("POST", "/api/v1/uploads/"+otherID+"/parts/presign", map[string]any{"part_numbers": []int{n}}, 200)["parts"].([]any)[0].(map[string]any)["url"].(string)
			part := content[(n-1)*partSize : min(n*partSize, len(content))]
			req, _ := http.NewRequestWithContext(ctx, "PUT", signed, bytes.NewReader(part))
			res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatal("other owner actual upload", res.StatusCode)
			}
			request("POST", "/api/v1/uploads/"+otherID+"/parts/confirm", map[string]any{"parts": []any{map[string]any{"part_number": n, "etag": strings.Trim(res.Header.Get("ETag"), "\""), "size_bytes": len(part)}}}, 200)
		}
		request("POST", "/api/v1/uploads/"+otherID+"/complete", nil, 202)
		wait("other owner canonical deduplication", func() bool {
			otherDone = request("GET", "/api/v1/uploads/"+otherID, nil, 200)
			return otherDone["status"] == "completed"
		})
		if otherDone["completed_object_id"] != session["completed_object_id"] {
			t.Fatal("verified duplicate did not share canonical object")
		}
		quota := request("GET", "/api/v1/users/me/quota", nil, 200)
		if quota["reserved_bytes"] != float64(0) || quota["used_logical_bytes"] != float64(len(content)) {
			t.Fatal("multipart/instant quota", quota)
		}
		setToken(ownerToken)
		quota = request("GET", "/api/v1/users/me/quota", nil, 200)
		if quota["reserved_bytes"] != float64(0) || quota["used_logical_bytes"] != float64(len(content)) {
			t.Fatal("original multipart owner quota", quota)
		}
		request("DELETE", "/api/v1/files/"+file, nil, 204)
		setToken(otherToken)
		otherFile := otherDone["completed_entry_id"].(string)
		request("GET", "/api/v1/files/"+otherFile+"/download-url", nil, 200)
		request("DELETE", "/api/v1/files/"+otherFile, nil, 204)
		t.Logf("three real parts in order 3,1,2; incomplete complete=409; repeated ACK=200; conflicting ACK=409; complete replay=202; SHA256 download matched; same-owner duplicate=409; other owner required multipart and reused canonical object after verification; logical quota per owner=%d", len(content))
	}()
}

// Observe the databases and real external indexes before the HTTP fixture tears
// them down. The flag keeps this audit separate from its black-box request pass.
func runChainWhiteboxChecks(t *testing.T, ctx context.Context, business, control, aidb *gorm.DB, cfg config.Config) {
	t.Helper()
	if os.Getenv("TEST_CHAIN_WHITEBOX") != "true" {
		return
	}
	t.Run("whitebox-domain-and-projection-invariants", func(t *testing.T) {
		checks := []struct {
			name, query string
			db          *gorm.DB
		}{
			{"reference_count", `SELECT count(*) FROM file_objects o WHERE o.reference_count<>(SELECT count(*) FROM file_entries e WHERE e.object_id=o.id AND e.deleted_at IS NULL)`, business},
			{"occupied_quota", `SELECT count(*) FROM user_quota_usage q WHERE q.occupied_bytes<>COALESCE((SELECT sum(o.size_bytes) FROM file_entries e JOIN file_objects o ON o.id=e.object_id WHERE e.owner_id=q.owner_id AND e.deleted_at IS NULL),0)+COALESCE((SELECT sum(s.reserved_bytes) FROM upload_sessions s WHERE s.user_id=q.owner_id),0)`, business},
			{"terminal_reservations", `SELECT count(*) FROM upload_sessions WHERE status IN ('completed','failed','aborted','expired') AND reserved_bytes<>0`, business},
			{"live_files_on_deleted_objects", `SELECT count(*) FROM file_entries e JOIN file_objects o ON o.id=e.object_id WHERE e.deleted_at IS NULL AND o.status<>'ready'`, business},
			{"completed_session_identity", `SELECT count(*) FROM upload_sessions s LEFT JOIN file_entries e ON e.id=s.completed_entry_id WHERE s.status='completed' AND (e.id IS NULL OR e.object_id IS DISTINCT FROM s.completed_object_id OR e.owner_id<>s.user_id)`, business},
		}
		if aidb != nil {
			checks = append(checks, []struct {
				name, query string
				db          *gorm.DB
			}{
				{"active_vector_version", `SELECT count(*) FROM ai_objects o JOIN ai_embedding_sets s ON s.id=o.active_embedding_set_id WHERE s.content_version_id IS DISTINCT FROM o.active_content_version_id OR s.object_id<>o.object_id OR s.content_generation<>o.content_generation`, aidb},
				{"active_text_source", `SELECT count(*) FROM ai_objects o JOIN ai_content_versions c ON c.id=o.active_content_version_id WHERE c.source_id IS DISTINCT FROM o.active_source_id`, aidb},
				{"active_summary_source", `SELECT count(*) FROM ai_objects o JOIN ai_summaries s ON s.id=o.active_summary_id WHERE s.source_id IS DISTINCT FROM o.active_source_id`, aidb},
				{"chunk_counts", `SELECT count(*) FROM ai_content_versions c WHERE c.child_count<>(SELECT count(*) FROM ai_child_chunks x WHERE x.content_version_id=c.id) OR c.parent_count<>(SELECT count(*) FROM ai_parent_chunks p WHERE p.content_version_id=c.id)`, aidb},
				{"vector_counts", `SELECT count(*) FROM ai_embedding_sets s WHERE s.vector_count<>(SELECT count(*) FROM ai_chunk_vectors v WHERE v.embedding_set_id=s.id)`, aidb},
				{"projection_completion_counts", `SELECT count(*) FROM ai_projection_states WHERE state='complete' AND expected_count<>applied_count`, aidb},
			}...)
		}
		observed := map[string]int64{}
		for _, check := range checks {
			var count int64
			if err := check.db.WithContext(ctx).Raw(check.query).Scan(&count).Error; err != nil {
				t.Fatalf("%s SQL: %v", check.name, err)
			}
			observed[check.name] = count
			if count != 0 {
				t.Errorf("%s: %d inconsistent rows", check.name, count)
			}
		}
		if cfg.Async.ProtocolVersion == 3 {
			var legacy int64
			if err := control.Raw(`SELECT (SELECT count(*) FROM results)+(SELECT count(*) FROM executions)+(SELECT count(*) FROM task_deliveries)+(SELECT count(*) FROM task_continuations)`).Scan(&legacy).Error; err != nil {
				t.Fatal(err)
			}
			observed["v3_legacy_protocol_rows"] = legacy
			if legacy != 0 {
				t.Error("v3 wrote legacy execution tables", legacy)
			}
			if aidb != nil {
				var artifacts int64
				if err := aidb.Table("ai_artifact_manifests").Count(&artifacts).Error; err != nil || artifacts != 0 {
					t.Fatal("v3 generic AI artifacts", artifacts, err)
				}
				var done []string
				if err := control.Table("tasks").Select("id::text").Where("state='done' AND effect_target='ai'").Scan(&done).Error; err != nil {
					t.Fatal(err)
				}
				var receipts []struct {
					TaskID string
					Count  int64
				}
				if err := aidb.Table("ai_commit_receipts").Select("task_id::text,count(*)").Group("task_id").Find(&receipts).Error; err != nil {
					t.Fatal(err)
				}
				byTask := map[string]int64{}
				for _, receipt := range receipts {
					byTask[receipt.TaskID] = receipt.Count
				}
				for _, id := range done {
					if byTask[id] != 1 {
						t.Errorf("completed AI task %s has %d receipts", id, byTask[id])
					}
				}
				observed["completed_ai_receipts_checked"] = int64(len(done))
			}
		}
		raw, _ := json.Marshal(observed)
		t.Logf("WHITEBOX domain %s", raw)
		if aidb != nil {
			checkLiveProjectionRows(t, ctx, aidb)
		}
	})
}

func checkLiveProjectionRows(t *testing.T, ctx context.Context, db *gorm.DB) {
	t.Helper()
	// Publication can precede external projection. Wait for the actual active
	// versions, rather than assuming that an older complete row covers them.
	deadline := time.Now().Add(45 * time.Second)
	for {
		var missing int64
		err := db.WithContext(ctx).Raw(`SELECT count(*) FROM ai_objects o WHERE o.tombstoned_at IS NULL AND
 (o.active_content_version_id IS NULL OR o.active_embedding_set_id IS NULL OR
 NOT EXISTS (SELECT 1 FROM ai_projection_states p JOIN ai_projection_targets t ON t.id=p.target_id WHERE t.state='active' AND p.backend='es' AND p.artifact_version_id=o.active_content_version_id AND p.operation='upsert' AND p.state='complete') OR
 NOT EXISTS (SELECT 1 FROM ai_projection_states p JOIN ai_projection_targets t ON t.id=p.target_id WHERE t.state='active' AND p.backend='qd' AND p.artifact_version_id=o.active_embedding_set_id AND p.operation='upsert' AND p.state='complete'))`).Scan(&missing).Error
		if err != nil {
			t.Fatal(err)
		}
		if missing == 0 {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatal("active versions lack complete external coverage", missing)
		}
		time.Sleep(100 * time.Millisecond)
	}
	var rows []struct{ ID, ParentID, ContentVersionID, Content, SetID, Embedding string }
	if err := db.WithContext(ctx).Raw(`SELECT c.id,c.parent_id,c.content_version_id,c.content,o.active_embedding_set_id AS set_id,v.embedding::text FROM ai_objects o JOIN ai_child_chunks c ON c.content_version_id=o.active_content_version_id JOIN ai_chunk_vectors v ON v.child_id=c.id AND v.embedding_set_id=o.active_embedding_set_id WHERE o.tombstoned_at IS NULL`).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("empty projection comparison cannot prove coverage")
	}
	// Retired physical versions may remain during the cleanup grace period.
	// They must still identify retained normalized rows, rather than unknown data.
	var retainedText []struct{ ID, ContentVersionID, ParentID, Content string }
	if err := db.Table("ai_child_chunks").Select("id,content_version_id,parent_id,content").Find(&retainedText).Error; err != nil {
		t.Fatal(err)
	}
	textByID := map[string]struct{ ParentID, Content string }{}
	for _, row := range retainedText {
		textByID[row.ContentVersionID+":"+row.ID] = struct{ ParentID, Content string }{row.ParentID, row.Content}
	}
	var retainedVectors []struct{ EmbeddingSetID, ChildID, ContentVersionID string }
	if err := db.Table("ai_chunk_vectors").Select("embedding_set_id,child_id,content_version_id").Find(&retainedVectors).Error; err != nil {
		t.Fatal(err)
	}
	vectorByID := map[string]string{}
	for _, row := range retainedVectors {
		vectorByID[row.EmbeddingSetID+":"+row.ChildID] = row.ContentVersionID
	}
	var targets []struct{ Backend, TargetVersion string }
	if err := db.Table("ai_projection_targets").Where("state='active'").Find(&targets).Error; err != nil || len(targets) != 2 {
		t.Fatal("expected two active external targets", targets, err)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	call := func(endpoint, path string, body any, out any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(endpoint, "/")+path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatal("projection audit HTTP", res.StatusCode, path)
		}
		if err := json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range targets {
		if target.Backend == "es" {
			// Explicit tags preserve the external snake_case contract.
			var raw struct {
				Hits struct {
					Hits []struct {
						Source map[string]string `json:"_source"`
					}
				}
			}
			call(os.Getenv("TEST_SEARCH_ES_ENDPOINT"), "/"+target.TargetVersion+"/_search", map[string]any{"size": 10000, "query": map[string]any{"match_all": map[string]any{}}}, &raw)
			documents := map[string]map[string]string{}
			for _, hit := range raw.Hits.Hits {
				key := hit.Source["content_id"] + ":" + hit.Source["child_id"]
				expected, known := textByID[key]
				if !known || expected.Content != hit.Source["content"] || expected.ParentID != hit.Source["parent_id"] {
					t.Error("ES physical row has no matching retained PG content", key)
				}
				if _, duplicate := documents[key]; duplicate {
					t.Error("duplicate ES child", key)
				}
				documents[key] = hit.Source
			}
			for _, row := range rows {
				doc, ok := documents[row.ContentVersionID+":"+row.ID]
				if !ok || doc["content"] != row.Content || doc["parent_id"] != row.ParentID {
					t.Errorf("ES content/identity mismatch for child %s", row.ID)
				}
			}
			t.Logf("WHITEBOX elasticsearch active_children=%d physical_documents=%d target=%s", len(rows), len(documents), target.TargetVersion)
			continue
		}
		type point struct {
			Payload map[string]string
			Vector  []float64
		}
		points := map[string]point{}
		var offset any
		for {
			var res struct {
				Result struct {
					Points []point
					Next   any `json:"next_page_offset"`
				}
			}
			body := map[string]any{"limit": 256, "with_payload": true, "with_vector": true}
			if offset != nil {
				body["offset"] = offset
			}
			call(os.Getenv("TEST_SEARCH_QDRANT_ENDPOINT"), "/collections/"+target.TargetVersion+"/points/scroll", body, &res)
			for _, p := range res.Result.Points {
				key := p.Payload["set_id"] + ":" + p.Payload["child_id"]
				if expected, known := vectorByID[key]; !known || expected != p.Payload["content_id"] {
					t.Error("QD physical row has no matching retained PG vector", key)
				}
				if _, exists := points[key]; exists {
					t.Error("duplicate Qdrant child", key)
				}
				points[key] = p
			}
			offset = res.Result.Next
			if offset == nil {
				break
			}
		}
		for _, row := range rows {
			p, ok := points[row.SetID+":"+row.ID]
			var expected []float64
			if err := json.Unmarshal([]byte(row.Embedding), &expected); err != nil {
				t.Fatal(err)
			}
			if !ok || p.Payload["content_id"] != row.ContentVersionID || len(p.Vector) != 1024 || len(expected) != 1024 {
				t.Errorf("QD version/dimension mismatch for child %s", row.ID)
				continue
			}
			var dot, a, b float64
			for i := range expected {
				dot += expected[i] * p.Vector[i]
				a += expected[i] * expected[i]
				b += p.Vector[i] * p.Vector[i]
			}
			cosine := dot / math.Sqrt(a*b)
			if math.IsNaN(cosine) || cosine < 0.999999 {
				t.Errorf("QD vector mismatch child=%s cosine=%g", row.ID, cosine)
			}
		}
		t.Logf("WHITEBOX qdrant active_vectors=%d physical_points=%d dimension=1024 target=%s", len(rows), len(points), target.TargetVersion)
	}
}
