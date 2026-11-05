package dbmetrics

import (
	"context"
	"database/sql"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"gorm.io/gorm"
)

// BacklogCollector reads durable work independently of the relay/reconciler.
// Failed scrapes expose up=0 and omit backlog samples instead of reporting zero.
type BacklogCollector struct {
	db    *gorm.DB
	query string
	size  *prometheus.Desc
	age   *prometheus.Desc
	up    *prometheus.Desc
}

func RegisterPool(registerer prometheus.Registerer, db *sql.DB, role string) {
	registerer.MustRegister(collectors.NewDBStatsCollector(db, role))
}

func NewOutboxCollector(db *gorm.DB) *BacklogCollector {
	return newBacklogCollector(db, "outbox", "destination", `
		SELECT d.destination AS kind, d.status AS state, COUNT(*) AS count,
		       GREATEST(0, EXTRACT(EPOCH FROM (now() - MIN(e.created_at)))) AS age
		FROM outbox_deliveries d JOIN outbox_events e ON e.id = d.event_id
		WHERE d.transport = 'rabbitmq' AND d.status IN ('pending', 'sending', 'failed')
		GROUP BY d.destination, d.status`)
}

func NewStorageCollector(db *gorm.DB) *BacklogCollector {
	return newBacklogCollector(db, "storage", "operation", `
		SELECT operation_type AS kind, state, COUNT(*) AS count,
		       GREATEST(0, EXTRACT(EPOCH FROM (now() - MIN(created_at)))) AS age
		FROM storage_operations WHERE state IN ('pending', 'running', 'failed', 'dead')
		GROUP BY operation_type, state`)
}

func NewRecoveryCollector(db *gorm.DB) *BacklogCollector {
	return newBacklogCollector(db, "recovery", "event_type", `
		SELECT e.event_type AS kind, 'stopped' AS state, COUNT(DISTINCT e.aggregate_id) AS count,
		       GREATEST(0, EXTRACT(EPOCH FROM (now() - MIN(r.stopped_at)))) AS age
		FROM outbox_recoveries r JOIN outbox_events e ON e.id = r.event_id
		WHERE r.stopped_at IS NOT NULL GROUP BY e.event_type`)
}

func newBacklogCollector(db *gorm.DB, name, kind, query string) *BacklogCollector {
	prefix := "yunpan_" + name
	return &BacklogCollector{
		db: db, query: query,
		size: prometheus.NewDesc(prefix+"_backlog", "Current durable unfinished work, including delayed retries.", []string{kind, "state"}, nil),
		age:  prometheus.NewDesc(prefix+"_oldest_age_seconds", "Age since creation of the oldest unfinished operation or outbox event, including delayed retries.", []string{kind, "state"}, nil),
		up:   prometheus.NewDesc(prefix+"_backlog_scrape_success", "Whether the durable backlog query succeeded.", nil, nil),
	}
}

func (c *BacklogCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.size
	ch <- c.age
	ch <- c.up
}

func (c *BacklogCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var rows []struct {
		Kind, State string
		Count       int64
		Age         float64
	}
	if err := c.db.WithContext(ctx).Raw(c.query).Scan(&rows).Error; err != nil {
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 1)
	for _, row := range rows {
		ch <- prometheus.MustNewConstMetric(c.size, prometheus.GaugeValue, float64(row.Count), row.Kind, row.State)
		ch <- prometheus.MustNewConstMetric(c.age, prometheus.GaugeValue, row.Age, row.Kind, row.State)
	}
}
