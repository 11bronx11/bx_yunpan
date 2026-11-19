// Package aigateway adapts authenticated HTTP to AI v2. Commands and user jobs
// are business-local; this package never opens an AI or control SQL connection.
package aigateway

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pbv2"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/core"
	corepb "github.com/11bronx11/bx_yunpan/backend/internal/core/pb"
	"github.com/11bronx11/bx_yunpan/backend/internal/ownerepoch"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/grpcx"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/httpapi"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/serviceauth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"gorm.io/gorm"
)

type HTTP struct {
	Limiter                  ai.RequestLimiter
	DB                       *gorm.DB
	Client                   pbv2.AIQueryServiceClient
	ScopeKey                 ed25519.PrivateKey
	Profile                  aiprotocol.Profile
	Enabled                  bool
	QueryTimeout, AskTimeout time.Duration
	Authenticate             gin.HandlerFunc
}

func (h *HTTP) RegisterRoutes(v1 *gin.RouterGroup) {
	v1.POST("/search", h.Authenticate, h.limit(), h.search)
	v1.POST("/ai/ask", h.Authenticate, h.limit(), h.ask)
	v1.GET("/ai/files", h.Authenticate, h.files)
	v1.GET("/files/:fileId/ai", h.Authenticate, h.overview)
	v1.POST("/files/:fileId/ai/reprocess", h.Authenticate, h.limit(), h.reprocess)
	v1.GET("/ai/jobs/:taskId", h.Authenticate, h.job)
}

func (h *HTTP) rate(c *gin.Context, action string) bool {
	if h.Limiter == nil {
		return true
	}
	owner, _ := httpapi.PrincipalID(c)
	allowed, err := h.Limiter.Allow(c.Request.Context(), owner, action)
	if err == nil && !allowed {
		c.JSON(429, httpapi.ErrorEnvelope(c, "ai.rate_limited", "请求过于频繁，请稍后重试"))
		return false
	}
	return true
}

func (h *HTTP) limit() gin.HandlerFunc {
	return func(c *gin.Context) { c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10); c.Next() }
}

func (h *HTTP) grant(ctx context.Context, owner uuid.UUID, scope aiprotocol.Scope) (string, error) {
	if err := scope.Normalize(); err != nil {
		return "", err
	}
	epoch, err := ownerepoch.Current(h.DB.WithContext(ctx), owner)
	if err != nil {
		return "", err
	}
	g := aiprotocol.ScopeGrant{Principal: owner, Scope: scope, ContentEpoch: epoch.ContentEpoch, ACLEpoch: epoch.ACLEpoch, RequestID: uuid.NewString(), Action: "retrieve", ExpiresAt: time.Now().Add(h.scopeGrantTTL(scope)).Unix()}
	return serviceauth.Sign(h.ScopeKey, "core-retrieval-v1", g.ExpiresAt, g)
}

func (h *HTTP) askTimeout(scope aiprotocol.Scope) time.Duration {
	timeout := max(h.AskTimeout, time.Second)
	if scope.Mode == "selected" {
		// Legacy folder+file filters can resolve to fewer files. Reserve the
		// single-file deadline so that intersection can use its full detail tier.
		timeout *= time.Duration(aiprotocol.SelectedAskMultiplier(1))
	}
	return timeout
}

func (h *HTTP) scopeGrantTTL(scope aiprotocol.Scope) time.Duration {
	if scope.Mode == "selected" {
		return max(2*time.Minute, h.askTimeout(scope)+time.Minute)
	}
	return 2 * time.Minute
}

