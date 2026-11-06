package sharing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/httpapi"
)

type shareFixture struct {
	db    *gorm.DB
	s     *Service
	owner uuid.UUID
	file  drive.FileEntry
}

func seedShare(t *testing.T, committed bool) shareFixture {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	t.Cleanup(func() { _ = pool.Close() })
	if !committed {
		db = db.Begin()
		t.Cleanup(func() { db.Rollback() })
	}
	owner, objectID := uuid.New(), uuid.New()
	if err := db.Exec("INSERT INTO users(id, username, username_normalized, password_hash) VALUES (?, ?, ?, 'test')", owner, owner.String(), owner.String()).Error; err != nil {
		t.Fatal(err)
	}
	d := drive.NewService(db)
	if err := d.ProvisionUser(db, owner); err != nil {
		t.Fatal(err)
	}
	root, err := d.Root(owner)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(objectID.String()))
	object := objectstore.Object{ID: objectID, SHA256: hex.EncodeToString(hash[:]), SizeBytes: 128, MimeType: "text/plain", Bucket: "test", ObjectKey: objectID.String(), Status: "ready", ReferenceCount: 1}
	if err := db.Create(&object).Error; err != nil {
		t.Fatal(err)
	}
	file, err := d.CreateFile(db, owner, root.ID, objectID, "分享测试.txt")
	if err != nil {
		t.Fatal(err)
	}
	s := NewService(db, d, objectstore.NewService(db), nil, config.Sharing{Secret: "share-management-test", AccessTTL: time.Minute})
	if committed {
		t.Cleanup(func() {
			db.Exec("DELETE FROM outbox_events WHERE aggregate_id IN (SELECT id FROM shares WHERE owner_id = ?)", owner)
			db.Exec("DELETE FROM shares WHERE owner_id = ?", owner)
			db.Exec("DELETE FROM users WHERE id = ?", owner)
			db.Exec("DELETE FROM file_objects WHERE id = ?", objectID)
		})
	}
	return shareFixture{db: db, s: s, owner: owner, file: file}
}

