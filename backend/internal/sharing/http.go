package sharing

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/httpapi"
)

type HTTP struct {
	service      *Service
	files        *drive.FileManager
	authenticate gin.HandlerFunc
}

func NewHTTP(service *Service, files *drive.FileManager, authenticate gin.HandlerFunc) *HTTP {
	return &HTTP{service: service, files: files, authenticate: authenticate}
}

func (h *HTTP) RegisterRoutes(v1 *gin.RouterGroup) {
	shares := v1.Group("/shares", h.authenticate)
	shares.GET("", h.list)
	shares.POST("", h.create)
	shares.GET("/:shareId", h.get)
	shares.DELETE("/:shareId", h.revoke)
	shares.POST("/:shareId/import", h.importShare)

	public := v1.Group("/public/shares")
	public.POST("/resolve", h.resolve)
	public.GET("/content", h.content)
}

func (h *HTTP) create(c *gin.Context) {
	var request struct {
		FileID    uuid.UUID  `json:"file_id" binding:"required"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		h.writeError(c, ErrConflict)
		return
	}
	ownerID, _ := httpapi.PrincipalID(c)
	share, key, err := h.service.Create(ownerID, request.FileID, request.ExpiresAt)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"share": shareResponse(share), "share_key": key})
}

func (h *HTTP) list(c *gin.Context) {
	ownerID, _ := httpapi.PrincipalID(c)
	shares, err := h.service.List(ownerID)
	if err != nil {
		h.writeError(c, err)
		return
	}
	items := make([]gin.H, 0, len(shares))
	for _, share := range shares {
		items = append(items, shareResponse(share))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "page": gin.H{"has_more": false}})
}

func (h *HTTP) get(c *gin.Context) {
	ownerID, _ := httpapi.PrincipalID(c)
	shareID, err := uuid.Parse(c.Param("shareId"))
	if err != nil {
		h.writeError(c, ErrNotFound)
		return
	}
	share, err := h.service.Get(ownerID, shareID)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, shareResponse(share))
}

func (h *HTTP) revoke(c *gin.Context) {
	ownerID, _ := httpapi.PrincipalID(c)
	shareID, err := uuid.Parse(c.Param("shareId"))
	if err != nil || h.service.Revoke(ownerID, shareID) != nil {
		h.writeError(c, ErrNotFound)
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
		"expires_in":         int(time.Until(expiresAt).Seconds()),
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
	kind, value, expiresAt, err := h.files.Preview(c.Request.Context(), share.OwnerID, file.ID)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"share":   publicShareResponse(share, file),
		"preview": gin.H{"kind": kind, "url": value, "expires_at": expiresAt},
	})
}

func (h *HTTP) importShare(c *gin.Context) {
	var request struct {
		TargetFolderID uuid.UUID `json:"target_folder_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		h.writeError(c, ErrConflict)
		return
	}
	userID, _ := httpapi.PrincipalID(c)
	shareID, err := uuid.Parse(c.Param("shareId"))
	if err != nil {
		h.writeError(c, ErrNotFound)
		return
	}
	file, err := h.service.Import(userID, shareID, request.TargetFolderID, c.GetHeader("Idempotency-Key"), c.GetHeader("X-Share-Token"))
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"share_id": shareID, "imported_file": drive.FileResponse(file)})
}

func (h *HTTP) writeError(c *gin.Context, err error) {
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
	}
}

func publicShareResponse(share Share, file drive.FileView) gin.H {
	return gin.H{
		"id": share.ID, "owner_display_name": "BX YunPan user", "file_name": file.Name,
		"size_bytes": file.SizeBytes, "mime_type": file.MimeType, "expires_at": share.ExpiresAt,
	}
}