// Legacy requests may supply both filters. Resolve their intersection in core
// business SQL, then retain that grant's epochs when signing the selected scope.
// A move or revocation during resolution must invalidate the resulting grant.
func (h *HTTP) legacyAskGrant(ctx context.Context, owner uuid.UUID, selected aiprotocol.Scope, folder uuid.UUID) (string, error) {
	if err := selected.Normalize(); err != nil {
		return "", err
	}
	token, err := h.grant(ctx, owner, aiprotocol.Scope{Mode: "folder", FolderID: &folder, IncludeSubfolders: true})
	if err != nil {
		return "", err
	}
	public := h.ScopeKey.Public().(ed25519.PublicKey)
	svc := &core.Service{DB: h.DB, ScopeKey: public}
	if _, err := svc.OpenRetrievalScope(ctx, &corepb.OpenScopeRequest{Grant: token}); err != nil {
		return "", err
	}
	ids := make([]string, 0, len(selected.FileIDs))
	for _, id := range selected.FileIDs {
		ids = append(ids, id.String())
	}
	refs, err := svc.BatchGetFileRefs(ctx, &corepb.FileRefsRequest{Grant: token, FileIds: ids})
	if err != nil {
		return "", err
	}
	if len(refs.Files) == 0 {
		return "", status.Error(codes.FailedPrecondition, "ai.scope_invalid")
	}
	selected.FileIDs = nil
	for _, f := range refs.Files {
		selected.FileIDs = append(selected.FileIDs, uuid.MustParse(f.FileId))
	}
	if err := selected.Normalize(); err != nil {
		return "", err
	}
	var g aiprotocol.ScopeGrant
	if err := serviceauth.Verify(public, token, "core-retrieval-v1", &g); err != nil {
		return "", status.Error(codes.Unavailable, "ai.unavailable")
	}
	g.Scope = selected
	g.ExpiresAt = time.Now().Add(h.scopeGrantTTL(selected)).Unix()
	return serviceauth.Sign(h.ScopeKey, "core-retrieval-v1", g.ExpiresAt, g)
}

func (h *HTTP) available(c *gin.Context) bool {
	if !h.Enabled || h.Client == nil {
		h.fail(c, status.Error(codes.Unavailable, "ai.unavailable"))
		return false
	}
	return true
}

func (h *HTTP) fail(c *gin.Context, err error) {
	code := "ai.unavailable"
	httpCode := 503
	message := "AI 服务暂不可用"
	switch {
	case errors.Is(err, aiprotocol.ErrInvalid):
		code = "ai.invalid_scope"
		httpCode = 400
		message = "文件范围或请求参数无效"
	case strings.HasPrefix(err.Error(), "ai.idempotency_conflict"):
		code = "ai.idempotency_conflict"
		httpCode = 409
		message = "同一请求标识对应的参数不同"
	case strings.HasPrefix(err.Error(), "ai.capability_"):
		code = err.Error()
		httpCode = 422
		message = "当前文件或配置不支持所选能力"
	default:
		switch status.Code(err) {
		case codes.InvalidArgument:
			code = "ai.invalid_scope"
			httpCode = 400
			message = "请求参数无效"
		case codes.FailedPrecondition:
			code = "ai.scope_invalid"
			httpCode = 409
			message = "部分文件已不可用，请重新选择"
		case codes.Aborted:
			code = "ai.scope_changed"
			httpCode = 409
			message = "文件范围已变化，请更新选择后重试"
		case codes.NotFound:
			code = "drive.not_found"
			httpCode = 404
			message = "文件不可访问"
		case codes.ResourceExhausted:
			code = "ai.scope_too_large"
			httpCode = 422
			message = "文件范围超过本次查询预算"
			for _, detail := range status.Convert(err).Details() {
				if info, ok := detail.(*errdetails.ErrorInfo); ok && info.Domain == "ai.yunpan" && info.Reason == "ai.quota_exhausted" {
					httpCode, code, message = ai.HTTPStatus(ai.ErrQuota)
					break
				}
			}
		case codes.DeadlineExceeded:
			code = "ai.query_timeout"
			httpCode = 504
			message = "查询超时，请稍后重试"
		case codes.Unavailable:
			if status.Convert(err).Message() == "ai.semantic_unavailable" {
				code = "ai.semantic_unavailable"
				message = "语义检索暂不可用"
			}
			if status.Convert(err).Message() == "ai.answer_unavailable" {
				code = "ai.answer_unavailable"
				message = "回答服务暂不可用"
			}
		}
	}
	envelope := httpapi.ErrorEnvelope(c, code, message)
	if code == "ai.scope_invalid" {
		invalid := []gin.H{}
		for _, detail := range status.Convert(err).Details() {
			if bad, ok := detail.(*errdetails.BadRequest); ok {
				for _, v := range bad.FieldViolations {
					if _, e := uuid.Parse(v.Field); e == nil && len(invalid) < 50 {
						invalid = append(invalid, gin.H{"file_id": v.Field, "reason": v.Description})
					}
				}
			}
		}
		envelope["invalid_files"] = invalid
	}
	c.JSON(httpCode, envelope)
}

