package outbox

import (
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
)

func TestConsumeOnceRejectsInvalidInputsBeforeWriting(t *testing.T) {
	if handled, err := ConsumeOnce(nil, uuid.New(), "consumer", nil); err == nil || handled {
		t.Fatalf("nil db result = handled:%v err:%v", handled, err)
	}
	if handled, err := ConsumeOnce(&gorm.DB{}, uuid.Nil, "consumer", nil); !errors.Is(err, ErrInvalidConsumer) || handled {
		t.Fatalf("nil event id result = handled:%v err:%v", handled, err)
	}
	if handled, err := ConsumeOnce(&gorm.DB{}, uuid.New(), "   ", nil); !errors.Is(err, ErrInvalidConsumer) || handled {
		t.Fatalf("blank consumer result = handled:%v err:%v", handled, err)
	}
}

// This integration test is intentionally opt-in.  It uses one transaction
// and rolls it back, so a shared development database is not mutated when a
// caller explicitly supplies TEST_POSTGRES_DSN.
func TestConsumeOnceRunsEffectOnlyForFirstDelivery(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}

	eventID := uuid.New()
	consumerName := "test-consumer-" + uuid.NewString()
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { _ = tx.Rollback().Error })
	if err := tx.Exec("CREATE TEMP TABLE consume_once_effects (value integer NOT NULL) ON COMMIT DROP").Error; err != nil {
		t.Fatal(err)
	}

	calls := 0
	handled, err := ConsumeOnce(tx, eventID, consumerName, func(effectTx *gorm.DB) error {
		calls++
		return effectTx.Exec("INSERT INTO consume_once_effects(value) VALUES (1)").Error
	})
	if err != nil || !handled {
		t.Fatalf("first delivery = handled:%v err:%v", handled, err)
	}
	handled, err = ConsumeOnce(tx, eventID, consumerName, func(*gorm.DB) error {
		calls++
		return nil
	})
	if err != nil || handled {
		t.Fatalf("duplicate delivery = handled:%v err:%v", handled, err)
	}
	if calls != 1 {
		t.Fatalf("effect calls = %d, want 1", calls)
	}
	var count int64
	if err := tx.Table("consume_once_effects").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("side-effect rows = %d, want 1", count)
	}
}

func TestConsumeOnceMarkerRollsBackWithEffectFailure(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	consumerName := "rollback-consumer-" + uuid.NewString()
	err = db.Transaction(func(tx *gorm.DB) error {
		handled, consumeErr := ConsumeOnce(tx, eventID, consumerName, func(*gorm.DB) error {
			return errors.New("injected side effect failure")
		})
		if handled || consumeErr == nil || consumeErr.Error() != "injected side effect failure" {
			return errors.New("unexpected consume result")
		}
		return consumeErr
	})
	if err == nil || err.Error() != "injected side effect failure" {
		t.Fatalf("transaction error = %v", err)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { _ = tx.Rollback().Error })
	handled, consumeErr := ConsumeOnce(tx, eventID, consumerName, nil)
	if consumeErr != nil {
		t.Fatal(consumeErr)
	}
	if !handled {
		t.Fatal("consumption marker survived rollback")
	}
}

func TestAddEnvelopeEventAndDeliveriesRollbackTogether(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}

	eventID := uuid.New()
	aggregateID := uuid.New()
	version := int64(1)
	envelope, err := eventcontract.New(eventcontract.EnvelopeInput{
		EventID:          eventID,
		EventType:        eventcontract.EventObjectReadyV1,
		AggregateID:      aggregateID,
		AggregateVersion: version,
		Producer:         "test",
		Payload:          eventcontract.ObjectReadyV1{ObjectID: aggregateID},
	})
	if err != nil {
		t.Fatal(err)
	}
	transactionErr := db.Transaction(func(tx *gorm.DB) error {
		if err := AddEnvelope(tx, envelope); err != nil {
			return err
		}
		var deliveries int64
		if err := tx.Model(&Delivery{}).Where("event_id = ?", eventID).Count(&deliveries).Error; err != nil {
			return err
		}
		if deliveries != 2 {
			return errors.New("unexpected delivery count inside transaction")
		}
		return errors.New("rollback probe")
	})
	if transactionErr == nil || transactionErr.Error() != "rollback probe" {
		t.Fatalf("transaction error = %v", transactionErr)
	}
	var events, deliveries int64
	if err := db.Model(&Event{}).Where("id = ?", eventID).Count(&events).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&Delivery{}).Where("event_id = ?", eventID).Count(&deliveries).Error; err != nil {
		t.Fatal(err)
	}
	if events != 0 || deliveries != 0 {
		t.Fatalf("rolled back rows = events:%d deliveries:%d", events, deliveries)
	}
}

