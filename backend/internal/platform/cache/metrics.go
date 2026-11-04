package cache

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/breaker"
)

type Metrics struct {
	lookups         *prometheus.CounterVec
	loads           *prometheus.CounterVec
	redisErrors     *prometheus.CounterVec
	evictions       prometheus.Counter
	shared          prometheus.Counter
	circuitState    prometheus.Gauge
	breakerRejected prometheus.Counter
}

func NewMetrics(registerer prometheus.Registerer) *Metrics {
	m := &Metrics{
		lookups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "yunpan_cache_lookups_total", Help: "Business cache lookup attempts by result, excluding coalesced waiters.",
		}, []string{"result"}),
		loads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "yunpan_cache_source_loads_total", Help: "Business cache source calls by result.",
		}, []string{"result"}),
		redisErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "yunpan_cache_redis_errors_total", Help: "Business cache Redis errors by operation.",
		}, []string{"operation"}),
		evictions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "yunpan_cache_l1_evictions_total", Help: "Business cache L1 capacity evictions.",
		}),
		shared: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "yunpan_cache_shared_loads_total", Help: "Callers participating in a shared singleflight result.",
		}),
		circuitState: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "yunpan_cache_redis_circuit_state", Help: "Redis cache circuit state: 0 closed, 1 open, 2 half-open.",
		}),
		breakerRejected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "yunpan_cache_redis_circuit_rejections_total", Help: "Redis cache operations skipped while the circuit is open.",
		}),
	}
	registerer.MustRegister(m.lookups, m.loads, m.redisErrors, m.evictions, m.shared, m.circuitState, m.breakerRejected)
	return m
}

func (m *Metrics) lookup(result string) {
	if m != nil {
		m.lookups.WithLabelValues(result).Inc()
	}
}

func (m *Metrics) source(err error) {
	if m != nil {
		result := "success"
		if err != nil {
			result = "error"
		}
		m.loads.WithLabelValues(result).Inc()
	}
}

func (m *Metrics) redisError(operation string, err error) {
	if m != nil && err != nil {
		m.redisErrors.WithLabelValues(operation).Inc()
	}
}

func (m *Metrics) setBreakerState(state breaker.State) {
	if m != nil {
		m.circuitState.Set(float64(state))
	}
}

func (m *Metrics) breakerRejectedCall() {
	if m != nil {
		m.breakerRejected.Inc()
	}
}
