package aigateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pbv2"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/core"
	corepb "github.com/11bronx11/bx_yunpan/backend/internal/core/pb"
	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/httpapi"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/serviceauth"
	"github.com/11bronx11/bx_yunpan/backend/internal/testdb"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"gorm.io/gorm"
)

type askScopeClient struct {
	pbv2.AIQueryServiceClient
	key    ed25519.PublicKey
	calls  int
	grant  aiprotocol.ScopeGrant
	core   *core.Service
	before func()
}

func (c *askScopeClient) Ask(ctx context.Context, r *pbv2.AskRequest, _ ...grpc.CallOption) (*pbv2.AskResponse, error) {
	c.calls++
	if err := serviceauth.Verify(c.key, r.Grant, "core-retrieval-v1", &c.grant); err != nil {
		return nil, err
	}
	if c.before != nil {
		c.before()
	}
	if _, err := c.core.OpenRetrievalScope(ctx, &corepb.OpenScopeRequest{Grant: r.Grant}); err != nil {
		return nil, err
	}
	if _, err := c.core.StreamScopeMembers(ctx, &corepb.ScopeMembersRequest{Grant: r.Grant}); err != nil {
		return nil, err
	}
	return &pbv2.AskResponse{Answer: "fixture"}, nil
}

func TestLegacyAskFolderAndFilesKeepTheirIntersection(t *testing.T) {
	db := testdb.Business(t)
	owner := identity.User{ID: uuid.New(), Username: uuid.NewString(), PasswordHash: "fixture", Status: 1}
	owner.UsernameNormalized = owner.Username
	if err := db.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	if err := drive.NewService(db).ProvisionUser(db, owner.ID); err != nil {
		t.Fatal(err)
	}
	stranger := identity.User{ID: uuid.New(), Username: uuid.NewString(), PasswordHash: "fixture", Status: 1}
	stranger.UsernameNormalized = stranger.Username
	if err := db.Create(&stranger).Error; err != nil {
		t.Fatal(err)
	}
	if err := drive.NewService(db).ProvisionUser(db, stranger.ID); err != nil {
		t.Fatal(err)
	}
	var otherRoot struct{ ID uuid.UUID }
	if err := db.Table("folders").Select("id").Where("owner_id=? AND parent_id IS NULL", stranger.ID).Take(&otherRoot).Error; err != nil {
		t.Fatal(err)
	}
	var root struct{ ID uuid.UUID }
	if err := db.Table("folders").Select("id").Where("owner_id=? AND parent_id IS NULL", owner.ID).Take(&root).Error; err != nil {
		t.Fatal(err)
	}
	folder, child := uuid.New(), uuid.New()
	for _, f := range []struct{ id, parent uuid.UUID }{{folder, root.ID}, {child, folder}} {
		if err := db.Exec(`INSERT INTO folders(id,owner_id,parent_id,name,name_normalized) VALUES(?,?,?,? ,?)`, f.id, owner.ID, f.parent, f.id.String(), f.id.String()).Error; err != nil {
			t.Fatal(err)
		}
	}
	inside, outside := uuid.New(), uuid.New()
	for _, f := range []struct{ id, folder uuid.UUID }{{inside, child}, {outside, root.ID}} {
		if err := db.Exec(`WITH o AS (INSERT INTO file_objects(id,sha256,size_bytes,mime_type,bucket,object_key,status,reference_count) VALUES(gen_random_uuid(),md5(?)||md5(?),10,'text/plain','fixture',?,'ready',1) RETURNING id) INSERT INTO file_entries(id,owner_id,folder_id,object_id,name,name_normalized,version) SELECT ?,?,?,id,?,?,1 FROM o`, f.id.String(), f.id.String(), f.id.String(), f.id, owner.ID, f.folder, f.id.String(), f.id.String()).Error; err != nil {
			t.Fatal(err)
		}
	}
	foreign := uuid.New()
	if err := db.Exec(`WITH o AS (INSERT INTO file_objects(id,sha256,size_bytes,mime_type,bucket,object_key,status,reference_count) VALUES(gen_random_uuid(),md5(?)||md5(?),10,'text/plain','fixture',?,'ready',1) RETURNING id) INSERT INTO file_entries(id,owner_id,folder_id,object_id,name,name_normalized,version) SELECT ?,?,?,id,?,?,1 FROM o`, foreign.String(), foreign.String(), foreign.String(), foreign, stranger.ID, otherRoot.ID, foreign.String(), foreign.String()).Error; err != nil {
		t.Fatal(err)
	}
	public, key, _ := ed25519.GenerateKey(rand.Reader)
	client := &askScopeClient{key: public, core: &core.Service{DB: db, ScopeKey: public}}
	h := &HTTP{DB: db, Enabled: true, Client: client, ScopeKey: key, AskTimeout: time.Second, Authenticate: func(c *gin.Context) { httpapi.SetPrincipalID(c, owner.ID) }}
	router := gin.New()
	h.RegisterRoutes(router.Group("/api/v1"))
	for _, tc := range []struct {
		name   string
		ids    []uuid.UUID
		folder uuid.UUID
		want   int
	}{
		{"nested-intersection", []uuid.UUID{inside, outside}, folder, 200},
		{"empty-intersection", []uuid.UUID{outside}, folder, 409},
		{"missing-folder", []uuid.UUID{inside}, uuid.New(), 409},
		{"foreign-file", []uuid.UUID{foreign}, folder, 409},
		{"foreign-folder", []uuid.UUID{foreign}, otherRoot.ID, 409},
		{"mixed-owner-intersection", []uuid.UUID{inside, foreign}, folder, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"question": "rollback", "folder_id": tc.folder, "file_ids": tc.ids})
			req := httptest.NewRequest("POST", "/api/v1/ai/ask", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			before := client.calls
			router.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("got %d %s, want %d", w.Code, w.Body, tc.want)
			}
			if tc.want == 200 {
				if client.calls != before+1 || client.grant.Scope.Mode != "selected" || fmt.Sprint(client.grant.Scope.FileIDs) != fmt.Sprint([]uuid.UUID{inside}) {
					t.Fatal("folder was ignored", client.grant.Scope)
				}
			} else if client.calls != before {
				t.Fatal("invalid intersection reached query")
			}
		})
	}
	t.Run("move-during-intersection", func(t *testing.T) {
		changed := false
		callback := "test:move-during-legacy-intersection"
		if err := db.Callback().Query().After("gorm:after_query").Register(callback, func(tx *gorm.DB) {
			query := tx.Statement.SQL.String()
			if changed || !strings.Contains(query, "file_entries e") || !strings.Contains(query, "e.id IN") {
				return
			}
			changed = true
			if err := db.Exec("UPDATE file_entries SET folder_id=?,version=version+1 WHERE id=?", root.ID, inside).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Exec("UPDATE owner_content_epochs SET content_epoch=content_epoch+1 WHERE owner_id=?", owner.ID).Error; err != nil {
				t.Fatal(err)
			}
		}); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = db.Callback().Query().Remove(callback)
			if err := db.Exec("UPDATE file_entries SET folder_id=?,version=version+1 WHERE id=?", child, inside).Error; err != nil {
				t.Fatal(err)
			}
		}()
		body, _ := json.Marshal(map[string]any{"question": "rollback", "folder_id": folder, "file_ids": []uuid.UUID{inside}})
		req := httptest.NewRequest("POST", "/api/v1/ai/ask", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if !changed || w.Code != 409 || !strings.Contains(w.Body.String(), "ai.scope_changed") {
			t.Fatalf("move during intersection was reauthorized: changed=%t HTTP %d %s", changed, w.Code, w.Body)
		}
	})
	t.Run("scope-changed-before-query", func(t *testing.T) {
		client.before = func() {
			if err := db.Exec("UPDATE owner_content_epochs SET content_epoch=content_epoch+1 WHERE owner_id=?", owner.ID).Error; err != nil {
				t.Fatal(err)
			}
		}
		defer func() { client.before = nil }()
		body, _ := json.Marshal(map[string]any{"question": "rollback", "folder_id": folder, "file_ids": []uuid.UUID{inside}})
		req := httptest.NewRequest("POST", "/api/v1/ai/ask", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "ai.scope_changed") {
			t.Fatalf("changed grant remained usable: %d %s", w.Code, w.Body)
		}
	})
}
