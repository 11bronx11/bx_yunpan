package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/health"
)

func TestReadinessFailsWhenDependencyIsUnavailable(t *testing.T) {
	router := NewRouter(RouterConfig{
		Environment:  "test",
		ProbeTimeout: time.Second,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:      NewMetrics(),
		Probes: []health.Probe{
			health.ProbeFunc{ProbeName: "postgres", Func: func(context.Context) error { return errors.New("offline") }},
		},
	})

	request := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", response.Code)
	}
	if response.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing request id")
	}
}

func TestRouterLimitsRequestBody(t *testing.T) {
	router := NewRouter(RouterConfig{
		Environment:  "test",
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:      NewMetrics(),
		MaxBodyBytes: 8,
		Registrars: []func(*gin.RouterGroup){func(v1 *gin.RouterGroup) {
			v1.POST("/body", func(c *gin.Context) {
				_, err := io.ReadAll(c.Request.Body)
				if err != nil {
					c.Status(http.StatusRequestEntityTooLarge)
					return
				}
				c.Status(http.StatusNoContent)
			})
		}},
	})

	request := httptest.NewRequest(http.MethodPost, "/api/v1/body", bytes.NewBufferString("123456789"))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", response.Code)
	}
}
