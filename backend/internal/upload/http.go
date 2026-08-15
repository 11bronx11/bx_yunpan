package upload

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/httpapi"
)

type HTTP struct {
	service      *Service
	authenticate gin.HandlerFunc
}

func NewHTTP(service *Service, authenticate gin.HandlerFunc) *HTTP {
	return &HTTP{service: service, authenticate: authenticate}
}

func (h *HTTP) RegisterRoutes(v1 *gin.RouterGroup) {
	uploads := v1.Group("/uploads", h.authenticate)
	uploads.POST("", h.create)
	uploads.GET("", h.list)
	uploads.GET("/:uploadId", h.get)
	uploads.DELETE("/:uploadId", h.abort)
	uploads.POST("/:uploadId/parts/presign", h.presign)
	uploads.POST("/:uploadId/parts/confirm", h.confirm)
	uploads.POST("/:uploadId/complete", h.complete)
}

func (h *HTTP) list(c *gin.Context) {
	if status := c.Query("status"); status != "" && status != "active" {
		h.writeError(c, ErrInvalidInput)
		return
	}
	userID, ok := httpapi.PrincipalID(c)
	if !ok {
		h.writeError(c, ErrNotFound)
		return
	}
	sessions, err := h.service.ListActive(userID)
	if err != nil {
		h.writeError(c, err)
		return
	}
	items := make([]gin.H, 0, len(sessions))
	for _, session := range sessions {
		items = append(items, sessionResponse(session))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *HTTP) create(c *gin.Context) {
	var request struct {
		FolderID  uuid.UUID `json:"folder_id" binding:"required"`
		Filename  string    `json:"filename" binding:"required"`
		SHA256    string    `json:"sha256" binding:"required"`
		SizeBytes int64     `json:"size_bytes" binding:"required"`
		MimeType  string    `json:"mime_type" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		h.writeError(c, ErrInvalidInput)
		return
	}
	userID, _ := httpapi.PrincipalID(c)
	result, err := h.service.Create(c.Request.Context(), userID, CreateInput{
		FolderID: request.FolderID, Filename: request.Filename, SHA256: request.SHA256,
		SizeBytes: request.SizeBytes, MimeType: request.MimeType, IdempotencyKey: c.GetHeader("Idempotency-Key"),
	})
	if err != nil {
		h.writeError(c, err)
		return
	}
	if result.Mode == "instant" {
		c.JSON(http.StatusCreated, gin.H{"mode": "instant", "file": drive.FileResponse(*result.File)})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"mode": "multipart", "upload": sessionResponse(*result.Upload)})
}

func (h *HTTP) get(c *gin.Context) {
	userID, uploadID, ok := uploadParams(c)
	if !ok {
		h.writeError(c, ErrNotFound)
		return
	}
	session, err := h.service.Get(userID, uploadID)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, sessionResponse(session))
}

func (h *HTTP) presign(c *gin.Context) {
	var request struct {
		PartNumbers []int `json:"part_numbers" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || len(request.PartNumbers) == 0 || len(request.PartNumbers) > 20 {
		h.writeError(c, ErrInvalidInput)
		return
	}
	userID, uploadID, ok := uploadParams(c)
	if !ok {
		h.writeError(c, ErrNotFound)
		return
	}
	urls, expiresAt, err := h.service.PresignParts(c.Request.Context(), userID, uploadID, request.PartNumbers)
	if err != nil {
		h.writeError(c, err)
		return
	}
	parts := make([]gin.H, 0, len(urls))
	for number, value := range urls {
		parts = append(parts, gin.H{"part_number": number, "url": value, "expires_at": expiresAt})
	}
	c.JSON(http.StatusOK, gin.H{"parts": parts})
}

func (h *HTTP) confirm(c *gin.Context) {
	var request struct {
		Parts []struct {
			PartNumber int     `json:"part_number"`
			ETag       string  `json:"etag"`
			SizeBytes  int64   `json:"size_bytes"`
			Checksum   *string `json:"checksum"`
		} `json:"parts" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || len(request.Parts) == 0 || len(request.Parts) > 100 {
		h.writeError(c, ErrInvalidInput)
		return
	}
	parts := make([]ConfirmedPart, 0, len(request.Parts))
	for _, part := range request.Parts {
		parts = append(parts, ConfirmedPart{PartNumber: part.PartNumber, ETag: part.ETag, SizeBytes: part.SizeBytes, Checksum: part.Checksum})
	}
	userID, uploadID, ok := uploadParams(c)
	if !ok {
		h.writeError(c, ErrNotFound)
		return
	}
	session, err := h.service.ConfirmParts(userID, uploadID, parts)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, sessionResponse(session))
}

func (h *HTTP) complete(c *gin.Context) {
	userID, uploadID, ok := uploadParams(c)
	if !ok {
		h.writeError(c, ErrNotFound)
		return
	}
	session, err := h.service.Complete(c.Request.Context(), userID, uploadID)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, sessionResponse(session))
}

func (h *HTTP) abort(c *gin.Context) {
	userID, uploadID, ok := uploadParams(c)
	if !ok {
		h.writeError(c, ErrNotFound)
		return
	}
	if err := h.service.Abort(c.Request.Context(), userID, uploadID); err != nil {
		h.writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *HTTP) writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		c.JSON(http.StatusNotFound, httpapi.ErrorEnvelope(c, "upload.not_found", "upload not found"))
	case errors.Is(err, ErrQuotaExceeded):
		c.JSON(http.StatusTooManyRequests, httpapi.ErrorEnvelope(c, "quota.exceeded", "storage quota exceeded"))
	case errors.Is(err, ErrFileExists), errors.Is(err, drive.ErrConflict):
		c.JSON(http.StatusConflict, httpapi.ErrorEnvelope(c, "upload.file_exists", "file already exists in drive"))
	case errors.Is(err, ErrConflict):
		c.JSON(http.StatusConflict, httpapi.ErrorEnvelope(c, "upload.conflict", "upload state or file name conflict"))
	case errors.Is(err, ErrInvalidInput):
		c.JSON(http.StatusUnprocessableEntity, httpapi.ErrorEnvelope(c, "upload.invalid_input", "invalid upload request"))
	default:
		c.JSON(http.StatusInternalServerError, httpapi.ErrorEnvelope(c, "internal.error", "internal server error"))
	}
}

func uploadParams(c *gin.Context) (uuid.UUID, uuid.UUID, bool) {
	userID, ok := httpapi.PrincipalID(c)
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	uploadID, err := uuid.Parse(c.Param("uploadId"))
	return userID, uploadID, err == nil
}

func sessionResponse(session Session) gin.H {
	confirmedParts := make([]gin.H, 0, len(session.ConfirmedParts))
	for _, part := range session.ConfirmedParts {
		confirmedParts = append(confirmedParts, gin.H{
			"part_number":  part.PartNumber,
			"size_bytes":   part.SizeBytes,
			"completed_at": part.CompletedAt,
		})
	}
	return gin.H{
		"id":                  session.ID,
		"status":              session.Status,
		"folder_id":           session.FolderID,
		"filename":            session.Filename,
		"sha256":              session.DeclaredSHA256,
		"size_bytes":          session.SizeBytes,
		"part_size":           session.PartSize,
		"part_count":          session.PartCount,
		"confirmed_parts":     confirmedParts,
		"completed_object_id": session.CompletedObjectID,
		"completed_entry_id":  session.CompletedEntryID,
		"error_code":          session.ErrorCode,
		"expires_at":          session.ExpiresAt,
		"created_at":          session.CreatedAt,
	}
}
