package ai

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/httpapi"
)

type HTTP struct {
	service      *Service
	authenticate gin.HandlerFunc
	limiter      RequestLimiter
}

func NewHTTP(service *Service, authenticate gin.HandlerFunc, limiter RequestLimiter) *HTTP {
	return &HTTP{service: service, authenticate: authenticate, limiter: limiter}
}

func (h *HTTP) RegisterRoutes(v1 *gin.RouterGroup) {
	v1.POST("/search", h.authenticate, h.limitBody(), h.rateLimit("search"), h.search)
	files := v1.Group("/files", h.authenticate)
	files.GET("/:fileId/ai", h.getDocument)
	files.POST("/:fileId/ai/reprocess", h.rateLimit("reprocess"), h.reprocess)
	ai := v1.Group("/ai", h.authenticate)
	ai.POST("/ask", h.limitBody(), h.rateLimit("ask"), h.ask)
	ai.GET("/jobs/:taskId", h.getTask)
}

func (h *HTTP) limitBody() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
		c.Next()
	}
}

func (h *HTTP) rateLimit(scope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ownerID, ok := httpapi.PrincipalID(c)
		if !ok || h.limiter == nil {
			c.Next()
			return
		}
		allowed, err := h.limiter.Allow(c.Request.Context(), ownerID, scope)
		if err != nil {
			c.Next()
			return
		}
		if !allowed {
			c.Header("Retry-After", "60")
			c.AbortWithStatusJSON(http.StatusTooManyRequests, httpapi.ErrorEnvelope(c, "ai.rate_limited", "AI request rate limit exceeded"))
			return
		}
		c.Next()
	}
}

func (h *HTTP) search(c *gin.Context) {
	var request struct {
		Query             string     `json:"query" binding:"required"`
		Mode              string     `json:"mode" binding:"required"`
		FolderID          *uuid.UUID `json:"folder_id"`
		IncludeSubfolders *bool      `json:"include_subfolders"`
		MimeTypes         []string   `json:"mime_types"`
		Limit             int        `json:"limit"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		h.writeError(c, ErrInvalid)
		return
	}
	includeSubfolders := true
	if request.IncludeSubfolders != nil {
		includeSubfolders = *request.IncludeSubfolders
	}
	ownerID, _ := httpapi.PrincipalID(c)
	hits, err := h.service.Search(c.Request.Context(), ownerID, SearchInput{
		Query: request.Query, Mode: request.Mode, FolderID: request.FolderID, IncludeSubfolders: includeSubfolders,
		MimeTypes: request.MimeTypes, Limit: request.Limit,
	})
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"hits": hits, "page": gin.H{"has_more": false}})
}

func (h *HTTP) getDocument(c *gin.Context) {
	ownerID, _ := httpapi.PrincipalID(c)
	fileID, err := uuid.Parse(c.Param("fileId"))
	if err != nil {
		h.writeError(c, ErrNotFound)
		return
	}
	file, document, err := h.service.GetDocument(ownerID, fileID)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"file_id": file.ID, "status": document.Status, "summary": document.Summary, "tags": []string(document.Tags),
		"language": document.Language, "pipeline_version": document.PipelineVersion, "model_version": document.ModelVersion,
		"error_code": document.ErrorCode,
	})
}

func (h *HTTP) reprocess(c *gin.Context) {
	ownerID, _ := httpapi.PrincipalID(c)
	fileID, err := uuid.Parse(c.Param("fileId"))
	if err != nil {
		h.writeError(c, ErrNotFound)
		return
	}
	task, err := h.service.RequestReprocess(ownerID, fileID, c.GetHeader("Idempotency-Key"))
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, taskResponse(task))
}

func (h *HTTP) ask(c *gin.Context) {
	var request struct {
		Question string      `json:"question" binding:"required"`
		FolderID *uuid.UUID  `json:"folder_id"`
		FileIDs  []uuid.UUID `json:"file_ids"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || len(request.FileIDs) > 50 {
		h.writeError(c, ErrInvalid)
		return
	}
	ownerID, _ := httpapi.PrincipalID(c)
	answer, citations, err := h.service.Ask(c.Request.Context(), ownerID, request.Question, request.FolderID, request.FileIDs)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"answer": answer, "citations": citations})
}

func (h *HTTP) getTask(c *gin.Context) {
	ownerID, _ := httpapi.PrincipalID(c)
	taskID, err := uuid.Parse(c.Param("taskId"))
	if err != nil {
		h.writeError(c, ErrNotFound)
		return
	}
	task, err := h.service.GetTask(ownerID, taskID)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, taskResponse(task))
}

func (h *HTTP) writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		c.JSON(http.StatusNotFound, httpapi.ErrorEnvelope(c, "ai.not_found", "AI resource not found"))
	case errors.Is(err, ErrInvalid):
		c.JSON(http.StatusUnprocessableEntity, httpapi.ErrorEnvelope(c, "ai.invalid_request", "invalid AI request"))
	case errors.Is(err, ErrQuota):
		c.JSON(http.StatusTooManyRequests, httpapi.ErrorEnvelope(c, "ai.quota_exhausted", "AI provider quota exhausted"))
	case errors.Is(err, ErrUnavailable):
		c.JSON(http.StatusServiceUnavailable, httpapi.ErrorEnvelope(c, "ai.unavailable", "AI provider unavailable"))
	default:
		c.JSON(http.StatusInternalServerError, httpapi.ErrorEnvelope(c, "internal.error", "internal server error"))
	}
}

func taskResponse(task Task) gin.H {
	return gin.H{
		"id": task.ID, "task_type": task.TaskType, "resource_type": nullableResponse(task.ResourceType), "resource_id": task.ResourceID,
		"status": task.Status, "progress": task.Progress, "attempt": task.Attempt, "error_code": task.ErrorCode,
		"error_message": task.ErrorMessage, "created_at": task.CreatedAt, "updated_at": task.UpdatedAt,
	}
}

func nullableResponse(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
