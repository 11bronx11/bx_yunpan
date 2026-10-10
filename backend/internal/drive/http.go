package drive

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

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
	folders := v1.Group("/folders", h.authenticate)
	folders.GET("/root", h.root)
	folders.GET("/:folderId/children", h.children)
	folders.GET("/:folderId/breadcrumb", h.breadcrumb)
	folders.POST("", h.create)
	folders.PATCH("/:folderId", h.rename)
	folders.POST("/:folderId/move", h.move)
	folders.DELETE("/:folderId", h.delete)
}

func (h *HTTP) root(c *gin.Context) {
	ownerID, _ := httpapi.PrincipalID(c)
	folder, err := h.service.Root(ownerID)
	h.writeFolder(c, http.StatusOK, folder, err)
}

func (h *HTTP) children(c *gin.Context) {
	ownerID, _ := httpapi.PrincipalID(c)
	folderID, err := uuid.Parse(c.Param("folderId"))
	if err != nil {
		h.writeError(c, ErrNotFound)
		return
	}
	limit := 50
	if rawLimit := c.Query("limit"); rawLimit != "" {
		parsed, parseErr := strconv.Atoi(rawLimit)
		if parseErr != nil || parsed < 1 || parsed > 100 {
			h.writeError(c, ErrInvalidInput)
			return
		}
		limit = parsed
	}
	folders, files, hasMore, nextCursor, err := h.service.ChildrenPage(c.Request.Context(), ownerID, folderID, c.Query("cursor"), limit, c.Query("sort"))
	if err != nil {
		h.writeError(c, err)
		return
	}
	items := make([]gin.H, 0, len(folders)+len(files))
	for _, folder := range folders {
		items = append(items, folderResponse(folder))
	}
	for _, file := range files {
		items = append(items, FileResponse(file))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "page": gin.H{"has_more": hasMore, "next_cursor": nextCursor}})
}

func (h *HTTP) breadcrumb(c *gin.Context) {
	ownerID, _ := httpapi.PrincipalID(c)
	folderID, err := uuid.Parse(c.Param("folderId"))
	if err != nil {
		h.writeError(c, ErrNotFound)
		return
	}
	folders, err := h.service.Breadcrumb(ownerID, folderID)
	if err != nil {
		h.writeError(c, err)
		return
	}
	items := make([]gin.H, 0, len(folders))
	for _, folder := range folders {
		items = append(items, folderResponse(folder))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *HTTP) create(c *gin.Context) {
	var request struct {
		ParentID uuid.UUID `json:"parent_id" binding:"required"`
		Name     string    `json:"name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		h.writeError(c, ErrInvalidInput)
		return
	}
	ownerID, _ := httpapi.PrincipalID(c)
	folder, err := h.service.CreateWithContext(c.Request.Context(), ownerID, request.ParentID, request.Name)
	h.writeFolder(c, http.StatusCreated, folder, err)
}

func (h *HTTP) rename(c *gin.Context) {
	var request struct {
		Name string `json:"name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		h.writeError(c, ErrInvalidInput)
		return
	}
	ownerID, folderID, version, err := resourceParams(c)
	if err != nil {
		h.writeError(c, err)
		return
	}
	folder, err := h.service.RenameWithContext(c.Request.Context(), ownerID, folderID, version, request.Name)
	h.writeFolder(c, http.StatusOK, folder, err)
}

func (h *HTTP) move(c *gin.Context) {
	var request struct {
		TargetFolderID uuid.UUID `json:"target_folder_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		h.writeError(c, ErrInvalidInput)
		return
	}
	ownerID, folderID, version, err := resourceParams(c)
	if err != nil {
		h.writeError(c, err)
		return
	}
	folder, err := h.service.MoveWithContext(c.Request.Context(), ownerID, folderID, request.TargetFolderID, version)
	h.writeFolder(c, http.StatusOK, folder, err)
}

func (h *HTTP) delete(c *gin.Context) {
	ownerID, folderID, version, err := resourceParams(c)
	if err != nil {
		h.writeError(c, err)
		return
	}
	if err := h.service.DeleteWithContext(c.Request.Context(), ownerID, folderID, version); err != nil {
		h.writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *HTTP) writeFolder(c *gin.Context, status int, folder Folder, err error) {
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(status, folderResponse(folder))
}

func (h *HTTP) writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		c.JSON(http.StatusNotFound, httpapi.ErrorEnvelope(c, "drive.not_found", "resource not found"))
	case errors.Is(err, ErrNotEmpty):
		c.JSON(http.StatusConflict, httpapi.ErrorEnvelope(c, "folder.not_empty", "folder is not empty"))
	case errors.Is(err, ErrConflict):
		c.JSON(http.StatusConflict, httpapi.ErrorEnvelope(c, "drive.conflict", "resource changed or name already exists"))
	case errors.Is(err, ErrInvalidInput):
		c.JSON(http.StatusUnprocessableEntity, httpapi.ErrorEnvelope(c, "drive.invalid_input", "invalid folder operation"))
	case errors.Is(err, ErrInvalidCursor):
		c.JSON(http.StatusUnprocessableEntity, httpapi.ErrorEnvelope(c, "drive.invalid_cursor", "invalid directory cursor"))
	case errors.Is(err, ErrCursorStale):
		c.JSON(http.StatusConflict, httpapi.ErrorEnvelope(c, "drive.cursor_stale", "directory changed; restart pagination"))
	default:
		c.JSON(http.StatusInternalServerError, httpapi.ErrorEnvelope(c, "internal.error", "internal server error"))
	}
}

func resourceParams(c *gin.Context) (uuid.UUID, uuid.UUID, int64, error) {
	ownerID, ok := httpapi.PrincipalID(c)
	if !ok {
		return uuid.Nil, uuid.Nil, 0, ErrNotFound
	}
	resourceID, err := uuid.Parse(c.Param("folderId"))
	if err != nil {
		return uuid.Nil, uuid.Nil, 0, ErrNotFound
	}
	version, err := strconv.ParseInt(c.GetHeader("If-Match"), 10, 64)
	if err != nil || version <= 0 {
		return uuid.Nil, uuid.Nil, 0, ErrInvalidInput
	}
	return ownerID, resourceID, version, nil
}

func folderResponse(folder Folder) gin.H {
	return gin.H{
		"type":       "folder",
		"id":         folder.ID,
		"owner_id":   folder.OwnerID,
		"parent_id":  folder.ParentID,
		"name":       folder.Name,
		"version":    folder.Version,
		"created_at": folder.CreatedAt,
		"updated_at": folder.UpdatedAt,
	}
}

func FileResponse(file FileView) gin.H {
	response := gin.H{
		"type":       "file",
		"id":         file.ID,
		"owner_id":   file.OwnerID,
		"folder_id":  file.FolderID,
		"object_id":  file.ObjectID,
		"name":       file.Name,
		"size_bytes": file.SizeBytes,
		"mime_type":  file.MimeType,
		"version":    file.Version,
		"created_at": file.CreatedAt,
		"updated_at": file.UpdatedAt,
	}
	if file.AIStatus != nil {
		response["ai_status"] = *file.AIStatus
	}
	return response
}
