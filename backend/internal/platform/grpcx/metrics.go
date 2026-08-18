package grpcx

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics 记录 gRPC 调用的耗时与错误码。服务端与客户端各建一份，
// 分别注册到自己进程的 registry。
type Metrics struct {
	handled  *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// NewMetrics 按 side 区分 subsystem：server 或 client。
func NewMetrics(registry prometheus.Registerer, side string) *Metrics {
	handled := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "yunpan",
		Subsystem: "grpc_" + side,
		Name:      "handled_total",
		Help:      "Total gRPC calls by method and status code.",
	}, []string{"method", "code"})
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "yunpan",
		Subsystem: "grpc_" + side,
		Name:      "handling_duration_seconds",
		Help:      "gRPC call latency by method.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"method"})
	registry.MustRegister(handled, duration)
	return &Metrics{handled: handled, duration: duration}
}

func (m *Metrics) observe(method, code string, elapsed time.Duration) {
	if m == nil {
		return
	}
	m.handled.WithLabelValues(method, code).Inc()
	m.duration.WithLabelValues(method).Observe(elapsed.Seconds())
}
