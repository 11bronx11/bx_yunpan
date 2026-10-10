package outbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func publishedRecoveryRow(t *testing.T, db *gorm.DB, eventType, destination string, published time.Time) Delivery {
	t.Helper()
	id := uuid.New()
	event := Event{ID: id, AggregateID: uuid.New(), AggregateType: "proof", EventType: eventType, EventVersion: 1, Payload: json.RawMessage(`{}`), Status: "published", PublishedAt: &published, AvailableAt: published, CreatedAt: published}
	if err := db.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	row := Delivery{EventID: id, Transport: "rabbitmq", Destination: destination, Status: "published", PublishedAt: &published, AvailableAt: published, NotBefore: &published}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func TestRecoveryBatchLegacyReceiptsAndUnsupportedRoutesCannotStarve(t *testing.T) {
	db := isolatedOutboxDB(t)
	now := time.Now().UTC()
	old := now.Add(-time.Hour)
	publishedRecoveryRow(t, db, EventObjectVerifyRequested, "unknown@object", old)
	legacy := publishedRecoveryRow(t, db, EventObjectVerifyRequested, EventObjectVerifyRequested+"@object", old)
	if err := db.Create(&taskqueue.Result{TaskID: deliveryTaskID(legacy), Status: "succeeded", FinishedAt: old}).Error; err != nil {
		t.Fatal(err)
	}
	live := publishedRecoveryRow(t, db, EventObjectGCRequested, EventObjectGCRequested+"@maintenance", old)
	token := uuid.New()
	expires := now.Add(time.Hour)
	if err := db.Create(&taskqueue.Execution{TaskID: deliveryTaskID(live), TaskType: TaskObjectGCV1, Queue: "maintenance", PayloadSHA256: "proof", Status: "running", LeaseToken: &token, LeaseUntil: &expires, AvailableAt: old, MaxRetry: 3, TimeoutMS: 60000, UpdatedAt: old}).Error; err != nil {
		t.Fatal(err)
	}
	missing := publishedRecoveryRow(t, db, EventObjectVerifyRequested, TaskObjectVerifyV1+"@object", old.Add(time.Minute))
	if err := db.Transaction(func(tx *gorm.DB) error {
		n, err := RecoverPublishedBatch(tx, now, 15*time.Minute, 1, true, true)
		if n != 1 {
			t.Errorf("replays=%d; older legacy successes or unsupported destinations starved missing work", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var row Delivery
	if err := db.Where("event_id=?", missing.EventID).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != "pending" {
		t.Fatal("missing work not recovered")
	}
}

func TestRecoveryBatchFiveQueuesAndFeatureSwitches(t *testing.T) {
	for _, ai := range []bool{false, true} {
		for _, search := range []bool{false, true} {
			t.Run(fmt.Sprintf("ai=%v/search=%v", ai, search), func(t *testing.T) {
				db := isolatedOutboxDB(t)
				now := time.Now().UTC()
				for _, spec := range []struct{ event, destination string }{
					{EventAIIndexRequested, EventAIIndexRequested + "@ai"},
					{EventObjectReady, TaskSearchProjectV1 + "@search"},
					{EventObjectVerifyRequested, TaskObjectVerifyV1 + "@object"},
					{EventObjectReady, EventObjectReady + "@media"},
					{EventObjectGCRequested, EventObjectGCRequested + "@maintenance"},
				} {
					publishedRecoveryRow(t, db, spec.event, spec.destination, now.Add(-time.Hour))
				}
				want := 3
				if ai {
					want++
				}
				if search {
					want++
				}
				total := 0
				for i := 0; i < want; i++ {
					if err := db.Transaction(func(tx *gorm.DB) error {
						n, err := RecoverPublishedBatch(tx, now, 15*time.Minute, 1, ai, search)
						total += n
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				if total != want {
					t.Fatalf("replays=%d want=%d", total, want)
				}
				if err := db.Transaction(func(tx *gorm.DB) error {
					n, err := RecoverPublishedBatch(tx, now, 15*time.Minute, 1, ai, search)
					if n != 0 {
						t.Error("recovered disabled route or replayed twice")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestRecoveryExecutionBoundariesAndBudget(t *testing.T) {
	for _, state := range []string{"missing", "running", "expired", "waiting", "retry_due", "dead", "succeeded"} {
		t.Run(state, func(t *testing.T) {
			db, id := newDeliveryFixture(t)
			d := newDispatcher(db, &recordingPublisher{}, slog.Default(), config.Outbox{BatchSize: 1, TaskTimeout: time.Minute})
			item := claimOne(t, d, id)
			if err := d.ackDelivery(item.delivery, item.event); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			old := now.Add(-time.Hour)
			if err := db.Model(&Delivery{}).Where("event_id=?", id).Update("published_at", old).Error; err != nil {
				t.Fatal(err)
			}
			taskID := deliveryTaskID(item.delivery)
			if state == "dead" || state == "succeeded" {
				if err := db.Create(&taskqueue.Result{TaskID: taskID, Status: state, FinishedAt: old}).Error; err != nil {
					t.Fatal(err)
				}
			} else if state != "missing" {
				status := "running"
				lease := now.Add(time.Hour)
				available := old
				if state == "expired" {
					lease = old
				}
				if state == "waiting" || state == "retry_due" {
					status = "retry"
					available = now.Add(time.Hour)
					if state == "retry_due" {
						available = old
					}
				}
				token := uuid.New()
				e := taskqueue.Execution{TaskID: taskID, TaskType: TaskObjectVerifyV1, Queue: "object", PayloadSHA256: "test", Status: status, LeaseToken: &token, LeaseUntil: &lease, AvailableAt: available, RetryCount: 5, MaxRetry: 8, TimeoutMS: 120000, UpdatedAt: old}
				if err := db.Create(&e).Error; err != nil {
					t.Fatal(err)
				}
			}
			replayed := false
			if err := db.Transaction(func(tx *gorm.DB) error {
				_, replayedErr, err := RecoverDelivery(tx, EventObjectVerifyRequested, item.event.AggregateID, now, 15*time.Minute)
				replayed = replayedErr
				return err
			}); err != nil {
				t.Fatal(err)
			}
			want := state == "missing" || state == "expired" || state == "retry_due"
			if replayed != want {
				t.Fatalf("replayed=%v want=%v", replayed, want)
			}
			var got Delivery
			if err := db.Where("event_id=?", id).Take(&got).Error; err != nil {
				t.Fatal(err)
			}
			if want && got.Status != "pending" {
				t.Fatal(got.Status)
			}
			if state == "expired" || state == "retry_due" {
				if got.TaskRetryCount != 5 || got.TaskMaxRetry == nil || *got.TaskMaxRetry != 8 || got.TaskTimeoutMS == nil || *got.TaskTimeoutMS != 120000 {
					t.Fatalf("budget=%+v", got)
				}
			}
			// A repeat scan before relay publication does not manufacture more work.
			if err := db.Transaction(func(tx *gorm.DB) error {
				_, again, err := RecoverDelivery(tx, EventObjectVerifyRequested, item.event.AggregateID, now, 15*time.Minute)
				if again {
					t.Error("replayed twice")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoveryConcurrentScansReplayOnce(t *testing.T) {
	db, id := newDeliveryFixture(t)
	d := newDispatcher(db, &recordingPublisher{}, slog.Default(), config.Outbox{BatchSize: 1, TaskTimeout: time.Minute})
	item := claimOne(t, d, id)
	if err := d.ackDelivery(item.delivery, item.event); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	db.Model(&Delivery{}).Where("event_id=?", id).Update("published_at", now.Add(-time.Hour))
	var count atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- db.Transaction(func(tx *gorm.DB) error {
				_, replayed, err := RecoverDelivery(tx, EventObjectVerifyRequested, item.event.AggregateID, now, 15*time.Minute)
				if replayed {
					count.Add(1)
				}
				return err
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if count.Load() != 1 {
		t.Fatalf("replays=%d", count.Load())
	}
}

func TestRealRabbitLostDeliveryReplaysOriginalTask(t *testing.T) {
	db, id := newDeliveryFixture(t)
	addr := isolatedRabbitURL(t)
	broker := taskqueue.New(taskqueue.Config{URL: addr, Concurrency: 4})
	defer broker.Close()
	broker.ExecutionStore = taskqueue.NewStore(db)
	broker.FailureRecorder = broker.ExecutionStore.RecordFailure
	d := newDispatcher(db, broker, slog.Default(), config.Outbox{BatchSize: 1, TaskTimeout: time.Minute, MaxRetry: 3})
	item := claimOne(t, d, id)
	if err := d.sendDelivery(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	conn, err := amqp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	lost, ok, err := ch.Get("object", true)
	if err != nil || !ok {
		t.Fatalf("lost=%v %v", ok, err)
	}
	now := time.Now().UTC()
	if err := db.Model(&Delivery{}).Where("event_id=?", id).Update("published_at", now.Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		n, err := RecoverPublishedBatch(tx, now, 15*time.Minute, 20, true, true)
		if n != 1 {
			t.Errorf("replayed=%d", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.dispatchBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	got := make(chan string, 2)
	var calls atomic.Int32
	go func() {
		done <- broker.Consume(ctx, map[string]int{"object": 1}, map[string]taskqueue.HandlerFunc{TaskObjectVerifyV1: func(_ context.Context, t *taskqueue.Task) error { calls.Add(1); got <- t.ID; return nil }}, slog.Default())
	}()
	defer func() { cancel(); <-done }()
	select {
	case taskID := <-got:
		if taskID != lost.MessageId {
			t.Fatalf("identity changed %s %s", taskID, lost.MessageId)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no replay")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		db.Model(&taskqueue.Result{}).Where("task_id=? AND status='succeeded'", lost.MessageId).Count(&n)
		if n == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("replayed task missing success receipt")
}

func TestRealRabbitLostDeliveriesAcrossFiveQueues(t *testing.T) {
	db := isolatedOutboxDB(t)
	addr := isolatedRabbitURL(t)
	b := taskqueue.New(taskqueue.Config{URL: addr, Concurrency: 5})
	defer b.Close()
	b.ExecutionStore = taskqueue.NewStore(db)
	d := newDispatcher(db, b, slog.Default(), config.Outbox{BatchSize: 5, TaskTimeout: time.Minute, MaxRetry: 3}).WithSearchEnabled(true)
	now := time.Now().UTC()
	conn, err := amqp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	handlers := map[string]taskqueue.HandlerFunc{}
	got := make(chan taskqueue.Task, 5)
	for _, spec := range []struct{ event, destination string }{
		{EventObjectVerifyRequested, EventObjectVerifyRequested + "@object"},
		{EventObjectReady, EventObjectReady + "@media"},
		{EventObjectGCRequested, EventObjectGCRequested + "@maintenance"},
		{EventAIIndexRequested, EventAIIndexRequested + "@ai"},
		{EventObjectReady, TaskSearchProjectV1 + "@search"},
	} {
		row := publishedRecoveryRow(t, db, spec.event, spec.destination, now.Add(-time.Hour))
		target, _ := targetFromDelivery(row)
		var event Event
		if err := db.Where("id=?", row.EventID).Take(&event).Error; err != nil {
			t.Fatal(err)
		}
		msg := taskqueue.Task{ID: deliveryTaskID(row), Kind: target.TaskType, Queue: target.Queue, Body: payloadWithTrace(event, nil), Timeout: time.Minute, MaxRetry: 3}
		if err := b.Publish(context.Background(), msg); err != nil {
			t.Fatal(err)
		}
		lost, ok, err := ch.Get(target.Queue, true)
		if err != nil || !ok || lost.MessageId != msg.ID {
			t.Fatalf("loss simulation queue=%s ok=%v error=%v", target.Queue, ok, err)
		}
		want[msg.ID] = target.Queue
		handlers[target.TaskType] = func(_ context.Context, msg *taskqueue.Task) error { got <- *msg; return nil }
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		n, err := RecoverPublishedBatch(tx, now, 15*time.Minute, 5, true, true)
		if n != 5 {
			t.Errorf("recovered=%d", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	items, err := d.claimDeliveries(context.Background())
	if err != nil || len(items) != 5 {
		t.Fatalf("claims=%d error=%v", len(items), err)
	}
	for _, item := range items {
		if err := d.sendDelivery(context.Background(), item); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- b.Consume(ctx, map[string]int{"object": 1, "media": 1, "maintenance": 1, "ai": 1, "search": 1}, handlers, slog.Default())
	}()
	defer func() { cancel(); <-done }()
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		select {
		case msg := <-got:
			if want[msg.ID] != msg.Queue || seen[msg.ID] {
				t.Fatalf("changed identity or duplicate: %+v", msg)
			}
			seen[msg.ID] = true
		case <-time.After(10 * time.Second):
			t.Fatal("missing queue replay")
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int64
		if err := db.Model(&taskqueue.Result{}).Where("status='succeeded'").Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		if n == 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("receipts=%d want=5", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Log("removed and replayed 5 real RabbitMQ messages, preserving original canonical TaskIDs and 5 success receipts")
}
