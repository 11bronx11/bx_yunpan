package upload

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/httpapi"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/storageurl"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
)

// These tests use the browser's JSON contract and a real, isolated business PG.
// Authentication is injected; neither browser scheduling nor S3 PUTs run here.
type sessionHTTPFixture struct {
	concurrencyFixture
	router *gin.Engine
}

func newSessionHTTPFixture(t *testing.T) sessionHTTPFixture {
	t.Helper()
	f := newConcurrencyFixture(t)
	presigner, err := storageurl.New("storage.example.test", "test-access", "test-secret", "us-east-1", false, "/storage")
	if err != nil {
		t.Fatal(err)
	}
	f.service.presigner = presigner
	r := gin.New()
	NewHTTP(f.service, func(c *gin.Context) {
		owner := f.userID
		if value := c.GetHeader("X-Test-Owner"); value != "" {
			owner = uuid.MustParse(value)
		}
		httpapi.SetPrincipalID(c, owner)
	}).RegisterRoutes(r.Group("/api/v1"))
	return sessionHTTPFixture{concurrencyFixture: f, router: r}
}

func (f sessionHTTPFixture) request(method, path string, body any, key string) *httptest.ResponseRecorder {
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			panic(err)
		}
	}
	r := httptest.NewRequest(method, "/api/v1/uploads"+path, bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, r)
	return w
}

func requireHTTP(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP %d, want %d: %s", w.Code, status, w.Body.String())
	}
}

