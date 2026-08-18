package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/grpcx"
)

const requestIDKey = "request_id"
const principalIDKey = "principal_id"

func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader("X-Request-ID")
		if requestID == "" {
			id, err := uuid.NewV7()
			if err != nil {
				id = uuid.New()
			}
			requestID = id.String()
		}
		c.Set(requestIDKey, requestID)
		c.Request = c.Request.WithContext(grpcx.WithRequestID(c.Request.Context(), requestID))
		c.Header("X-Request-ID", requestID)
		c.Next()
	}
}

func AccessLog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		c.Next()

		logger.InfoContext(c.Request.Context(), "http request",
			"request_id", RequestIDValue(c),
			"method", c.Request.Method,
			"route", c.FullPath(),
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(started).Milliseconds(),
		)
	}
}

func Recovery(logger *slog.Logger) gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered any) {
		logger.ErrorContext(c.Request.Context(), "panic recovered",
			"request_id", RequestIDValue(c),
			"panic", recovered,
		)
		c.AbortWithStatusJSON(http.StatusInternalServerError, ErrorEnvelope(c, "internal.error", "internal server error"))
	})
}

func LimitRequestBody(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if maxBytes > 0 && c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		}
		c.Next()
	}
}

func RequestIDValue(c *gin.Context) string {
	value, _ := c.Get(requestIDKey)
	requestID, _ := value.(string)
	return requestID
}

func ErrorEnvelope(c *gin.Context, code, message string) gin.H {
	return gin.H{
		"error": gin.H{
			"code":    code,
			"message": message,
		},
		"request_id": RequestIDValue(c),
	}
}

func SetPrincipalID(c *gin.Context, userID uuid.UUID) {
	c.Set(principalIDKey, userID)
}

func PrincipalID(c *gin.Context) (uuid.UUID, bool) {
	value, ok := c.Get(principalIDKey)
	if !ok {
		return uuid.Nil, false
	}
	userID, ok := value.(uuid.UUID)
	return userID, ok
}
