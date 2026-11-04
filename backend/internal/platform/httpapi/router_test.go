package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
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

func TestReadinessReportsAdvisoryFailureWithoutRemovingAPI(t *testing.T) {
	router := NewRouter(RouterConfig{
		Environment:  "test",
		ProbeTimeout: time.Second,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:      NewMetrics(),
		AdvisoryProbes: []health.Probe{
			health.ProbeFunc{ProbeName: "redis_cache", Func: func(context.Context) error { return errors.New("offline") }},
		},
	})
	request := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	if !bytes.Contains(response.Body.Bytes(), []byte(`"redis_cache":"degraded"`)) {
		t.Fatalf("missing degraded cache state: %s", response.Body.String())
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

func TestConcurrentRequestLimitRejectsInsteadOfQueueing(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	router := gin.New()
	router.GET("/limited", LimitConcurrentRequests(1), func(c *gin.Context) {
		close(entered)
		<-release
		c.Status(http.StatusNoContent)
	})

	firstResponse := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		router.ServeHTTP(firstResponse, httptest.NewRequest(http.MethodGet, "/limited", nil))
	}()
	<-entered

	secondResponse := httptest.NewRecorder()
	router.ServeHTTP(secondResponse, httptest.NewRequest(http.MethodGet, "/limited", nil))
	if secondResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("second request status=%d, want=%d", secondResponse.Code, http.StatusServiceUnavailable)
	}
	if secondResponse.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After=%q, want 1", secondResponse.Header().Get("Retry-After"))
	}
	if !bytes.Contains(secondResponse.Body.Bytes(), []byte(`"service.overloaded"`)) {
		t.Fatalf("missing overload error code: %s", secondResponse.Body.String())
	}

	releaseOnce.Do(func() { close(release) })
	<-firstDone
	if firstResponse.Code != http.StatusNoContent {
		t.Fatalf("first request status=%d, want=%d", firstResponse.Code, http.StatusNoContent)
	}
}

func TestRouterPropagatesOutboxFeatureFlagsToRequests(t *testing.T) {
	router := NewRouter(RouterConfig{
		Environment: "test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: NewMetrics(),
		V2EventsEnabled: true, SearchEnabled: true,
		Registrars: []func(*gin.RouterGroup){func(v1 *gin.RouterGroup) {
			v1.GET("/flags", func(c *gin.Context) {
				ctx := c.Request.Context()
				if !outbox.V2EventsEnabled(ctx) || !outbox.SearchEnabled(ctx) {
					c.Status(http.StatusInternalServerError)
					return
				}
				c.Status(http.StatusNoContent)
			})
		}},
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/flags", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("feature flags were not propagated, status=%d", response.Code)
	}
}
