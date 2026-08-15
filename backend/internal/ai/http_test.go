package ai

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/httpapi"
)

type rejectingLimiter struct{}

func (rejectingLimiter) Allow(context.Context, uuid.UUID, string) (bool, error) {
	return false, nil
}

func TestAIRateLimitReturnsStructuredError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHTTP(nil, nil, rejectingLimiter{})
	router := httpapi.NewRouter(httpapi.RouterConfig{
		Environment: "test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: httpapi.NewMetrics(),
		Registrars: []func(*gin.RouterGroup){func(v1 *gin.RouterGroup) {
			v1.POST("/limited", func(c *gin.Context) {
				httpapi.SetPrincipalID(c, uuid.New())
				c.Next()
			}, handler.rateLimit("search"), func(c *gin.Context) { c.Status(http.StatusNoContent) })
		}},
	})

	request := httptest.NewRequest(http.MethodPost, "/api/v1/limited", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", response.Code)
	}
	if response.Header().Get("Retry-After") != "60" {
		t.Fatal("missing Retry-After header")
	}
}
