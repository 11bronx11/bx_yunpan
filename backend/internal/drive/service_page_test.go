package drive

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/ownerepoch"
	platformcache "github.com/11bronx11/bx_yunpan/backend/internal/platform/cache"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/httpapi"
)

func TestFirstPageFallsBackToSnapshotWhenEpochChanges(t *testing.T) {
	f := seedPageFixture(t)
	for _, cached := range []bool{false, true} {
		reader := NewService(f.db)
		if cached {
			reader = NewServiceWithCache(f.db, platformcache.NewJSONStore(nil, time.Minute, 8))
		}
		changed := false
		callback := "test:first-page-epoch-race"
		if err := f.db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
			if _, ok := tx.Statement.Model.(*ownerepoch.Epoch); !ok || changed {
				return
			}
			changed = true
			if _, err := ownerepoch.BumpContent(f.db, f.ownerID); err != nil {
				tx.AddError(err)
			}
		}); err != nil {
			t.Fatal(err)
		}
		folders, files, more, cursor, err := reader.ChildrenPage(context.Background(), f.ownerID, f.target.ID, "", 3, "")
		_ = f.db.Callback().Query().Remove(callback)
		if !changed || err != nil || !more || cursor == "" || len(folders)+len(files) != 3 {
			t.Fatalf("cached=%v changed=%v page=%d more=%v cursor=%q err=%v", cached, changed, len(folders)+len(files), more, cursor, err)
		}
		if _, _, _, _, err := reader.ChildrenPage(context.Background(), f.ownerID, f.target.ID, cursor, 3, ""); err != nil {
			t.Fatalf("snapshot cursor: %v", err)
		}
	}
}

