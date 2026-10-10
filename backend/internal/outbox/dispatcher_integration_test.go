package outbox

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"
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
