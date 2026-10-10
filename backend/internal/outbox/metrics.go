package outbox

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics exposes the queue health signals required during the expand/contract
// window. The *_total name is retained for dashboard compatibility; it is a
// gauge representing the current unresolved backlog, not a cumulative counter.
type Metrics struct {
	UnroutedEventsTotal *prometheus.GaugeVec
	UnroutedOldestAge   prometheus.Gauge
	LeaseRecoveries     prometheus.Counter
}

// UnroutedCount is the aggregate form used by the dispatcher. Keeping the
// oldest timestamp alongside the count lets the relay report an exact backlog
// without loading every event row into memory.
type UnroutedCount struct {
	EventType string
	Count     int64
	Oldest    time.Time
}

func NewMetrics(registerer prometheus.Registerer) *Metrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	metrics := &Metrics{
		LeaseRecoveries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "yunpan_outbox_lease_recoveries_total", Help: "Expired delivery leases successfully reclaimed by this relay.",
		}),
		UnroutedEventsTotal: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "yunpan",
			Subsystem: "outbox",
			Name:      "unrouted_events_total",
			Help:      "Current unresolved outbox events without an enabled delivery route.",
		}, []string{"event_type"}),
		UnroutedOldestAge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "yunpan",
			Subsystem: "outbox",
			Name:      "unrouted_oldest_age_seconds",
			Help:      "Age in seconds of the oldest unresolved unrouted outbox event.",
		}),
	}
	registerer.MustRegister(metrics.UnroutedEventsTotal, metrics.UnroutedOldestAge, metrics.LeaseRecoveries)
	return metrics
}

func (m *Metrics) ObserveUnrouted(events []Event, now time.Time) {
	if m == nil {
		return
	}
	m.UnroutedEventsTotal.Reset()
	oldest := time.Time{}
	for _, event := range events {
		m.UnroutedEventsTotal.WithLabelValues(event.EventType).Inc()
		created := event.CreatedAt
		if oldest.IsZero() || created.Before(oldest) {
			oldest = created
		}
	}
	if oldest.IsZero() {
		m.UnroutedOldestAge.Set(0)
		return
	}
	age := now.Sub(oldest).Seconds()
	if age < 0 {
		age = 0
	}
	m.UnroutedOldestAge.Set(age)
}

func (m *Metrics) ObserveUnroutedCounts(counts []UnroutedCount, now time.Time) {
	if m == nil {
		return
	}
	m.UnroutedEventsTotal.Reset()
	oldest := time.Time{}
	for _, item := range counts {
		if item.Count <= 0 {
			continue
		}
		m.UnroutedEventsTotal.WithLabelValues(item.EventType).Set(float64(item.Count))
		if !item.Oldest.IsZero() && (oldest.IsZero() || item.Oldest.Before(oldest)) {
			oldest = item.Oldest
		}
	}
	if oldest.IsZero() {
		m.UnroutedOldestAge.Set(0)
		return
	}
	age := now.Sub(oldest).Seconds()
	if age < 0 {
		age = 0
	}
	m.UnroutedOldestAge.Set(age)
}