func asMap(m proto.Message) map[string]any {
	b, _ := (protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}).Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	normalizeProtoNumbers(out, m.ProtoReflect().Descriptor())
	return out
}

// Protobuf JSON encodes int64 as strings; the public HTTP contract uses JSON
// numbers. Preserve that existing contract, including exact integer precision.
func normalizeProtoNumbers(out map[string]any, descriptor protoreflect.MessageDescriptor) {
	fields := descriptor.Fields()
	var normalize func(any, protoreflect.FieldDescriptor) any
	normalize = func(value any, field protoreflect.FieldDescriptor) any {
		switch field.Kind() {
		case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
			if number, ok := value.(string); ok {
				return json.Number(number)
			}
		case protoreflect.MessageKind, protoreflect.GroupKind:
			if nested, ok := value.(map[string]any); ok {
				normalizeProtoNumbers(nested, field.Message())
			}
		}
		return value
	}
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		name := string(field.Name())
		value, exists := out[name]
		if !exists {
			continue
		}
		if field.IsMap() {
			if values, ok := value.(map[string]any); ok {
				for key, v := range values {
					values[key] = normalize(v, field.MapValue())
				}
			}
		} else if field.IsList() {
			if values, ok := value.([]any); ok {
				for index, v := range values {
					values[index] = normalize(v, field)
				}
			}
		} else {
			out[name] = normalize(value, field)
		}
	}
}

func (h *HTTP) ask(c *gin.Context) {
	if !h.rate(c, "ask") {
		return
	}
	if !h.available(c) {
		return
	}
	var req struct {
		Question string            `json:"question"`
		Scope    *aiprotocol.Scope `json:"scope"`
		FileIDs  []uuid.UUID       `json:"file_ids"`
		FolderID *uuid.UUID        `json:"folder_id"`
	}
	if c.ShouldBindJSON(&req) != nil || utf8.RuneCountInString(req.Question) > 2000 || strings.TrimSpace(req.Question) == "" {
		h.fail(c, aiprotocol.ErrInvalid)
		return
	}
	scope := aiprotocol.Scope{Mode: "all"}
	if req.Scope != nil {
		if len(req.FileIDs) > 0 || req.FolderID != nil {
			h.fail(c, aiprotocol.ErrInvalid)
			return
		}
		scope = *req.Scope
	} else if len(req.FileIDs) > 0 {
		scope = aiprotocol.Scope{Mode: "selected", FileIDs: req.FileIDs}
	} else if req.FolderID != nil {
		scope = aiprotocol.Scope{Mode: "folder", FolderID: req.FolderID, IncludeSubfolders: true}
	}
	owner, _ := httpapi.PrincipalID(c)
	var grant string
	var err error
	if req.Scope == nil && len(req.FileIDs) > 0 && req.FolderID != nil {
		grant, err = h.legacyAskGrant(c.Request.Context(), owner, scope, *req.FolderID)
	} else {
		grant, err = h.grant(c.Request.Context(), owner, scope)
	}
	if err != nil {
		h.fail(c, err)
		return
	}
	timeout := h.askTimeout(scope)
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	ctx = grpcx.WithAskTimeout(ctx, timeout)
	resp, err := h.Client.Ask(ctx, &pbv2.AskRequest{Grant: grant, Question: req.Question, RequestId: httpapi.RequestIDValue(c)})
	if err != nil {
		h.fail(c, err)
		return
	}
	out := asMap(resp)
	out["grounded"] = len(resp.Citations) > 0
	c.JSON(200, out)
}

