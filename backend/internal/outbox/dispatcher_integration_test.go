package outbox

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"
	"github.com/lib/pq"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

func newDeliveryFixture(t *testing.T) (*gorm.DB, uuid.UUID) {
	t.Helper()
	db := isolatedOutboxDB(t)
	id, aggregateID := uuid.New(), uuid.New()
	envelope, err := eventcontract.New(eventcontract.EnvelopeInput{
		EventID: id, EventType: eventcontract.EventObjectVerifyRequestedV1,
		AggregateID: aggregateID, AggregateVersion: 1, Producer: "hc-window-test",
		Payload: eventcontract.ObjectVerifyRequestedV1{UploadID: aggregateID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := AddEnvelope(db, envelope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Delete(&Event{}, "id = ?", id).Error })
	return db, id
}

func claimOne(t *testing.T, d *Dispatcher, id uuid.UUID) claimedDelivery {
	t.Helper()
	if err := d.prepareDeliveries(context.Background()); err != nil {
		t.Fatal(err)
	}
	items, err := d.claimDeliveries(context.Background())
	if err != nil || len(items) != 1 || items[0].event.ID != id {
		t.Fatalf("claim: items=%v error=%v", items, err)
	}
	return items[0]
}

func expireDelivery(t *testing.T, db *gorm.DB, id uuid.UUID) {
	t.Helper()
	if err := db.Model(&Delivery{}).Where("event_id = ?", id).Update("lease_until", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
}

func TestDispatcherLeaseTakeoverFencesOldACK(t *testing.T) {
	db, id := newDeliveryFixture(t)
	d := newDispatcher(db, &recordingPublisher{}, slog.Default(), config.Outbox{BatchSize: 1, TaskTimeout: time.Minute})
	old := claimOne(t, d, id)
	expireDelivery(t, db, id)
	next := claimOne(t, d, id)
	if err := d.ackDelivery(old.delivery, old.event); err == nil {
		t.Fatal("stale ACK unexpectedly succeeded")
	}
	if err := d.failDelivery(old.delivery, "late failure"); err == nil {
		t.Fatal(err)
	}
	var got Delivery
	if err := db.First(&got, "event_id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != "sending" || got.LeaseToken == nil || *got.LeaseToken != *next.delivery.LeaseToken {
		t.Fatalf("stale ACK/fail overwrote replacement lease: %+v", got)
	}
	if err := d.ackDelivery(next.delivery, next.event); err != nil {
		t.Fatal(err)
	}
}

func TestDispatcherRealRabbitCrashBeforeACK(t *testing.T) {
	addr := isolatedRabbitURL(t)
	db, id := newDeliveryFixture(t)
	client := taskqueue.New(taskqueue.Config{URL: addr})
	t.Cleanup(func() { _ = client.Close() })
	d := newDispatcher(db, client, slog.Default(), config.Outbox{BatchSize: 1, TaskTimeout: time.Minute})
	old := claimOne(t, d, id)
	target, ok := targetFromDelivery(old.delivery)
	if !ok {
		t.Fatal("missing route")
	}
	taskID := id.String() + ":" + old.delivery.Destination
	conn, err := amqp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Publish(context.Background(), taskqueue.Task{ID: taskID, Kind: target.TaskType, Queue: target.Queue, Body: payloadWithTrace(old.event, nil)}); err != nil {
		t.Fatal(err)
	}
	// Leave the database at sending, as a killed relay would after RabbitMQ confirms.
	expireDelivery(t, db, id)
	next := claimOne(t, d, id)
	if err := d.sendDelivery(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	// Both copies have the same identity; consumers/business dedup owns this
	// unavoidable broker-confirm / database-ACK crash window.
	for i := 0; i < 2; i++ {
		msg, ok, e := ch.Get(target.Queue, true)
		if e != nil || !ok || msg.MessageId != taskID {
			t.Fatalf("delivery %d: %v %v %s", i, ok, e, msg.MessageId)
		}
	}
	var got Delivery
	if err := db.First(&got, "event_id = ?", id).Error; err != nil || got.Status != "published" || got.Attempt != 2 {
		t.Fatalf("recovered delivery: %+v, error=%v", got, err)
	}
}

func TestDispatcherDrainsBacklogWithoutWaitingForPollInterval(t *testing.T) {
	db, first := newDeliveryFixture(t)
	_, second := newDeliveryFixture(t)
	_, third := newDeliveryFixture(t)
	ids := []uuid.UUID{first, second, third}
	d := newDispatcher(db, &recordingPublisher{}, slog.Default(), config.Outbox{BatchSize: 1, PollInterval: time.Hour, TaskTimeout: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); d.poll(ctx, nil) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int64
		if err := db.Model(&Delivery{}).Where("event_id IN ? AND status = 'published'", ids).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count == 3 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("backlog was throttled by the one-hour idle poll interval")
}

func TestDispatcherThrottlesCompatibilityPreparationButPinsNewDeliveries(t *testing.T) {
	db := isolatedOutboxDB(t)
	publisher := &recordingPublisher{}
	d := newDispatcher(db, publisher, slog.Default(), config.Outbox{
		BatchSize: 10, PollInterval: 20 * time.Millisecond, MaxPollInterval: time.Hour,
		TaskTimeout: time.Minute, GCDelay: 24 * time.Hour,
	})
	if err := d.prepareDeliveriesForDispatch(context.Background()); err != nil {
		t.Fatal(err)
	}

	legacyID, aggregateID := uuid.New(), uuid.New()
	legacy := Event{
		ID: legacyID, AggregateType: "upload", AggregateID: aggregateID,
		EventType: EventObjectVerifyRequested, EventVersion: 1,
		Payload: []byte(`{"upload_id":"` + aggregateID.String() + `"}`),
		Status:  "pending", AvailableAt: time.Now().UTC(),
	}
	if err := db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}

	newID := uuid.New()
	newAggregateID := uuid.New()
	envelope, err := eventcontract.New(eventcontract.EnvelopeInput{
		EventID: newID, EventType: eventcontract.EventObjectVerifyRequestedV1,
		AggregateID: newAggregateID, AggregateVersion: 1, Producer: "outbox-preparation-test",
		Payload: eventcontract.ObjectVerifyRequestedV1{UploadID: newAggregateID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := AddEnvelope(db, envelope); err != nil {
		t.Fatal(err)
	}

	processed, err := d.dispatchBatchCount(context.Background())
	if err != nil || processed != 1 {
		t.Fatalf("new delivery dispatch: processed=%d error=%v", processed, err)
	}
	var fresh Delivery
	if err := db.First(&fresh, "event_id = ?", newID).Error; err != nil {
		t.Fatal(err)
	}
	if fresh.Status != "published" || fresh.NotBefore == nil {
		t.Fatalf("new delivery was not pinned and sent during preparation throttle: %+v", fresh)
	}
	var legacyDeliveries int64
	if err := db.Model(&Delivery{}).Where("event_id = ?", legacyID).Count(&legacyDeliveries).Error; err != nil {
		t.Fatal(err)
	}
	if legacyDeliveries != 0 {
		t.Fatalf("compatibility pass ran before its interval: deliveries=%d", legacyDeliveries)
	}

	d.preparationMu.Lock()
	d.preparedAt = time.Time{}
	d.preparationMu.Unlock()
	processed, err = d.dispatchBatchCount(context.Background())
	if err != nil || processed != 1 {
		t.Fatalf("compatibility recovery dispatch: processed=%d error=%v", processed, err)
	}
	if err := db.First(&fresh, "event_id = ? AND transport = 'rabbitmq'", legacyID).Error; err != nil {
		t.Fatal(err)
	}
	if fresh.Status != "published" {
		t.Fatalf("legacy delivery status = %q, want published", fresh.Status)
	}
}

func TestDispatcherNotificationWakesWhileFallbackIsLong(t *testing.T) {
	db, first := newDeliveryFixture(t)
	publisher := &recordingPublisher{}
	wakeups := make(chan struct{}, 1)
	d := newDispatcher(db, publisher, slog.Default(), config.Outbox{
		BatchSize: 1, PollInterval: 10 * time.Second, MaxPollInterval: 30 * time.Second,
		TaskTimeout: time.Minute,
	}).WithWakeups(wakeups)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); d.poll(ctx, nil) }()
	defer func() { cancel(); <-done }()
	waitPublished(t, db, first, time.Second)

	_, second := newDeliveryFixture(t)
	wakeups <- struct{}{}
	waitPublished(t, db, second, time.Second)
}

func TestDispatcherFallbackScanFindsWorkWithNotificationsDisabled(t *testing.T) {
	db := isolatedOutboxDB(t)
	publisher := &recordingPublisher{}
	d := newDispatcher(db, publisher, slog.Default(), config.Outbox{
		BatchSize: 1, PollInterval: 20 * time.Millisecond, MaxPollInterval: 80 * time.Millisecond,
		TaskTimeout: time.Minute,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); d.poll(ctx, nil) }()
	defer func() { cancel(); <-done }()
	// Let the empty scan back off to its configured ceiling before inserting work.
	time.Sleep(300 * time.Millisecond)
	_, id := newDeliveryFixture(t)
	waitPublished(t, db, id, time.Second)
}

func TestDispatcherFailsClosedWhenConfiguredLeaderLockHasNoClient(t *testing.T) {
	db, id := newDeliveryFixture(t)
	d := newDispatcher(db, &recordingPublisher{}, nil, config.Outbox{BatchSize: 1})
	d.WithLeaderLock(nil, config.Locking{DispatcherLeaderEnabled: true})
	d.Run(context.Background())
	var delivery Delivery
	if err := db.First(&delivery, "event_id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if delivery.Status != "pending" {
		t.Fatalf("delivery status = %q, want pending without a leader lock client", delivery.Status)
	}
}

func waitPublished(t *testing.T, db *gorm.DB, id uuid.UUID, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var delivery Delivery
		err := db.First(&delivery, "event_id = ? AND transport = 'rabbitmq'", id).Error
		if err == nil && delivery.Status == "published" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("delivery %s was not published within %s", id, timeout)
}

type countingFailPublisher struct{ calls int }

func (p *countingFailPublisher) Publish(context.Context, taskqueue.Task) error {
	p.calls++
	return errors.New("injected broker outage")
}

func TestDispatcherStopsAtFirstPublishFailureAndReleasesUnsentClaims(t *testing.T) {
	db, first := newDeliveryFixture(t)
	_, second := newDeliveryFixture(t)
	publisher := &countingFailPublisher{}
	d := newDispatcher(db, publisher, slog.Default(), config.Outbox{BatchSize: 10, PollInterval: time.Second, MaxPollInterval: time.Minute, TaskTimeout: time.Minute})

	processed, err := d.dispatchBatchCount(context.Background())
	if err == nil || processed != 1 || publisher.calls != 1 {
		t.Fatalf("processed=%d calls=%d error=%v", processed, publisher.calls, err)
	}
	var failed, pending int64
	if err := db.Model(&Delivery{}).Where("event_id IN ? AND status = 'failed'", []uuid.UUID{first, second}).Count(&failed).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&Delivery{}).Where("event_id IN ? AND status = 'pending' AND lease_token IS NULL AND lease_until IS NULL", []uuid.UUID{first, second}).Count(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if failed != 1 || pending != 1 {
		t.Fatalf("after broker failure: failed=%d released_pending=%d", failed, pending)
	}
}

func TestPostgresWakeupIsDeliveredAfterDeliveryCommit(t *testing.T) {
	db := isolatedOutboxDB(t)
	if err := db.Exec(`CREATE OR REPLACE FUNCTION notify_outbox_delivery_insert() RETURNS trigger
        LANGUAGE plpgsql AS $$ BEGIN
            PERFORM pg_notify('bx_outbox_delivery_ready', '');
            RETURN NULL;
        END; $$`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER outbox_delivery_insert_notify
        AFTER INSERT ON outbox_deliveries
        FOR EACH STATEMENT EXECUTE FUNCTION notify_outbox_delivery_insert()`).Error; err != nil {
		t.Fatal(err)
	}

	listener := pq.NewListener(os.Getenv("TEST_POSTGRES_DSN"), time.Second, 5*time.Second, nil)
	t.Cleanup(func() { _ = listener.Close() })
	listenResult := make(chan error, 1)
	go func() { listenResult <- listener.Listen(deliveryWakeupChannel) }()
	select {
	case err := <-listenResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		_ = listener.Close()
		t.Fatal("timed out subscribing to PostgreSQL notification channel")
	}

	aggregateID := uuid.New()
	envelope, err := eventcontract.New(eventcontract.EnvelopeInput{
		EventID: uuid.New(), EventType: eventcontract.EventObjectVerifyRequestedV1,
		AggregateID: aggregateID, AggregateVersion: 1, Producer: "outbox-notify-test",
		Payload: eventcontract.ObjectVerifyRequestedV1{UploadID: aggregateID},
	})
	if err != nil {
		t.Fatal(err)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	if err := AddEnvelope(tx, envelope); err != nil {
		_ = tx.Rollback().Error
		t.Fatal(err)
	}
	select {
	case notification := <-listener.NotificationChannel():
		t.Fatalf("notification arrived before commit: %#v", notification)
	case <-time.After(50 * time.Millisecond):
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	select {
	case notification := <-listener.NotificationChannel():
		if notification == nil || notification.Channel != deliveryWakeupChannel {
			t.Fatalf("unexpected notification: %#v", notification)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("committed delivery did not wake the listener")
	}
	t.Cleanup(func() { _ = db.Delete(&Event{}, "id = ?", envelope.EventID).Error })
}
