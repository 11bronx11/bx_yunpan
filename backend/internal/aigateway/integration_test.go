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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type eligibilityClient struct {
	pbv2.AIQueryServiceClient
	ready string
}

func (c *eligibilityClient) BatchCapabilities(_ context.Context, r *pbv2.BatchCapabilitiesRequest, _ ...grpc.CallOption) (*pbv2.BatchCapabilitiesResponse, error) {
	out := &pbv2.BatchCapabilitiesResponse{}
	for _, o := range r.Objects {
		out.Items = append(out.Items, &pbv2.ObjectCapabilities{Object: o, Capabilities: &pbv2.Capabilities{Askable: o.ObjectId == c.ready, Text: &pbv2.Capability{Available: o.ObjectId == c.ready}, Vector: &pbv2.Capability{LatestBuild: &pbv2.LatestBuild{State: "failed"}}, Summary: &pbv2.Capability{LatestBuild: &pbv2.LatestBuild{State: "failed"}}}})
	}
	return out, nil
}
func TestSparseEligibilityCursorAndCoreGrantBoundaries(t *testing.T) {
	db := testdb.Business(t)
	ctx := context.Background()
	owner := identity.User{ID: uuid.New(), Username: "scope-" + uuid.NewString(), PasswordHash: "fixture", Status: 1, QuotaBytes: 1 << 30}
	owner.UsernameNormalized = owner.Username
	if err := db.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	if err := drive.NewService(db).ProvisionUser(db, owner.ID); err != nil {
		t.Fatal(err)
	}
	var root struct{ ID uuid.UUID }
	if err := db.Table("folders").Select("id").Where("owner_id=? AND parent_id IS NULL", owner.ID).Take(&root).Error; err != nil {
		t.Fatal(err)
	}
	// Eligible text sits beyond an entire scan budget. Noneligible files are
	// actual migrated business rows, not a mocked first-page array.
	if err := db.Exec(`WITH objects AS (INSERT INTO file_objects(id,sha256,size_bytes,mime_type,bucket,object_key,status,reference_count,version) SELECT gen_random_uuid(),md5(n::text)||md5(n::text),10,'text/plain','fixture',n::text,'ready',1,1 FROM generate_series(1,1001)n RETURNING id,object_key) INSERT INTO file_entries(id,owner_id,folder_id,object_id,name,name_normalized,version) SELECT gen_random_uuid(),?,?,id,lpad(object_key,4,'0')||'.txt',lpad(object_key,4,'0')||'.txt',1 FROM objects`, owner.ID, root.ID).Error; err != nil {
		t.Fatal(err)
	}
	var last struct{ ID, ObjectID uuid.UUID }
	if err := db.Table("file_entries").Where("owner_id=? AND name='1001.txt'", owner.ID).Take(&last).Error; err != nil {
		t.Fatal(err)
	}
	public, key, _ := ed25519.GenerateKey(rand.Reader)
	h := &HTTP{DB: db, Enabled: true, Client: &eligibilityClient{ready: last.ObjectID.String()}, ScopeKey: key, QueryTimeout: 15 * time.Second}
	router := gin.New()
	h.Authenticate = func(c *gin.Context) { httpapi.SetPrincipalID(c, owner.ID) }
	h.RegisterRoutes(router.Group("/api/v1"))
	request := func(method, path, body string, want int) map[string]any {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := request("GET", "/api/v1/ai/files?limit=100", "", 200)
	page := first["page"].(map[string]any)
	if len(first["items"].([]any)) != 0 || !first["scan_incomplete"].(bool) || !page["has_more"].(bool) || page["next_cursor"] == "" {
		t.Fatal("sparse page lost continuation", first)
	}
	second := request("GET", "/api/v1/ai/files?limit=100&cursor="+page["next_cursor"].(string), "", 200)
	if len(second["items"].([]any)) != 1 || second["items"].([]any)[0].(map[string]any)["id"] != last.ID.String() {
		t.Fatal("eligible file behind scan bound was lost", second)
	}
	if _, err := h.grant(ctx, owner.ID, aiprotocol.Scope{Mode: "selected"}); err == nil {
		t.Fatal("empty selected normalized to all")
	}
	grant, err := h.grant(ctx, owner.ID, aiprotocol.Scope{Mode: "selected", FileIDs: []uuid.UUID{last.ID}})
	if err != nil {
		t.Fatal(err)
	}
	svc := &core.Service{DB: db, ScopeKey: public}
	if _, err := svc.OpenRetrievalScope(ctx, &corepb.OpenScopeRequest{Grant: grant}); err != nil {
		t.Fatal(err)
	}
	_, wrong, _ := ed25519.GenerateKey(rand.Reader)
	forged, _ := serviceauth.Sign(wrong, "core-retrieval-v1", time.Now().Add(time.Minute).Unix(), aiprotocol.ScopeGrant{Principal: owner.ID, Scope: aiprotocol.Scope{Mode: "all"}, Action: "retrieve", ExpiresAt: time.Now().Add(time.Minute).Unix()})
	if _, err := svc.OpenRetrievalScope(ctx, &corepb.OpenScopeRequest{Grant: forged}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("forged scope accepted", err)
	}
	if err := db.Exec("UPDATE file_entries SET deleted_at=clock_timestamp() WHERE id=?", last.ID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OpenRetrievalScope(ctx, &corepb.OpenScopeRequest{Grant: grant}); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("deleted entry remained authorized", err)
	}
	if err := db.Exec("UPDATE owner_content_epochs SET content_epoch=content_epoch+1 WHERE owner_id=?", owner.ID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OpenRetrievalScope(ctx, &corepb.OpenScopeRequest{Grant: grant}); status.Code(err) != codes.Aborted {
		t.Fatal("changed epoch accepted", err)
	}
	request("GET", "/api/v1/ai/files?limit=100&cursor="+page["next_cursor"].(string), "", 409)
	// Filename search remains a business-only endpoint when AI is unavailable.
	h.Enabled = false
	h.Client = nil
	name := request("POST", "/api/v1/search", fmt.Sprintf(`{"query":"0001","mode":"name"}`), 200)
	if len(name["hits"].([]any)) != 1 {
		t.Fatal("AI outage disabled core name search", name)
	}
}