func (h *HTTP) search(c *gin.Context) {
	var r struct {
		Query, Mode       string
		FolderID          *uuid.UUID `json:"folder_id"`
		IncludeSubfolders *bool      `json:"include_subfolders"`
		MimeTypes         []string   `json:"mime_types"`
		Limit             int
	}
	if c.ShouldBindJSON(&r) != nil || strings.TrimSpace(r.Query) == "" || utf8.RuneCountInString(r.Query) > 1000 || len(r.MimeTypes) > 20 || (r.Mode != "name" && r.Mode != "fulltext" && r.Mode != "semantic" && r.Mode != "hybrid") {
		h.fail(c, aiprotocol.ErrInvalid)
		return
	}
	owner, _ := httpapi.PrincipalID(c)
	scope := aiprotocol.Scope{Mode: "all"}
	if r.FolderID != nil {
		scope = aiprotocol.Scope{Mode: "folder", FolderID: r.FolderID, IncludeSubfolders: r.IncludeSubfolders == nil || *r.IncludeSubfolders}
	}
	if r.Mode == "name" {
		svc := &core.Service{DB: h.DB}
		files, err := core.NameSearch(c.Request.Context(), svc, owner, scope, r.Query, r.MimeTypes, r.Limit)
		if err != nil {
			h.fail(c, err)
			return
		}
		hits := []any{}
		for _, f := range files {
			hits = append(hits, gin.H{"file": fileMap(f), "score": 1, "match_type": "name", "citations": []any{}})
		}
		c.JSON(200, gin.H{"hits": hits, "page": gin.H{"has_more": false}})
		return
	}
	if !h.rate(c, "search") {
		return
	}
	if !h.available(c) {
		return
	}
	grant, err := h.grant(c.Request.Context(), owner, scope)
	if err != nil {
		h.fail(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), max(h.QueryTimeout, time.Second))
	defer cancel()
	resp, err := h.Client.Search(ctx, &pbv2.SearchRequest{Grant: grant, Query: r.Query, Mode: r.Mode, Limit: int32(r.Limit), MimeTypes: r.MimeTypes})
	if err != nil {
		h.fail(c, err)
		return
	}
	out := asMap(resp)
	hits := []any{}
	for _, hit := range resp.Hits {
		value := asMap(hit)
		value["file"] = fileMap(hit.File)
		hits = append(hits, value)
	}
	out["hits"] = hits
	out["page"] = gin.H{"has_more": false}
	c.JSON(200, out)
}

func fileMap(f *corepb.FileRef) gin.H {
	return gin.H{"id": f.FileId, "name": f.Name, "path": f.Path, "folder_id": f.FolderId, "mime_type": f.MimeType, "size_bytes": f.Size, "size": f.Size, "version": f.EntryVersion, "updated_at": f.UpdatedAt}
}

func (h *HTTP) overview(c *gin.Context) {
	if !h.available(c) {
		return
	}
	id, err := uuid.Parse(c.Param("fileId"))
	if err != nil {
		h.fail(c, status.Error(codes.NotFound, "file unavailable"))
		return
	}
	owner, _ := httpapi.PrincipalID(c)
	grant, err := h.grant(c.Request.Context(), owner, aiprotocol.Scope{Mode: "selected", FileIDs: []uuid.UUID{id}})
	if err != nil {
		h.fail(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), max(h.QueryTimeout, time.Second))
	defer cancel()
	resp, err := h.Client.GetOverview(ctx, &pbv2.GetOverviewRequest{Grant: grant, FileId: id.String()})
	if err != nil {
		h.fail(c, err)
		return
	}
	out := asMap(resp)
	out["preview"] = gin.H{"kind": resp.PreviewKind, "state": resp.PreviewState}
	out["status"] = indexStatus(resp.Capabilities)
	out["ai_status"] = out["status"]
	c.JSON(200, out)
}

type fileCursor struct {
	Business    string `json:"b"`
	Fingerprint string `json:"f"`
}

