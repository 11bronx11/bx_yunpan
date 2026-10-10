package outbox

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
)

func TestV2EventContextDefaultsClosedAndCanBeEnabled(t *testing.T) {
	if V2EventsEnabled(context.Background()) {
		t.Fatal("background context unexpectedly enables v2 events")
	}
	if !V2EventsEnabled(WithV2EventsEnabled(context.Background(), true)) {
		t.Fatal("enabled context was not propagated")
	}
	if V2EventsEnabled(WithV2EventsEnabled(context.Background(), false)) {
		t.Fatal("disabled context unexpectedly enabled v2 events")
	}
}

func TestUnroutedMetricsExposeBacklogAndOldestAge(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	now := time.Now().UTC()
	oldest := now.Add(-3 * time.Second)
	events := []Event{
		{ID: uuid.New(), EventType: eventcontract.EventFileUpdatedV1, CreatedAt: oldest},
		{ID: uuid.New(), EventType: eventcontract.EventFileUpdatedV1, CreatedAt: now.Add(-time.Second)},
	}
	metrics.ObserveUnrouted(events, now)
	metrics.ObserveUnrouted(nil, now)
	// A second observation with an empty backlog must clear the labelled gauge.
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "yunpan_outbox_unrouted_oldest_age_seconds" && family.GetMetric()[0].GetGauge().GetValue() != 0 {
			t.Fatalf("oldest age was not cleared: %v", family.GetMetric()[0].GetGauge().GetValue())
		}
	}
}

// This opt-in integration probe exercises the actual rollout switch against a
// migrated PostgreSQL schema. It always rolls back, so the shared database is
// not modified. Set TEST_POSTGRES_DSN to run it.
func TestV2FlagControlsTypedDomainEventProduction(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	aggregateID, ownerID := uuid.New(), uuid.New()
	input := eventcontract.EnvelopeInput{
		EventType: eventcontract.EventFileUpdatedV1, AggregateID: aggregateID, AggregateVersion: 2,
		Producer: "compatibility-test", Payload: eventcontract.FileUpdatedV1{OwnerID: ownerID, FileEntryID: aggregateID, ObjectID: uuid.New(), OldName: "old", NewName: "new"},
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := AddTyped(context.Background(), tx, input); err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&Event{}).Where("aggregate_id = ?", aggregateID).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return errors.New("disabled v2 flag wrote a domain event")
		}
		if err := AddTyped(WithV2EventsEnabled(context.Background(), true), tx, input); err != nil {
			return err
		}
		if err := tx.Model(&Event{}).Where("aggregate_id = ?", aggregateID).Count(&count).Error; err != nil {
			return err
		}
		if count != 1 {
			return errors.New("enabled v2 flag did not write exactly one domain event")
		}
		return errors.New("rollout probe rollback")
	})
	if err == nil || err.Error() != "rollout probe rollback" {
		t.Fatalf("transaction result = %v", err)
	}
}

func TestLegacyRelayBridgePreservesPublishedStateAndAddsExplicitRoutes(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pendingID, publishedID := uuid.New(), uuid.New()
	rollback := errors.New("compatibility probe rollback")
	err = db.Transaction(func(tx *gorm.DB) error {
		for _, event := range []Event{
			{ID: pendingID, AggregateType: "file_object", AggregateID: uuid.New(), EventType: EventObjectReady, EventVersion: 1, Payload: []byte(`{"object_id":"00000000-0000-0000-0000-000000000001"}`), Status: "pending", AvailableAt: time.Now().UTC()},
			{ID: publishedID, AggregateType: "file_object", AggregateID: uuid.New(), EventType: EventObjectReady, EventVersion: 1, Payload: []byte(`{"object_id":"00000000-0000-0000-0000-000000000001"}`), Status: "published", AvailableAt: time.Now().UTC(), PublishedAt: ptr(time.Now().UTC())},
		} {
			if err := tx.Create(&event).Error; err != nil {
				return err
			}
		}
		if err := ensureLegacyDeliveries(tx, false); err != nil {
			return err
		}
		if err := ensureLegacyDeliveries(tx, true); err != nil {
			return err
		}
		var pendingDeliveries int64
		if err := tx.Model(&Delivery{}).Where("event_id = ?", pendingID).Count(&pendingDeliveries).Error; err != nil {
			return err
		}
		if pendingDeliveries != 2 {
			return errors.New("legacy pending event did not get both explicit routes")
		}
		var publishedDeliveries int64
		if err := tx.Model(&Delivery{}).Where("event_id = ?", publishedID).Count(&publishedDeliveries).Error; err != nil {
			return err
		}
		if publishedDeliveries != 0 {
			return errors.New("compatibility bridge re-created a published event delivery")
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("transaction result = %v", err)
	}
}
