package drive

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/httpapi"
)

type FileHTTP struct {
	manager      *FileManager
	authenticate gin.HandlerFunc
}

func NewFileHTTP(manager *FileManager, authenticate gin.HandlerFunc) *FileHTTP {
	return &FileHTTP{manager: manager, authenticate: authenticate}
}

func (h *FileHTTP) RegisterRoutes(v1 *gin.RouterGroup) {
	files := v1.Group("/files", h.authenticate)
	files.GET("/:fileId", h.get)
	files.PATCH("/:fileId", h.rename)
	files.POST("/:fileId/move", h.move)
	files.DELETE("/:fileId", h.delete)
	files.GET("/:fileId/download-url", h.download)
	files.GET("/:fileId/preview", h.preview)
}

func (h *FileHTTP) get(c *gin.Context) {
	ownerID, fileID, _, err := fileParams(c, false)
	if err != nil {
		new(HTTP).writeError(c, err)
		return
	}
	file, err := h.manager.Get(ownerID, fileID)
	if err != nil {
		new(HTTP).writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, FileResponse(file))
}

func (h *FileHTTP) rename(c *gin.Context) {
	var request struct {
		Name string `json:"name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		new(HTTP).writeError(c, ErrInvalidInput)
		return
	}
	ownerID, fileID, version, err := fileParams(c, true)
	if err != nil {
		new(HTTP).writeError(c, err)
		return
	}
	file, err := h.manager.Rename(ownerID, fileID, version, request.Name)
	if err != nil {
		new(HTTP).writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, FileResponse(file))
}

func (h *FileHTTP) move(c *gin.Context) {
	var request struct {
		TargetFolderID uuid.UUID `json:"target_folder_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		new(HTTP).writeError(c, ErrInvalidInput)
		return
	}
	ownerID, fileID, version, err := fileParams(c, true)
	if err != nil {
		new(HTTP).writeError(c, err)
		return
	}
	file, err := h.manager.Move(ownerID, fileID, request.TargetFolderID, version)
	if err != nil {
		new(HTTP).writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, FileResponse(file))
}

func (h *FileHTTP) delete(c *gin.Context) {
	ownerID, fileID, version, err := fileParams(c, true)
	if err != nil {
		new(HTTP).writeError(c, err)
		return
	}
	if err := h.manager.Delete(ownerID, fileID, version); err != nil {
		new(HTTP).writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *FileHTTP) download(c *gin.Context) {
	ownerID, fileID, _, err := fileParams(c, false)
	if err != nil {
		new(HTTP).writeError(c, err)
		return
	}
	value, expiresAt, err := h.manager.DownloadURL(c.Request.Context(), ownerID, fileID)
	if err != nil {
		new(HTTP).writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": value, "expires_at": expiresAt})
}

func (h *FileHTTP) preview(c *gin.Context) {
	ownerID, fileID, _, err := fileParams(c, false)
	if err != nil {
		new(HTTP).writeError(c, err)
		return
	}
	kind, value, expiresAt, err := h.manager.Preview(c.Request.Context(), ownerID, fileID)
	if err != nil {
		new(HTTP).writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"kind": kind, "url": value, "expires_at": expiresAt})
}

func fileParams(c *gin.Context, requireVersion bool) (uuid.UUID, uuid.UUID, int64, error) {
	ownerID, ok := httpapi.PrincipalID(c)
	if !ok {
		return uuid.Nil, uuid.Nil, 0, ErrNotFound
	}
	fileID, err := uuid.Parse(c.Param("fileId"))
	if err != nil {
		return uuid.Nil, uuid.Nil, 0, ErrNotFound
	}
	if !requireVersion {
		return ownerID, fileID, 0, nil
	}
	version, err := strconv.ParseInt(c.GetHeader("If-Match"), 10, 64)
	if err != nil || version <= 0 {
		return uuid.Nil, uuid.Nil, 0, ErrInvalidInput
	}
	return ownerID, fileID, version, nil
}
