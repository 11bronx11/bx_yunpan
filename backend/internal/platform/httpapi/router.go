package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	apispec "github.com/11bronx11/bx_yunpan/backend/api"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/health"
)

// tracedRequest 过滤掉探针与静态契约路由：它们由 k8s/compose 高频轮询，
// 全采会把 Jaeger 刷满且没有诊断价值。
func tracedRequest(c *gin.Context) bool {
	switch c.FullPath() {
	case "/health/live", "/health/ready", "/metrics", "/openapi.yaml", "/docs":
		return false
	default:
		return true
	}
}

type RouterConfig struct {
	Environment     string
	ServiceName     string
	ProbeTimeout    time.Duration
	MaxBodyBytes    int64
	Logger          *slog.Logger
	Probes          []health.Probe
	AdvisoryProbes  []health.Probe
	Metrics         *Metrics
	V2EventsEnabled bool
	SearchEnabled   bool
	KafkaEnabled    bool
	Readiness       *Readiness
	Registrars      []func(*gin.RouterGroup)
}

func NewRouter(cfg RouterConfig) *gin.Engine {
	if cfg.Environment == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	// otelgin 放在最前：它负责从 traceparent 头恢复上游 trace 并开出 server span，
	// 后面所有中间件与 handler 都能从 ctx 拿到这个 span 作为父节点。
	router.Use(otelgin.Middleware(cfg.ServiceName, otelgin.WithGinFilter(tracedRequest)))
	router.Use(RequestID(), outboxContext(cfg.V2EventsEnabled, cfg.SearchEnabled, cfg.KafkaEnabled), cfg.Metrics.Middleware(), AccessLog(cfg.Logger), Recovery(cfg.Logger), LimitRequestBody(cfg.MaxBodyBytes))

	router.GET("/health/live", func(c *gin.Context) {
		c.JSON(http.StatusOK, health.Result{Status: "ok"})
	})
	router.GET("/health/ready", func(c *gin.Context) {
		if !cfg.Readiness.Ready() {
			c.JSON(http.StatusServiceUnavailable, health.Result{Status: "draining"})
			return
		}
		result, err := health.Run(c.Request.Context(), cfg.ProbeTimeout, cfg.Probes)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, result)
			return
		}
		advisory, _ := health.Run(c.Request.Context(), cfg.ProbeTimeout, cfg.AdvisoryProbes)
		for name, status := range advisory.Checks {
			if status == "unavailable" {
				status = "degraded"
			}
			result.Checks[name] = status
		}
		c.JSON(http.StatusOK, result)
	})
	router.GET("/metrics", gin.WrapH(promhttp.HandlerFor(cfg.Metrics.Registry, promhttp.HandlerOpts{})))
	router.GET("/openapi.yaml", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/yaml; charset=utf-8", apispec.Contract)
	})
	router.GET("/docs", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(docsHTML))
	})
	v1 := router.Group("/api/v1")
	for _, register := range cfg.Registrars {
		register(v1)
	}
	router.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, ErrorEnvelope(c, "route.not_found", "route not found"))
	})

	return router
}

func outboxContext(v2Enabled, searchEnabled, kafkaEnabled bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		ctx = outbox.WithV2EventsEnabled(ctx, v2Enabled)
		ctx = outbox.WithSearchEnabled(ctx, searchEnabled)
		ctx = outbox.WithKafkaEnabled(ctx, kafkaEnabled)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

const docsHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>BX YunPan API</title>
</head>
<body>
  <redoc spec-url="/openapi.yaml"></redoc>
  <script src="https://cdn.redoc.ly/redoc/latest/bundles/redoc.standalone.js"></script>
</body>
</html>`
