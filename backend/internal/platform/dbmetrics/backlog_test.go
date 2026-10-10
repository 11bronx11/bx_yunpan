package dbmetrics

import (
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestBacklogCollectorTracksDurableWorkAndQueryFailure(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	tx := db.Begin()
	t.Cleanup(func() { _ = tx.Rollback().Error })
	registry := prometheus.NewRegistry()
	registry.MustRegister(NewStorageCollector(tx))
	outboxRegistry := prometheus.NewRegistry()
	outboxRegistry.MustRegister(NewOutboxCollector(tx))
	baseline := gaugeSum(t, registry, "yunpan_storage_backlog")
	id := uuid.New()
	if err := tx.Exec(`INSERT INTO storage_operations (id, operation_type, idempotency_key, state, created_at)
		VALUES (?, 'multipart.create', ?, 'failed', ?)`, id, id.String(), time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if got := gaugeSum(t, registry, "yunpan_storage_backlog"); got != baseline+1 {
		t.Fatalf("backlog=%v, want %v", got, baseline+1)
	}
	if got := gaugeSum(t, registry, "yunpan_storage_oldest_age_seconds"); got < 59 {
		t.Fatalf("oldest age=%v", got)
	}
	if err := tx.Exec("UPDATE storage_operations SET state = 'succeeded' WHERE id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if got := gaugeSum(t, registry, "yunpan_storage_backlog"); got != baseline {
		t.Fatalf("drained backlog=%v, want %v", got, baseline)
	}
	if got := gaugeSum(t, outboxRegistry, "yunpan_outbox_backlog_scrape_success"); got != 1 {
		t.Fatalf("outbox scrape success=%v", got)
	}
	outboxBaseline := gaugeSum(t, outboxRegistry, "yunpan_outbox_backlog")
	if err := tx.Exec(`INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, event_version, payload, created_at)
		VALUES (?, 'upload_session', ?, 'object.verify_requested.v1', 1, '{}', ?)`, id, id, time.Now().Add(-10*time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(`INSERT INTO outbox_deliveries (event_id, transport, destination, status, available_at)
		VALUES (?, 'rabbitmq', 'object.verify.v1@object', 'failed', ?)`, id, time.Now().Add(time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if got := gaugeSum(t, outboxRegistry, "yunpan_outbox_backlog"); got != outboxBaseline+1 {
		t.Fatalf("outbox backlog=%v, want %v", got, outboxBaseline+1)
	}
	if got := gaugeSum(t, outboxRegistry, "yunpan_outbox_oldest_age_seconds"); got < 599 {
		t.Fatalf("retry reset original event age: %v", got)
	}
	if err := tx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	if got := gaugeSum(t, registry, "yunpan_storage_backlog_scrape_success"); got != 0 {
		t.Fatalf("failed query reported success=%v", got)
	}
}

func gaugeSum(t *testing.T, registry *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, family := range families {
		if family.GetName() == name {
			for _, metric := range family.Metric {
				sum += metric.GetGauge().GetValue()
			}
		}
	}
	return sum
}
