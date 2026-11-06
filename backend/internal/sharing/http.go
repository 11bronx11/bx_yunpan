package sharing

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"

	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/httpapi"
)

type HTTP struct {
	service      *Service
	files        *drive.FileManager
	authenticate gin.HandlerFunc
	storage      *minio.Client
}

func (h *HTTP) WithStorage(storage *minio.Client) *HTTP {
	h.storage = storage
	return h
}

func NewHTTP(service *Service, files *drive.FileManager, authenticate gin.HandlerFunc) *HTTP {
	return &HTTP{service: service, files: files, authenticate: authenticate}
}

func (h *HTTP) RegisterRoutes(v1 *gin.RouterGroup) {
	shares := v1.Group("/shares", h.authenticate)
	shares.GET("", h.list)
	shares.POST("", h.create)
	shares.GET("/:shareId", h.get)
	shares.PATCH("/:shareId", h.update)
	shares.DELETE("/:shareId", h.revoke)
	shares.POST("/:shareId/import", h.importShare)

	public := v1.Group("/public/shares")
	public.POST("/resolve", h.resolve)
	public.GET("/content", h.content)
	public.GET("/file", h.file)
	public.HEAD("/file", h.file)
}

func (h *HTTP) create(c *gin.Context) {
	var request struct {
		FileID    uuid.UUID  `json:"file_id" binding:"required"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		h.writeError(c, ErrInvalidInput)
		return
	}
	ownerID, _ := httpapi.PrincipalID(c)
	share, key, err := h.service.CreateWithContext(c.Request.Context(), ownerID, request.FileID, request.ExpiresAt)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"share": shareResponse(share), "share_key": key})
}

func (h *HTTP) list(c *gin.Context) {
	ownerID, _ := httpapi.PrincipalID(c)
	limit := 50
	if raw, exists := c.GetQuery("limit"); exists {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil {
			h.writeError(c, ErrInvalidInput)
			return
		}
	}
	shares, next, err := h.service.ListPage(c.Request.Context(), ownerID, c.Query("cursor"), limit)
	if err != nil {
		h.writeError(c, err)
		return
	}
	items := make([]gin.H, 0, len(shares))
	for _, share := range shares {
		items = append(items, shareViewResponse(share))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "page": gin.H{"has_more": next != "", "next_cursor": next}})
}

func (h *HTTP) get(c *gin.Context) {
	ownerID, _ := httpapi.PrincipalID(c)
	shareID, err := uuid.Parse(c.Param("shareId"))
	if err != nil {
		h.writeError(c, ErrNotFound)
		return
	}
	share, err := h.service.GetView(c.Request.Context(), ownerID, shareID)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, shareViewResponse(share))
}

func (h *HTTP) update(c *gin.Context) {
	var request struct {
		Version int64 `json:"version" binding:"required"`
		// RawMessage distinguishes an explicit null (no expiry) from omission.
		ExpiresAt json.RawMessage `json:"expires_at" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		h.writeError(c, ErrInvalidInput)
		return
	}
	var expiresAt *time.Time
	if err := json.Unmarshal(request.ExpiresAt, &expiresAt); err != nil {
		h.writeError(c, ErrInvalidInput)
		return
	}
	ownerID, _ := httpapi.PrincipalID(c)
	shareID, err := uuid.Parse(c.Param("shareId"))
	if err != nil {
		h.writeError(c, ErrNotFound)
		return
	}
	if err := h.service.UpdateExpiry(c.Request.Context(), ownerID, shareID, request.Version, expiresAt); err != nil {
		h.writeError(c, err)
		return
	}
	h.get(c)
}