func (h *HTTP) files(c *gin.Context) {
	if !h.available(c) {
		return
	}
	owner, _ := httpapi.PrincipalID(c)
	scope := aiprotocol.Scope{Mode: "all"}
	if folder := c.Query("folder_id"); folder != "" {
		id, err := uuid.Parse(folder)
		if err != nil {
			h.fail(c, aiprotocol.ErrInvalid)
			return
		}
		scope = aiprotocol.Scope{Mode: "folder", FolderID: &id, IncludeSubfolders: c.Query("include_subfolders") != "false"}
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "30"))
	if limit < 1 || limit > 100 {
		h.fail(c, aiprotocol.ErrInvalid)
		return
	}
	sort := c.DefaultQuery("sort", "name_asc")
	mimes := c.QueryArray("mime_types")
	fingerprint := aiprotocol.HashJSON([]any{owner, scope, c.Query("q"), sort, mimes})
	cursor := fileCursor{Fingerprint: fingerprint}
	if token := c.Query("cursor"); token != "" {
		b, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || json.Unmarshal(b, &cursor) != nil || cursor.Fingerprint != fingerprint {
			h.fail(c, aiprotocol.ErrInvalid)
			return
		}
	}
	grant, err := h.grant(c.Request.Context(), owner, scope)
	if err != nil {
		h.fail(c, err)
		return
	}
	svc := &core.Service{DB: h.DB, ScopeKey: h.ScopeKey.Public().(ed25519.PublicKey)}
	ctx, cancel := context.WithTimeout(c.Request.Context(), max(h.QueryTimeout, time.Second))
	defer cancel()
	items := []any{}
	more := true
	scanned := 0
	for more && len(items) < limit && scanned < 1000 {
		page, err := svc.StreamScopeMembers(ctx, &corepb.ScopeMembersRequest{Grant: grant, Cursor: cursor.Business, Limit: int32(min(100, limit-len(items))), Query: c.Query("q"), MimeTypes: mimes, Sort: sort})
		if err != nil {
			h.fail(c, err)
			return
		}
		if len(page.Files) == 0 {
			more = false
			break
		}
		objects := []*pbv2.ObjectRef{}
		for _, f := range page.Files {
			objects = append(objects, &pbv2.ObjectRef{ObjectId: f.ObjectId, ContentGeneration: f.ContentGeneration, MimeType: f.MimeType})
		}
		caps, err := h.Client.BatchCapabilities(ctx, &pbv2.BatchCapabilitiesRequest{Objects: objects})
		if err != nil {
			h.fail(c, err)
			return
		}
		if len(caps.Items) != len(page.Files) {
			h.fail(c, status.Error(codes.Unavailable, "ai.unavailable"))
			return
		}
		for n, f := range page.Files {
			if caps.Items[n].Capabilities.Askable {
				item := fileMap(f)
				item["capabilities"] = asMap(caps.Items[n].Capabilities)
				items = append(items, item)
			}
		}
		scanned += len(page.Files)
		cursor.Business = page.NextCursor
		more = page.HasMore
	}
	token := ""
	if more {
		b, _ := json.Marshal(cursor)
		token = base64.RawURLEncoding.EncodeToString(b)
	}
	c.JSON(200, gin.H{"items": items, "page": gin.H{"next_cursor": token, "has_more": more}, "scan_incomplete": more && scanned >= 1000})
}

func (h *HTTP) reprocess(c *gin.Context) {
	if !h.rate(c, "reprocess") {
		return
	}
	// Acceptance requires no AI connection. Disabled AI is an explicit product setting.
	if !h.Enabled {
		h.fail(c, status.Error(codes.Unavailable, "ai.unavailable"))
		return
	}
	owner, _ := httpapi.PrincipalID(c)
	id, err := uuid.Parse(c.Param("fileId"))
	if err != nil {
		h.fail(c, aiprotocol.ErrInvalid)
		return
	}
	var body struct {
		Targets []string `json:"targets"`
		Profile string   `json:"profile"`
	}
	if err := c.ShouldBindJSON(&body); err != nil && !errors.Is(err, io.EOF) {
		h.fail(c, aiprotocol.ErrInvalid)
		return
	}
	if body.Profile != "" && body.Profile != h.Profile.ID {
		h.fail(c, aiprotocol.ErrInvalid)
		return
	}
	job, effective, err := core.RequestReprocess(c.Request.Context(), h.DB, owner, id, c.GetHeader("Idempotency-Key"), body.Targets, h.Profile)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(202, gin.H{"id": job.ID, "job_id": job.ID, "status": job.Status, "task_type": job.TaskType, "requested_targets": effective, "poll_after_ms": 2000})
}

func (h *HTTP) job(c *gin.Context) {
	owner, _ := httpapi.PrincipalID(c)
	id, err := uuid.Parse(c.Param("taskId"))
	if err != nil {
		h.fail(c, status.Error(codes.NotFound, "job unavailable"))
		return
	}
	var job core.Job
	if err := h.DB.WithContext(c.Request.Context()).Where("id=? AND owner_id=?", id, owner).Take(&job).Error; err != nil {
		h.fail(c, status.Error(codes.NotFound, "job unavailable"))
		return
	}
	var request core.JobRequest
	_ = h.DB.WithContext(c.Request.Context()).Where("job_id=?", id).Find(&request).Error
	c.JSON(200, gin.H{"id": job.ID, "status": job.Status, "progress": job.Progress, "task_type": job.TaskType, "error_code": job.ErrorCode, "error_message": job.ErrorMessage, "requested_targets": request.RequestedTargets, "capability_outcomes": request.CapabilityOutcomes, "result_version_refs": request.ResultVersionRefs, "poll_after_ms": 2000})
}