func decodeSession(t *testing.T, w *httptest.ResponseRecorder) Session {
	t.Helper()
	// The API uses id, while GORM's domain structs deliberately have no JSON tags.
	var body struct {
		ID        uuid.UUID `json:"id"`
		UploadID  uuid.UUID `json:"upload_id"`
		Status    string    `json:"status"`
		Confirmed []struct {
			Number int   `json:"part_number"`
			Size   int64 `json:"size_bytes"`
		} `json:"confirmed_parts"`
		Acknowledged []struct {
			Number int   `json:"part_number"`
			Size   int64 `json:"size_bytes"`
		} `json:"acknowledged_parts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ID == uuid.Nil {
		body.ID = body.UploadID
	}
	s := Session{ID: body.ID, Status: body.Status}
	for _, p := range append(body.Confirmed, body.Acknowledged...) {
		s.ConfirmedParts = append(s.ConfirmedParts, Part{PartNumber: p.Number, SizeBytes: p.Size})
	}
	return s
}

func (f sessionHTTPFixture) create(t *testing.T, name string, size int64, key string) Session {
	t.Helper()
	w := f.request(http.MethodPost, "", map[string]any{
		"folder_id": f.rootID, "filename": name, "sha256": strings.Repeat("a", 64),
		"size_bytes": size, "mime_type": "application/octet-stream",
	}, key)
	requireHTTP(t, w, http.StatusCreated)
	var body struct {
		Mode   string          `json:"mode"`
		Upload json.RawMessage `json:"upload"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Mode != "multipart" {
		t.Fatalf("multipart create: %s %v", w.Body.String(), err)
	}
	response := httptest.NewRecorder()
	response.Body.Write(body.Upload)
	s := decodeSession(t, response)
	if s.Status != StatusInitializing {
		t.Fatalf("create status: %s", s.Status)
	}
	return f.stored(t, s.ID)
}

func (f sessionHTTPFixture) stored(t *testing.T, id uuid.UUID) Session {
	t.Helper()
	s, err := f.service.Get(f.userID, id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f sessionHTTPFixture) apply(kind string, in asyncprotocol.Input, out asyncprotocol.Output) (asyncprotocol.Decision, error) {
	var decision asyncprotocol.Decision
	err := f.db.Transaction(func(tx *gorm.DB) error {
		var err error
		decision, err = f.service.CommitAsync(tx, kind, in, out)
		return err
	})
	return decision, err
}

func (f sessionHTTPFixture) ready(t *testing.T, s Session) Session {
	t.Helper()
	if _, err := f.apply(storageops.MultipartCreate, asyncprotocol.Input{SessionID: s.ID, OwnerID: f.userID}, asyncprotocol.Output{UploadID: "s3-" + s.ID.String()}); err != nil {
		t.Fatal(err)
	}
	return f.stored(t, s.ID)
}

func partJSON(number int, size int64, etag string) map[string]any {
	return map[string]any{"part_number": number, "etag": etag, "size_bytes": size}
}

func (f sessionHTTPFixture) confirm(id uuid.UUID, parts ...map[string]any) *httptest.ResponseRecorder {
	return f.request(http.MethodPost, "/"+id.String()+"/parts/confirm?view=delta", map[string]any{"parts": parts}, "")
}

func (f sessionHTTPFixture) operationCount(t *testing.T, id uuid.UUID, kind string) int64 {
	t.Helper()
	var count int64
	if err := f.db.Model(&storageops.Operation{}).Where("upload_session_id=? AND operation_type=?", id, kind).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

func (f sessionHTTPFixture) occupied(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := f.db.Table("user_quota_usage").Where("owner_id=?", f.userID).Pluck("occupied_bytes", &n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSessionHTTPPauseResumeLostResponsesAndCompletion(t *testing.T) {
	f := newSessionHTTPFixture(t)
	const size = 20*1024*1024 + 123
	key := uuid.NewString()
	s := f.create(t, "resume.bin", size, key)
	// Lose the create response and recover with the identical browser request.
	again := f.create(t, "resume.bin", size, key)
	if again.ID != s.ID || f.occupied(t) != size || f.operationCount(t, s.ID, storageops.MultipartCreate) != 1 {
		t.Fatal("create replay duplicated a session, intent or reservation")
	}
	s = f.ready(t, s)
	for _, n := range []int{3, 1} {
		requireHTTP(t, f.confirm(s.ID, partJSON(n, expectedPartSize(s, n), fmt.Sprint(n))), http.StatusOK)
	}
	first := f.stored(t, s.ID)
	// A pause is a drained client window: no pause mutation exists in the API.
	w := f.request(http.MethodGet, "/"+s.ID.String(), nil, "")
	requireHTTP(t, w, http.StatusOK)
	recovered := decodeSession(t, w)
	if recovered.Status != StatusUploading || len(recovered.ConfirmedParts) != 2 || recovered.ConfirmedParts[0].PartNumber != 1 || recovered.ConfirmedParts[1].PartNumber != 3 {
		t.Fatalf("resume authority: %+v", recovered)
	}
	requireHTTP(t, f.request(http.MethodPost, "/"+s.ID.String()+"/complete", nil, ""), http.StatusConflict)
	// ACK was lost four times: reuse the original ETag, never overwrite it.
	for range 4 {
		requireHTTP(t, f.confirm(s.ID, partJSON(1, s.PartSize, "1")), http.StatusOK)
	}
	if got := f.stored(t, s.ID); !got.ConfirmedParts[0].CompletedAt.Equal(first.ConfirmedParts[0].CompletedAt) || f.occupied(t) != size {
		t.Fatal("ACK retry changed acknowledged facts or quota")
	}
	requireHTTP(t, f.confirm(s.ID, partJSON(2, s.PartSize, "2")), http.StatusOK)
	// Lose complete's response and recover through GET, then replay complete.
	requireHTTP(t, f.request(http.MethodPost, "/"+s.ID.String()+"/complete", nil, ""), http.StatusAccepted)
	w = f.request(http.MethodGet, "/"+s.ID.String(), nil, "")
	if decodeSession(t, w).Status != StatusCompleting {
		t.Fatal("complete was not durable before its response")
	}
	requireHTTP(t, f.request(http.MethodPost, "/"+s.ID.String()+"/complete", nil, ""), http.StatusAccepted)
	if f.operationCount(t, s.ID, storageops.MultipartComplete) != 1 {
		t.Fatal("complete replay duplicated merge intent")
	}
	requireHTTP(t, f.request(http.MethodDelete, "/"+s.ID.String(), nil, ""), http.StatusConflict)
	requireHTTP(t, f.confirm(s.ID, partJSON(1, s.PartSize, "1")), http.StatusOK)
	requireHTTP(t, f.confirm(s.ID, partJSON(1, s.PartSize, "changed")), http.StatusConflict)
	if calls, complete := f.storage.calls(); calls != 0 || complete != 0 || f.storage.abortCount() != 0 {
		t.Fatal("business request called S3")
	}
}

func TestSessionHTTPBatchValidationAndLimits(t *testing.T) {
	f := newSessionHTTPFixture(t)
	s := f.ready(t, f.create(t, "limits.bin", 101*10*1024*1024, uuid.NewString()))
	path := "/" + s.ID.String()
	for _, count := range []int{20, 21} {
		numbers := make([]int, count)
		for i := range numbers {
			numbers[i] = i + 1
		}
		w := f.request(http.MethodPost, path+"/parts/presign", map[string]any{"part_numbers": numbers}, "")
		want := http.StatusOK
		if count == 21 {
			want = http.StatusUnprocessableEntity
		}
		requireHTTP(t, w, want)
		if count == 20 {
			var body struct {
				Parts []struct {
					Number int    `json:"part_number"`
					URL    string `json:"url"`
				} `json:"parts"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Parts) != 20 {
				t.Fatalf("signatures: %s %v", w.Body.String(), err)
			}
			for _, p := range body.Parts {
				u, err := url.Parse(p.URL)
				if err != nil || u.Query().Get("uploadId") != s.StorageUploadID || u.Query().Get("partNumber") != fmt.Sprint(p.Number) || !strings.HasPrefix(u.Path, "/storage/") {
					t.Fatalf("wrong signature: %v", p)
				}
			}
		}
	}
	for _, numbers := range [][]int{nil, {1, 1}, {0}, {102}} {
		requireHTTP(t, f.request(http.MethodPost, path+"/parts/presign", map[string]any{"part_numbers": numbers}, ""), http.StatusUnprocessableEntity)
	}
	for _, tc := range []struct {
		name  string
		parts []map[string]any
	}{
		{"empty", nil},
		{"wrong_tail", []map[string]any{partJSON(1, s.PartSize, "one"), partJSON(101, s.PartSize-1, "tail")}},
		{"zero_number", []map[string]any{partJSON(0, s.PartSize, "zero")}},
		{"out_of_range", []map[string]any{partJSON(102, s.PartSize, "too-far")}},
		{"zero_size", []map[string]any{partJSON(1, 0, "one")}},
		{"negative_size", []map[string]any{partJSON(1, -1, "one")}},
		{"empty_etag", []map[string]any{partJSON(1, s.PartSize, " \"\" ")}},
		{"large_etag", []map[string]any{partJSON(1, s.PartSize, strings.Repeat("a", 513))}},
		{"duplicate_conflict", []map[string]any{partJSON(1, s.PartSize, "one"), partJSON(1, s.PartSize, "other")}},
		{"large_checksum", []map[string]any{{"part_number": 1, "size_bytes": s.PartSize, "etag": "one", "checksum": strings.Repeat("x", 513)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireHTTP(t, f.confirm(s.ID, tc.parts...), http.StatusUnprocessableEntity)
			if got := f.stored(t, s.ID); got.Status != StatusCreated || len(got.ConfirmedParts) != 0 {
				t.Fatal("invalid batch partially committed")
			}
		})
	}
	parts := make([]map[string]any, 101)
	for i := range parts {
		parts[i] = partJSON(i+1, s.PartSize, fmt.Sprint(i+1))
	}
	requireHTTP(t, f.confirm(s.ID, parts...), http.StatusUnprocessableEntity)
	requireHTTP(t, f.confirm(s.ID, parts[:100]...), http.StatusOK)
	w := f.confirm(s.ID, parts[100], parts[100])
	requireHTTP(t, w, http.StatusOK)
	if len(decodeSession(t, w).ConfirmedParts) != 1 || bytes.Contains(w.Body.Bytes(), []byte("confirmed_parts")) {
		t.Fatal("delta included historical parts or duplicate ACKs")
	}
	w = f.request(http.MethodPost, path+"/parts/confirm", map[string]any{"parts": parts[99:]}, "")
	requireHTTP(t, w, http.StatusOK)
	if len(decodeSession(t, w).ConfirmedParts) != 101 {
		t.Fatal("legacy full response lost confirmed parts")
	}
	if f.operationCount(t, s.ID, storageops.MultipartComplete) != 0 {
		t.Fatal("ACK implicitly completed upload")
	}
}

func TestSessionHTTPConcurrentOverlappingAndConflictingBatches(t *testing.T) {
	f := newSessionHTTPFixture(t)
	s := f.ready(t, f.create(t, "overlap.bin", 30*1024*1024, uuid.NewString()))
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 12)
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			n := i%2 + 1
			results <- f.confirm(s.ID, partJSON(n, s.PartSize, fmt.Sprint(n)), partJSON(3, s.PartSize, "3"))
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	for w := range results {
		requireHTTP(t, w, http.StatusOK)
	}
	if len(f.stored(t, s.ID).ConfirmedParts) != 3 {
		t.Fatal("overlapping requests lost or duplicated parts")
	}
	// A batch with one conflicting existing part must not insert another part.
	s2 := f.ready(t, f.create(t, "conflict.bin", 20*1024*1024, uuid.NewString()))
	start = make(chan struct{})
	results = make(chan *httptest.ResponseRecorder, 2)
	for _, etag := range []string{"device-a", "device-b"} {
		wg.Add(1)
		go func(etag string) {
			defer wg.Done()
			<-start
			results <- f.confirm(s2.ID, partJSON(1, s2.PartSize, etag))
		}(etag)
	}
	close(start)
	wg.Wait()
	close(results)
	statuses := map[int]int{}
	for w := range results {
		statuses[w.Code]++
	}
	if statuses[http.StatusOK] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("competing devices: %v", statuses)
	}
	stored := f.stored(t, s2.ID)
	requireHTTP(t, f.confirm(s2.ID, partJSON(2, s2.PartSize, "two"), partJSON(1, s2.PartSize, "different")), http.StatusConflict)
	if got := f.stored(t, s2.ID); len(got.ConfirmedParts) != 1 || !got.ConfirmedParts[0].CompletedAt.Equal(stored.ConfirmedParts[0].CompletedAt) {
		t.Fatal("conflicting batch changed facts")
	}
}

func TestSessionHTTPOwnerIsolation(t *testing.T) {
	f := newSessionHTTPFixture(t)
	s := f.ready(t, f.create(t, "private.bin", 128, uuid.NewString()))
	for _, tc := range []struct{ method, suffix, body string }{
		{http.MethodGet, "", ""}, {http.MethodDelete, "", ""}, {http.MethodPost, "/complete", ""},
		{http.MethodPost, "/parts/presign", `{"part_numbers":[1]}`},
		{http.MethodPost, "/parts/confirm?view=delta", `{"parts":[{"part_number":1,"etag":"x","size_bytes":128}]}`},
	} {
		r := httptest.NewRequest(tc.method, "/api/v1/uploads/"+s.ID.String()+tc.suffix, strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Test-Owner", uuid.NewString())
		w := httptest.NewRecorder()
		f.router.ServeHTTP(w, r)
		requireHTTP(t, w, http.StatusNotFound)
	}
	if got := f.stored(t, s.ID); got.Status != StatusCreated || len(got.ConfirmedParts) != 0 || f.occupied(t) != 128 {
		t.Fatal("other owner mutated session")
	}
}

type uploadQueryTrace struct {
	logger.Interface
	queries []string
}

func (l *uploadQueryTrace) Trace(_ context.Context, _ time.Time, sql func() (string, int64), _ error) {
	query, _ := sql()
	l.queries = append(l.queries, query)
}

func TestSessionHTTPDeltaAndPresignAvoidHistoricalPartReads(t *testing.T) {
	f := newSessionHTTPFixture(t)
	s := f.ready(t, f.create(t, "bounded-query.bin", 100*10*1024*1024, uuid.NewString()))
	trace := &uploadQueryTrace{Interface: logger.Discard}
	f.service.db = f.db.Session(&gorm.Session{Logger: trace})
	parts := make([]map[string]any, 100)
	for i := range parts {
		parts[i] = partJSON(i+1, s.PartSize, fmt.Sprint(i+1))
	}
	requireHTTP(t, f.confirm(s.ID, parts...), http.StatusOK)
	inserts, reads := 0, 0
	for _, query := range trace.queries {
		if strings.HasPrefix(query, `INSERT INTO "upload_parts"`) {
			inserts++
		}
		if strings.HasPrefix(query, "SELECT") && strings.Contains(query, `"upload_parts"`) {
			reads++
			if !strings.Contains(query, "part_number IN") {
				t.Fatal("delta read all historical parts", query)
			}
		}
	}
	if inserts != 1 || reads != 1 {
		t.Fatalf("100-part batch used %d inserts and %d part reads", inserts, reads)
	}
	trace.queries = nil
	requireHTTP(t, f.request(http.MethodPost, "/"+s.ID.String()+"/parts/presign", map[string]any{"part_numbers": []int{1, 2, 3}}, ""), http.StatusOK)
	for _, query := range trace.queries {
		if strings.Contains(query, `"upload_parts"`) {
			t.Fatal("presign read historical ACKs", query)
		}
	}
}

func TestSessionHTTPTerminalSessionsRemainClosed(t *testing.T) {
	for _, state := range []string{StatusFailed, StatusAborted, StatusExpired} {
		t.Run(state, func(t *testing.T) {
			f := newSessionHTTPFixture(t)
			key := uuid.NewString()
			s := f.ready(t, f.create(t, "terminal.bin", 128, key))
			path := "/" + s.ID.String()
			switch state {
			case StatusAborted:
				requireHTTP(t, f.request(http.MethodDelete, path, nil, ""), http.StatusNoContent)
			case StatusFailed:
				if _, err := f.apply(storageops.MultipartCreate, asyncprotocol.Input{SessionID: s.ID, OwnerID: f.userID}, asyncprotocol.Output{Convergence: true, Error: "attempts exhausted"}); err != nil {
					t.Fatal(err)
				}
			case StatusExpired:
				if err := f.db.Model(&Session{}).Where("id=?", s.ID).Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.expireOne(context.Background(), s.ID); err != nil {
					t.Fatal(err)
				}
			}
			before := f.stored(t, s.ID)
			requireHTTP(t, f.confirm(s.ID, partJSON(1, 128, "late")), http.StatusConflict)
			requireHTTP(t, f.request(http.MethodPost, path+"/parts/presign", map[string]any{"part_numbers": []int{1}}, ""), http.StatusConflict)
			requireHTTP(t, f.request(http.MethodPost, path+"/complete", nil, ""), http.StatusConflict)
			requireHTTP(t, f.request(http.MethodDelete, path, nil, ""), http.StatusNoContent)
			w := f.request(http.MethodPost, "", map[string]any{"folder_id": f.rootID, "filename": "terminal.bin", "sha256": s.DeclaredSHA256, "size_bytes": 128, "mime_type": s.MimeType}, key)
			requireHTTP(t, w, http.StatusCreated)
			after := f.stored(t, s.ID)
			if after.Status != state || after.Version != before.Version || len(after.ConfirmedParts) != 0 || f.occupied(t) != 0 {
				t.Fatal("terminal session reopened or released quota twice")
			}
			w = f.request(http.MethodGet, "?status=active", nil, "")
			requireHTTP(t, w, http.StatusOK)
			if strings.Contains(w.Body.String(), s.ID.String()) {
				t.Fatal("terminal session listed as active")
			}
			// An explicit new key may start another session; it must not reset the old one.
			fresh := f.create(t, "terminal.bin", 128, uuid.NewString())
			if fresh.ID == s.ID || f.occupied(t) != 128 {
				t.Fatal("new attempt reused terminal identity or wrong quota")
			}
		})
	}
}