func (h *HTTP) revoke(c *gin.Context) {
	ownerID, _ := httpapi.PrincipalID(c)
	shareID, err := uuid.Parse(c.Param("shareId"))
	if err != nil {
		h.writeError(c, ErrNotFound)
		return
	}
	if err := h.service.RevokeWithContext(c.Request.Context(), ownerID, shareID); err != nil {
		h.writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *HTTP) resolve(c *gin.Context) {
	var request struct {
		ShareKey string `json:"share_key" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		h.writeError(c, ErrNotFound)
		return
	}
	share, file, token, expiresAt, err := h.service.Resolve(request.ShareKey)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"share_access_token": token,
		"expires_in":         int(math.Ceil(time.Until(expiresAt).Seconds())),
		"share":              publicShareResponse(share, file),
	})
}

func (h *HTTP) content(c *gin.Context) {
	token := c.GetHeader("X-Share-Token")
	share, file, err := h.service.Access(token)
	if err != nil {
		h.writeError(c, err)
		return
	}
	// Keep storage credentials behind a gate that rechecks the share for
	// every request. A revoked share must not retain a reusable S3 URL.
	kind := "unsupported"
	switch file.MimeType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		kind = "image"
	case "application/pdf":
		kind = "pdf"
	default:
		if strings.HasPrefix(file.MimeType, "text/") {
			kind = "text"
		}
	}
	value := "/api/v1/public/shares/file?" + url.Values{"access_token": {token}}.Encode()
	previewURL := ""
	if kind != "unsupported" {
		previewURL = value + "&preview=1"
	}
	var claims accessClaims
	parts := strings.Split(token, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[0])
	_ = json.Unmarshal(payload, &claims) // Access already verified the claims.
	expiresAt := time.Unix(claims.ExpiresAt, 0).UTC()
	if share.ExpiresAt != nil && share.ExpiresAt.Before(expiresAt) {
		expiresAt = *share.ExpiresAt
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"share":    publicShareResponse(share, file),
		"preview":  gin.H{"kind": kind, "url": previewURL, "expires_at": expiresAt},
		"download": gin.H{"url": value, "expires_at": expiresAt},
	})
}

func (h *HTTP) file(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	token := c.GetHeader("X-Share-Token")
	if token == "" {
		token = c.Query("access_token")
	}
	_, file, err := h.service.Access(token)
	if err != nil {
		h.writeError(c, err)
		return
	}
	object, err := h.service.objects.Get(file.ObjectID)
	if err != nil {
		h.writeError(c, err)
		return
	}
	if h.storage == nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	stream, err := h.storage.GetObject(c.Request.Context(), object.Bucket, object.ObjectKey, minio.GetObjectOptions{})
	if err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	defer stream.Close()
	info, err := stream.Stat()
	if err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	disposition, contentType := "attachment", "application/octet-stream"
	if c.Query("preview") == "1" {
		switch file.MimeType {
		case "image/jpeg", "image/png", "image/gif", "image/webp", "application/pdf":
			disposition, contentType = "inline", file.MimeType
		default:
			if strings.HasPrefix(file.MimeType, "text/") {
				disposition, contentType = "inline", "text/plain; charset=utf-8"
			}
		}
	}
	c.Header("Content-Type", contentType)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "sandbox; default-src 'none'")
	c.Header("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": file.Name}))
	http.ServeContent(c.Writer, c.Request, file.Name, info.LastModified, stream)
}

func (h *HTTP) importShare(c *gin.Context) {
	var request struct {
		TargetFolderID uuid.UUID `json:"target_folder_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		h.writeError(c, ErrInvalidInput)
		return
	}
	userID, _ := httpapi.PrincipalID(c)
	shareID, err := uuid.Parse(c.Param("shareId"))
	if err != nil {
		h.writeError(c, ErrNotFound)
		return
	}
	file, err := h.service.Import(c.Request.Context(), userID, shareID, request.TargetFolderID, c.GetHeader("Idempotency-Key"), c.GetHeader("X-Share-Token"))
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"share_id": shareID, "imported_file": drive.FileResponse(file)})
}

func (h *HTTP) writeError(c *gin.Context, err error) {
	if errors.Is(err, ErrQuotaExceeded) {
		c.JSON(http.StatusTooManyRequests, httpapi.ErrorEnvelope(c, "quota.exceeded", "storage quota exceeded"))
		return
	}
	if errors.Is(err, ErrVersionConflict) {
		c.JSON(http.StatusConflict, httpapi.ErrorEnvelope(c, "share.version_conflict", "share was modified"))
		return
	}
	if errors.Is(err, ErrInvalidInput) {
		c.JSON(http.StatusUnprocessableEntity, httpapi.ErrorEnvelope(c, "share.invalid_input", "invalid share request"))
		return
	}
	if errors.Is(err, ErrNotFound) {
		c.JSON(http.StatusNotFound, httpapi.ErrorEnvelope(c, "share.not_found", "share not found"))
		return
	}
	if errors.Is(err, ErrConflict) || errors.Is(err, drive.ErrConflict) {
		c.JSON(http.StatusConflict, httpapi.ErrorEnvelope(c, "share.conflict", "file already exists in drive"))
		return
	}
	c.JSON(http.StatusInternalServerError, httpapi.ErrorEnvelope(c, "internal.error", "internal server error"))
}

func shareResponse(share Share) gin.H {
	return gin.H{
		"id": share.ID, "file_id": share.FileEntryID, "expires_at": share.ExpiresAt,
		"revoked_at": share.RevokedAt, "created_at": share.CreatedAt,
		"version": share.Version,
	}
}

func shareViewResponse(view ShareView) gin.H {
	result := shareResponse(view.Share)
	status := "active"
	switch {
	case view.RevokedAt != nil:
		status = "revoked"
	case !view.FileAvailable:
		status = "unavailable"
	case view.ExpiresAt != nil && !view.ExpiresAt.After(time.Now().UTC()):
		status = "expired"
	}
	result["file_name"], result["size_bytes"], result["mime_type"] = view.FileName, view.SizeBytes, view.MimeType
	result["status"], result["file_available"] = status, view.FileAvailable
	return result
}

func publicShareResponse(share Share, file drive.FileView) gin.H {
	return gin.H{
		"id": share.ID, "owner_display_name": "BX YunPan user", "file_name": file.Name,
		"size_bytes": file.SizeBytes, "mime_type": file.MimeType, "expires_at": share.ExpiresAt,
	}
}