func seedPageFixture(t *testing.T) moveFixture {
	t.Helper()
	f := newMoveFixture(t)
	for _, name := range []string{"zeta", "Alpha", "a-b", "a_b", "a b"} {
		if _, err := f.service.Create(f.ownerID, f.target.ID, name); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"z.pdf", "Alpha.pdf", "a-b.pdf", "a_b.pdf", "a b.pdf", "m.pdf", "n.pdf", "o.pdf", strings.Repeat("x", 255)} {
		if _, err := f.service.CreateFile(f.db, f.ownerID, f.target.ID, createMoveObject(t, f.db), name); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func TestChildrenPageVisitsEachVisibleItemInDatabaseOrder(t *testing.T) {
	f := seedPageFixture(t)
	for _, order := range []string{"ASC", "DESC"} {
		var expected []uuid.UUID
		if err := f.db.Raw(`
			SELECT id FROM (
				SELECT id, name_normalized, 0 AS kind FROM folders
				WHERE owner_id = ? AND parent_id = ? AND deleted_at IS NULL
				UNION ALL
				SELECT e.id, e.name_normalized, 1 AS kind FROM file_entries e
				JOIN file_objects o ON o.id = e.object_id AND o.status = 'ready'
				WHERE e.owner_id = ? AND e.folder_id = ? AND e.deleted_at IS NULL
			) items ORDER BY kind, name_normalized `+order+`, id `+order,
			f.ownerID, f.target.ID, f.ownerID, f.target.ID).Scan(&expected).Error; err != nil {
			t.Fatal(err)
		}
		if len(expected) != 14 {
			t.Fatalf("fixture has %d visible rows, want 14", len(expected))
		}
		for _, limit := range []int{1, 3, 5, 8, 100} {
			t.Run(fmt.Sprintf("%s/limit_%d", order, limit), func(t *testing.T) {
				var actual []uuid.UUID
				cursor := ""
				for pages := 0; ; pages++ {
					if pages > len(expected) {
						t.Fatal("pagination did not terminate")
					}
					folders, files, more, next, err := f.service.ChildrenPage(context.Background(), f.ownerID, f.target.ID, cursor, limit, "name_"+strings.ToLower(order))
					if err != nil {
						t.Fatal(err)
					}
					if count := len(folders) + len(files); count > limit || (more && count != limit) {
						t.Fatalf("page size=%d limit=%d has_more=%v", count, limit, more)
					}
					for _, folder := range folders {
						actual = append(actual, folder.ID)
					}
					for _, file := range files {
						actual = append(actual, file.ID)
					}
					if !more {
						if next != "" {
							t.Fatal("last page has a cursor")
						}
						break
					}
					if next == "" || next == cursor {
						t.Fatal("cursor failed to advance")
					}
					cursor = next
				}
				if !reflect.DeepEqual(actual, expected) {
					t.Fatalf("page sequence=%v, want %v", actual, expected)
				}
			})
		}
	}
}

func TestChildrenPageCursorScopeAndEpoch(t *testing.T) {
	f := seedPageFixture(t)
	reader := NewServiceWithCache(f.db, platformcache.NewJSONStore(nil, time.Minute, 8))
	pageCursor := func() string {
		t.Helper()
		_, _, more, cursor, err := reader.ChildrenPage(context.Background(), f.ownerID, f.target.ID, "", 3, "")
		if err != nil || !more || cursor == "" {
			t.Fatalf("first page: more=%v cursor=%q error=%v", more, cursor, err)
		}
		return cursor
	}
	cursor := pageCursor()
	if cached := pageCursor(); cached != cursor {
		t.Fatal("unchanged cached first page differs")
	}
	for _, tc := range []struct {
		name   string
		folder uuid.UUID
		cursor string
		sort   string
	}{
		{"malformed", f.target.ID, "!invalid", ""},
		{"incomplete", f.target.ID, base64.RawURLEncoding.EncodeToString([]byte(`{"v":"1"}`)), ""},
		{"folder scope", f.source.ID, cursor, ""},
		{"sort scope", f.target.ID, cursor, "name_desc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, err := reader.ChildrenPage(context.Background(), f.ownerID, tc.folder, tc.cursor, 3, tc.sort)
			if !errors.Is(err, ErrInvalidCursor) {
				t.Fatalf("error=%v, want invalid cursor", err)
			}
		})
	}
	parsed, err := decodeChildCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Scope = cursorScope(uuid.New(), f.target.ID)
	otherOwnerCursor, _ := encodeChildCursor(parsed)
	if _, _, _, _, err := reader.ChildrenPage(context.Background(), f.ownerID, f.target.ID, otherOwnerCursor, 3, ""); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("owner scope: %v", err)
	}
	if _, err := f.service.Create(f.ownerID, f.target.ID, "new-folder"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := reader.ChildrenPage(context.Background(), f.ownerID, f.target.ID, cursor, 3, ""); !errors.Is(err, ErrCursorStale) {
		t.Fatalf("content epoch: %v", err)
	}
	newCursor := pageCursor()
	if newCursor == cursor {
		t.Fatal("first page cache ignored another service's mutation")
	}
	if _, err := ownerepoch.BumpACL(f.db, f.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := reader.ChildrenPage(context.Background(), f.ownerID, f.target.ID, newCursor, 3, ""); !errors.Is(err, ErrCursorStale) {
		t.Fatalf("ACL epoch: %v", err)
	}
}

func TestChildrenPageBoundsVisibilityAndCancellation(t *testing.T) {
	f := newMoveFixture(t)
	for _, limit := range []int{-1, 101} {
		if _, _, _, _, err := f.service.ChildrenPage(context.Background(), f.ownerID, f.source.ID, "", limit, ""); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
	if _, _, _, _, err := f.service.ChildrenPage(context.Background(), f.ownerID, f.source.ID, "", 1, "invalid"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("sort: %v", err)
	}
	if _, _, _, _, err := f.service.ChildrenPage(context.Background(), uuid.New(), f.source.ID, "", 0, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner visibility: %v", err)
	}
	if err := f.db.Exec("UPDATE file_objects SET status = 'deleting' WHERE id = ?", f.object).Error; err != nil {
		t.Fatal(err)
	}
	folders, files, more, cursor, err := f.service.ChildrenPage(context.Background(), f.ownerID, f.source.ID, "", 0, "")
	if err != nil || len(folders)+len(files) != 0 || more || cursor != "" {
		t.Fatalf("non-ready object was visible: folders=%v files=%v more=%v cursor=%q error=%v", folders, files, more, cursor, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, _, err := f.service.ChildrenPage(ctx, f.ownerID, f.source.ID, "", 1, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request: %v", err)
	}
}

func TestChildrenPageHTTPContract(t *testing.T) {
	f := seedPageFixture(t)
	router := gin.New()
	NewHTTP(f.service, func(c *gin.Context) { httpapi.SetPrincipalID(c, f.ownerID) }).RegisterRoutes(router.Group("/api/v1"))
	request := func(query string, want int) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/folders/"+f.target.ID.String()+"/children"+query, nil))
		if response.Code != want {
			t.Fatalf("query=%q status=%d want=%d body=%s", query, response.Code, want, response.Body)
		}
		return response
	}
	for _, query := range []string{"?limit=0", "?limit=-1", "?limit=101", "?limit=abc", "?sort=invalid", "?cursor=!bad"} {
		request(query, http.StatusUnprocessableEntity)
	}
	response := request("?limit=3&sort=name_desc", http.StatusOK)
	var body struct {
		Items []struct{ ID uuid.UUID }
		Page  struct {
			HasMore    bool   `json:"has_more"`
			NextCursor string `json:"next_cursor"`
		}
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 3 || !body.Page.HasMore || body.Page.NextCursor == "" {
		t.Fatalf("unexpected response: %s", response.Body)
	}
	query := "?limit=3&sort=name_desc&cursor=" + body.Page.NextCursor
	request(query, http.StatusOK)
	if _, err := ownerepoch.BumpContent(f.db, f.ownerID); err != nil {
		t.Fatal(err)
	}
	stale := request(query, http.StatusConflict)
	if !strings.Contains(stale.Body.String(), "drive.cursor_stale") {
		t.Fatalf("stale error: %s", stale.Body)
	}
}
