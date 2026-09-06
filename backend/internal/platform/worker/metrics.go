package worker

import (
	"context"
	"sort"
	"time"

	"github.com/hibiken/asynq"
	"github.com/prometheus/client_golang/prometheus"
	redis "github.com/redis/go-redis/v9"
)

// Metrics exposes task execution and broker queue state for one worker role.
// Queue state is observational only; PostgreSQL remains the authority for
// recovery and completion.
type Metrics struct {
	role       string
	queues     []string
	inspector  *asynq.Inspector
	tasks      *prometheus.CounterVec
	duration   *prometheus.HistogramVec
	inflight   *prometheus.GaugeVec
	queueSize  *prometheus.GaugeVec
	queueState *prometheus.GaugeVec
}

// NewMetrics registers role-scoped worker metrics on the supplied registry.
// The Redis client may be nil in unit tests; task metrics still work then.
func NewMetrics(registerer prometheus.Registerer, role Role, queues map[string]int, client redis.UniversalClient) *Metrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	queueNames := make([]string, 0, len(queues))
	for queue := range queues {
		queueNames = append(queueNames, queue)
	}
	sort.Strings(queueNames)
	metrics := &Metrics{
		role:   string(role),
		queues: queueNames,
		tasks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "yunpan", Subsystem: "worker", Name: "tasks_total",
			Help: "Total Asynq tasks handled by role and result.",
		}, []string{"role", "task_type", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "yunpan", Subsystem: "worker", Name: "task_duration_seconds",
			Help: "Asynq task handling duration by role and task type.",
		}, []string{"role", "task_type"}),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "yunpan", Subsystem: "worker", Name: "inflight_tasks",
			Help: "Current in-flight Asynq tasks by role.",
		}, []string{"role"}),
		queueSize: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "yunpan", Subsystem: "worker", Name: "queue_size",
			Help: "Current total task count in an Asynq queue.",
		}, []string{"role", "queue"}),
		queueState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "yunpan", Subsystem: "worker", Name: "queue_state",
			Help: "Asynq queue state values: pending, active, retry, archived, failed_total, latency_seconds.",
		}, []string{"role", "queue", "state"}),
	}
	registerer.MustRegister(metrics.tasks, metrics.duration, metrics.inflight, metrics.queueSize, metrics.queueState)
	if client != nil {
		metrics.inspector = asynq.NewInspectorFromRedisClient(client)
	}
	return metrics
}

// Middleware records every handler invocation, including failed attempts.
func (m *Metrics) Middleware() asynq.MiddlewareFunc {
	return func(next asynq.Handler) asynq.Handler {
		return asynq.HandlerFunc(func(ctx context.Context, task *asynq.Task) error {
			if m == nil {
				return next.ProcessTask(ctx, task)
			}
			started := time.Now()
			m.inflight.WithLabelValues(m.role).Inc()
			defer m.inflight.WithLabelValues(m.role).Dec()
			err := next.ProcessTask(ctx, task)
			result := "success"
			if err != nil {
				result = "error"
			}
			m.tasks.WithLabelValues(m.role, task.Type(), result).Inc()
			m.duration.WithLabelValues(m.role, task.Type()).Observe(time.Since(started).Seconds())
			return err
		})
	}
}

// StartQueueObserver refreshes broker-backed queue gauges until ctx ends.
func (m *Metrics) StartQueueObserver(ctx context.Context) {
	if m == nil || m.inspector == nil || len(m.queues) == 0 {
		return
	}
	go func() {
		m.observeQueues()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.observeQueues()
			}
		}
	}()
}

func (m *Metrics) observeQueues() {
	for _, queue := range m.queues {
		info, err := m.inspector.GetQueueInfo(queue)
		if err != nil {
			continue
		}
		m.queueSize.WithLabelValues(m.role, queue).Set(float64(info.Size))
		m.queueState.WithLabelValues(m.role, queue, "pending").Set(float64(info.Pending))
		m.queueState.WithLabelValues(m.role, queue, "active").Set(float64(info.Active))
		m.queueState.WithLabelValues(m.role, queue, "retry").Set(float64(info.Retry))
		m.queueState.WithLabelValues(m.role, queue, "archived").Set(float64(info.Archived))
		m.queueState.WithLabelValues(m.role, queue, "failed_total").Set(float64(info.FailedTotal))
		m.queueState.WithLabelValues(m.role, queue, "latency_seconds").Set(info.Latency.Seconds())
	}
}