func TestAddEnvelopeDeliveryFanoutIsUniquePerDestination(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	aggregateID := uuid.New()
	version := int64(1)
	envelope, err := eventcontract.New(eventcontract.EnvelopeInput{
		EventID:          eventID,
		EventType:        eventcontract.EventObjectReadyV1,
		AggregateID:      aggregateID,
		AggregateVersion: version,
		Producer:         "test",
		Payload:          eventcontract.ObjectReadyV1{ObjectID: aggregateID},
	})
	if err != nil {
		t.Fatal(err)
	}
	transactionErr := db.Transaction(func(tx *gorm.DB) error {
		if err := AddEnvelope(tx, envelope); err != nil {
			return err
		}
		var event Event
		if err := tx.First(&event, "id = ?", eventID).Error; err != nil {
			return err
		}
		if err := addDeliveries(tx, event, true); err != nil {
			return err
		}
		var deliveries int64
		if err := tx.Model(&Delivery{}).Where("event_id = ?", eventID).Count(&deliveries).Error; err != nil {
			return err
		}
		if deliveries != 2 {
			return errors.New("delivery fanout was not unique")
		}
		return errors.New("rollback probe")
	})
	if transactionErr == nil || transactionErr.Error() != "rollback probe" {
		t.Fatalf("transaction error = %v", transactionErr)
	}
}

func TestEnsureRecoveryConcurrentInsertIsIdempotent(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	key := "concurrent-recovery:" + uuid.NewString()
	resourceID := uuid.New()
	eventID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("bx-yunpan/recovery/"+key))
	input := eventcontract.EnvelopeInput{
		EventType:        eventcontract.EventStorageOperationRecoverV1,
		AggregateID:      resourceID,
		AggregateVersion: 1,
		Producer:         "concurrency-test",
		Payload: eventcontract.StorageOperationRecoverV1{
			OperationID: resourceID, OperationType: "object.delete", RecoveryKey: key,
		},
	}
	t.Cleanup(func() {
		_ = db.Exec(`DELETE FROM outbox_recoveries WHERE recovery_key = ?`, key).Error
		_ = db.Exec(`DELETE FROM outbox_events WHERE id = ?`, eventID).Error
	})

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan struct {
		created bool
		err     error
	}, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var created bool
			err := db.Transaction(func(tx *gorm.DB) error {
				var err error
				created, err = EnsureRecovery(tx, key, "concurrency_probe", "storage_operation", resourceID, 1, input)
				return err
			})
			results <- struct {
				created bool
				err     error
			}{created: created, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	createdCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("created count = %d, want 1", createdCount)
	}
	var recoveryCount, eventCount, deliveryCount int64
	if err := db.Table("outbox_recoveries").Where("recovery_key = ?", key).Count(&recoveryCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("outbox_events").Where("id = ?", eventID).Count(&eventCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("outbox_deliveries").Where("event_id = ?", eventID).Count(&deliveryCount).Error; err != nil {
		t.Fatal(err)
	}
	if recoveryCount != 1 || eventCount != 1 || deliveryCount != 1 {
		t.Fatalf("rows = recovery:%d event:%d delivery:%d", recoveryCount, eventCount, deliveryCount)
	}
}