func TestShareManagementLifecycle(t *testing.T) {
	f := seedShare(t, false)
	ctx := context.Background()
	soon := time.Now().Add(20 * time.Second)
	share, key, err := f.s.Create(f.owner, f.file.ID, &soon)
	if err != nil {
		t.Fatal(err)
	}
	_, _, token, accessExpiry, err := f.s.Resolve(key)
	if err != nil || accessExpiry.After(soon) {
		t.Fatalf("expiry=%v err=%v", accessExpiry, err)
	}
	view, err := f.s.GetView(ctx, f.owner, share.ID)
	if err != nil || view.FileName != f.file.Name || view.SizeBytes != 128 || !view.FileAvailable {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	if _, err := f.s.GetView(ctx, uuid.New(), share.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner get: %v", err)
	}
	if err := f.s.UpdateExpiry(ctx, f.owner, share.ID, 1, nil); err != nil {
		t.Fatal(err)
	}
	view, _ = f.s.GetView(ctx, f.owner, share.ID)
	if view.Version != 2 || view.ExpiresAt != nil {
		t.Fatalf("updated: %+v", view)
	}
	if err := f.s.UpdateExpiry(ctx, f.owner, share.ID, 1, nil); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale update: %v", err)
	}
	past := time.Now().Add(-time.Minute)
	if err := f.s.UpdateExpiry(ctx, f.owner, share.ID, 2, &past); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("past expiry: %v", err)
	}
	if err := f.db.Model(&Share{}).Where("id = ?", share.ID).Update("expires_at", past).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.Access(token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old token bypasses expiry: %v", err)
	}
	if _, _, _, _, err := f.s.Resolve(key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired resolve: %v", err)
	}
	if err := f.s.UpdateExpiry(ctx, f.owner, share.ID, 2, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Revoke(f.owner, share.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Revoke(f.owner, share.ID); err != nil {
		t.Fatalf("repeat revoke: %v", err)
	}
	if err := f.s.Revoke(uuid.New(), share.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign revoke: %v", err)
	}
	if _, _, err := f.s.Access(token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old token bypasses revocation: %v", err)
	}
	if err := f.s.UpdateExpiry(ctx, f.owner, share.ID, 4, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restored revoked share: %v", err)
	}
}

func TestSharePageTiedDatesIsolationAndUnavailableSource(t *testing.T) {
	f := seedShare(t, false)
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		if _, _, err := f.s.Create(f.owner, f.file.ID, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.db.Model(&Share{}).Where("owner_id = ?", f.owner).Update("created_at", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).Error; err != nil {
		t.Fatal(err)
	}
	seen := map[uuid.UUID]bool{}
	cursor := ""
	for {
		items, next, err := f.s.ListPage(ctx, f.owner, cursor, 2)
		if err != nil || len(items) > 2 {
			t.Fatalf("page: %d %v", len(items), err)
		}
		for _, item := range items {
			if seen[item.ID] {
				t.Fatal("duplicate share")
			}
			seen[item.ID] = true
		}
		if next == "" {
			break
		}
		if _, _, err := f.s.ListPage(ctx, uuid.New(), next, 2); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("foreign cursor: %v", err)
		}
		cursor = next
	}
	if len(seen) != 7 {
		t.Fatalf("visited %d", len(seen))
	}
	for _, limit := range []int{0, -1, 101} {
		if _, _, err := f.s.ListPage(ctx, f.owner, "", limit); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
	if _, _, err := f.s.ListPage(ctx, f.owner, "garbage", 2); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	if err := f.db.Model(&drive.FileEntry{}).Where("id = ?", f.file.ID).Update("deleted_at", time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	items, _, err := f.s.ListPage(ctx, f.owner, "", 10)
	if err != nil || len(items) != 7 || shareViewResponse(items[0])["status"] != "unavailable" {
		t.Fatalf("unavailable: %+v %v", items, err)
	}
	if _, _, err := f.s.Create(f.owner, f.file.ID, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("share deleted source: %v", err)
	}
}

func TestConcurrentShareEditsAndRevokes(t *testing.T) {
	f := seedShare(t, true)
	ctx := outbox.WithV2EventsEnabled(context.Background(), true)
	share, _, err := f.s.Create(f.owner, f.file.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- f.s.UpdateExpiry(ctx, f.owner, share.ID, 1, nil) }()
	}
	wg.Wait()
	close(errs)
	winners := 0
	for err := range errs {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrVersionConflict) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("edit winners=%d", winners)
	}
	errs = make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- f.s.RevokeWithContext(ctx, f.owner, share.ID) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int64
	if err := f.db.Table("outbox_events").Where("aggregate_id = ? AND event_type = ?", share.ID, eventcontract.EventShareRevokedV1).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("revocation events=%d err=%v", count, err)
	}
}

func TestShareCreateSerializesWithSourceDeletion(t *testing.T) {
	f := seedShare(t, true)
	tx := f.db.Begin()
	defer tx.Rollback()
	if err := tx.Model(&drive.FileEntry{}).Where("id = ?", f.file.ID).Update("deleted_at", time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _, _, err := f.s.CreateWithContext(ctx, f.owner, f.file.ID, nil); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("creation did not wait for source lock: %v", err)
	case <-time.After(80 * time.Millisecond):
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrNotFound) {
		t.Fatalf("created after source deletion: %v", err)
	}
}

func TestShareHTTPManagementAndExpiryValidation(t *testing.T) {
	f := seedShare(t, false)
	h := NewHTTP(f.s, nil, func(c *gin.Context) { httpapi.SetPrincipalID(c, f.owner); c.Next() })
	r := gin.New()
	h.RegisterRoutes(r.Group("/api/v1"))
	call := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w
	}
	w := call("POST", "/api/v1/shares", fmt.Sprintf(`{"file_id":%q,"expires_at":null}`, f.file.ID), 201)
	var created struct {
		Share struct {
			ID uuid.UUID `json:"id"`
		} `json:"share"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/shares/" + created.Share.ID.String()
	for _, body := range []string{`{}`, `{"version":1}`, `{"version":1,"expires_at":"bad"}`, `{"version":-1,"expires_at":null}`} {
		call("PATCH", path, body, 422)
	}
	call("PATCH", path, `{"version":1,"expires_at":null}`, 200)
	call("PATCH", path, `{"version":1,"expires_at":null}`, 409)
	call("GET", "/api/v1/shares?limit=1", "", 200)
	call("GET", "/api/v1/shares?limit=0", "", 422)
	call("DELETE", path, "", 204)
	call("DELETE", path, "", 204)
	call("PATCH", path, `{"version":3,"expires_at":null}`, 404)
	call("GET", "/api/v1/public/shares/file?access_token=garbage", "", 404)
}
