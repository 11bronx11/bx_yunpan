package ai

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/httpapi"
)

// 限流已下移到 aisvc，API 侧现在是把 gRPC 返回的 ErrRateLimited 还原后
// 映射为 429 + Retry-After。这里断言映射表在 HTTP 边界上的行为不变。
func TestRateLimitedMapsToTooManyRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHTTP(nil, nil, nil)
	router := httpapi.NewRouter(httpapi.RouterConfig{
		Environment: "test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: httpapi.NewMetrics(),
		Registrars: []func(*gin.RouterGroup){func(v1 *gin.RouterGroup) {
			v1.POST("/limited", func(c *gin.Context) {
				httpapi.SetPrincipalID(c, uuid.New())
				handler.writeError(c, ErrRateLimited)
			})
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

// 三层映射表是错误码的唯一来源，逐条断言 gRPC 往返后 HTTP 状态码不漂移。
func TestErrorMappingRoundTripsThroughGRPC(t *testing.T) {
	for _, mapping := range ErrorMappings() {
		restored := FromGRPCError(GRPCStatus(mapping.Err).Err())
		status, code, _ := HTTPStatus(restored)
		if code != mapping.AppCode {
			t.Errorf("%s: round-tripped app code = %s", mapping.AppCode, code)
		}
		if status != mapping.Status {
			t.Errorf("%s: round-tripped HTTP status = %d, want %d", mapping.AppCode, status, mapping.Status)
		}
	}
}
