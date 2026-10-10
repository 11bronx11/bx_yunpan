package worker

import (
	"context"
	"sort"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics exposes task execution and broker queue state for one worker role.
// Queue state is observational only; PostgreSQL remains the authority for
// recovery and completion.
type Metrics struct {
	role     string
	queues   []string
	tasks    *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inflight *prometheus.GaugeVec
}

// NewMetrics registers role-scoped worker metrics on the supplied registry.
// Broker queue state is exposed by RabbitMQ's native Prometheus plugin.
func NewMetrics(registerer prometheus.Registerer, role Role, queues map[string]int) *Metrics {
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
			Help: "Total RabbitMQ tasks handled by role and result.",
		}, []string{"role", "task_type", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "yunpan", Subsystem: "worker", Name: "task_duration_seconds",
			Help: "RabbitMQ task handling duration by role and task type.",
		}, []string{"role", "task_type"}),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "yunpan", Subsystem: "worker", Name: "inflight_tasks",
			Help: "Current in-flight RabbitMQ tasks by role.",
		}, []string{"role"}),
	}
	registerer.MustRegister(metrics.tasks, metrics.duration, metrics.inflight)

	return metrics
}

// Middleware records every handler invocation, including failed attempts.
func (m *Metrics) Middleware() taskqueue.MiddlewareFunc {
	return func(next taskqueue.Handler) taskqueue.Handler {
		return taskqueue.HandlerFunc(func(ctx context.Context, task *taskqueue.Task) error {
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
