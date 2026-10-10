package taskqueue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestRabbitMQStuckOwnedConsumerHelper(t *testing.T) {
	addr := os.Getenv("TASK_STUCK_CONSUMER_URL")
	if addr == "" {
		t.Skip("subprocess helper")
	}
	b := New(Config{URL: addr, Concurrency: 1, StopTimeout: 40 * time.Millisecond})
	b.ExecutionStore = hardeningStore(t).WithStopTimeout(40 * time.Millisecond)
	if err := b.Consume(context.Background(), map[string]int{"object": 1}, map[string]HandlerFunc{"owned-stuck": func(context.Context, *Task) error { select {} }}, slog.Default()); err != nil {
		t.Fatal(err)
	}
}

func TestRabbitMQStuckConsumerRedeliveryPreservesDurableBudget(t *testing.T) {
	s := hardeningStore(t)
	f := fixture(t)
	b := f.broker(t, 1)
	id := uuid.NewString()
	cleanupTask(t, s, id)
	msg := Task{ID: id, Kind: "owned-stuck", Queue: "object", Body: []byte(`{}`), MaxRetry: 2, Timeout: 50 * time.Millisecond}
	if err := b.Publish(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRabbitMQStuckOwnedConsumerHelper$")
	cmd.Env = append(os.Environ(), "TASK_STUCK_CONSUMER_URL="+f.url)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 70 {
		t.Fatalf("exit=%v output=%s", err, out)
	}
	ch := f.channel(t)
	deadline := time.Now().Add(5 * time.Second)
	for {
		d, ok, err := ch.Get("object", false)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			if d.MessageId != id || !d.Redelivered {
				t.Fatalf("lost original unacknowledged delivery: id=%s redelivered=%v", d.MessageId, d.Redelivered)
			}
			if err := d.Nack(false, true); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stuck consumer delivery did not return")
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.ExecutionStore = s
	got := make(chan Task, 1)
	stop := startConsumer(t, b, map[string]int{"object": 1}, map[string]HandlerFunc{"owned-stuck": func(_ context.Context, msg *Task) error { got <- *msg; return nil }})
	select {
	case next := <-got:
		if next.ID != id || next.RetryCount != 1 || next.MaxRetry != 2 || next.Timeout != 50*time.Millisecond {
			t.Fatalf("redelivery reset durable settings: %+v", next)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("redelivery did not execute after real lease expiry")
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		var n int64
		if err := s.db.Model(&Result{}).Where("task_id=? AND status='succeeded'", id).Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("redelivery missing success receipt")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	t.Log("consumer exit=70; original AMQP delivery returned; real lease expired; next execution retry_count=1 max_retry=2")
}

func TestExecutionCompletionDatabaseFailureNeverACKs(t *testing.T) {
	s := hardeningStore(t)
	observer := hardeningStore(t)
	id := uuid.NewString()
	cleanupTask(t, observer, id)
	b := New(Config{})
	defer b.Close()
	b.ExecutionStore = s
	ack := &acknowledger{}
	pool, _ := s.db.DB()
	delivery := amqp.Delivery{Acknowledger: ack, MessageId: id, Type: "proof", Body: []byte(`{}`)}
	err := b.handleDelivery(context.Background(), "object", delivery, map[string]HandlerFunc{"proof": func(context.Context, *Task) error { return pool.Close() }}, slog.Default())
	if !errors.Is(err, ErrTaskStorage) || ack.ack.Load() != 0 || ack.nack.Load() != 1 {
		t.Fatalf("completion error=%v ack=%d nack=%d", err, ack.ack.Load(), ack.nack.Load())
	}
	var receipts int64
	if err := observer.db.Model(&Result{}).Where("task_id=?", id).Count(&receipts).Error; err != nil {
		t.Fatal(err)
	}
	if receipts != 0 {
		t.Fatal("success recorded despite completion database failure")
	}
}

func TestRabbitMQWeightedDispatchWithSaturatedQueues(t *testing.T) {
	f := fixture(t)
	b := New(Config{URL: f.url, Concurrency: 1, Prefetch: 128, TaskTimeout: time.Minute})
	t.Cleanup(func() { _ = b.Close() })
	entered, release := make(chan struct{}), make(chan struct{})
	got := make(chan string, 70)
	var measured atomic.Int32
	stop := startConsumer(t, b, map[string]int{"media": 4, "object": 2, "maintenance": 1}, map[string]HandlerFunc{"weighted": func(ctx context.Context, msg *Task) error {
		if msg.ID == "block" {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if measured.Add(1) <= 70 {
			got <- msg.Queue
			return nil
		}
		<-ctx.Done()
		return ctx.Err()
	}})
	if err := b.Publish(context.Background(), Task{ID: "block", Kind: "weighted", Queue: "object"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("initial handler not entered")
	}
	for i := 0; i < 100; i++ {
		for _, q := range []string{"media", "object", "maintenance"} {
			if err := b.Publish(context.Background(), Task{ID: fmt.Sprintf("%s-%d", q, i), Kind: "weighted", Queue: q}); err != nil {
				t.Fatal(err)
			}
		}
	}
	ch := f.channel(t)
	deadline := time.Now().Add(10 * time.Second)
	for {
		ready := 0
		for _, q := range []string{"media", "object", "maintenance"} {
			state, err := ch.QueueInspect(q)
			if err != nil {
				t.Fatal(err)
			}
			ready += state.Messages
		}
		if ready == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("prefetch did not fill all pending queues")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	counts := map[string]int{}
	for i := 0; i < 70; i++ {
		counts[await(t, got)]++
	}
	stop()
	if counts["media"] != 40 || counts["object"] != 20 || counts["maintenance"] != 10 {
		t.Fatalf("real dispatch weights=%v", counts)
	}
	t.Logf("real RabbitMQ saturated dispatch: %v (4:2:1), concurrency=1", counts)
}

func hardeningStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN required")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	t.Cleanup(func() { _ = pool.Close() })
	return NewStore(db)
}
func cleanupTask(t *testing.T, s *Store, id string) {
	t.Helper()
	t.Cleanup(func() {
		s.db.Delete(&Execution{}, "task_id=?", id)
		s.db.Delete(&Result{}, "task_id=?", id)
		s.db.Delete(&Failure{}, "task_id=?", id)
	})
}
func TestStoreConcurrentDuplicateAndIdentityConflict(t *testing.T) {
	s := hardeningStore(t)
	id := uuid.NewString()
	cleanupTask(t, s, id)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	var calls atomic.Int32
	h := s.Middleware()(HandlerFunc(func(context.Context, *Task) error { calls.Add(1); close(entered); <-release; return nil }))
	msg := Task{ID: id, Kind: "proof", Queue: "object", Body: []byte(`{"same":true}`), MaxRetry: 3, Timeout: time.Minute}
	go func() { copy := msg; done <- h.ProcessTask(context.Background(), &copy) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler not entered")
	}
	copy := msg
	err := h.ProcessTask(context.Background(), &copy)
	close(release)
	if got := <-done; got != nil {
		t.Fatal(got)
	}
	if !errors.Is(err, ErrTaskBusy) || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
	copy = msg
	if err := h.ProcessTask(context.Background(), &copy); err != nil {
		t.Fatal(err)
	}
	copy = msg
	copy.Body = []byte(`{"different":true}`)
	if err := h.ProcessTask(context.Background(), &copy); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("identity conflict=%v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate side effect")
	}
}
func TestStoreRetryBudgetLeaseFenceAndShutdown(t *testing.T) {
	s := hardeningStore(t)
	msg := Task{ID: uuid.NewString(), Kind: "proof", Queue: "object", Body: []byte(`{}`), MaxRetry: 3, Timeout: time.Minute}
	cleanupTask(t, s, msg.ID)
	if err := s.claim(context.Background(), &msg); err != nil {
		t.Fatal(err)
	}
	old := msg
	if err := s.db.Model(&Execution{}).Where("task_id=?", msg.ID).Update("lease_until", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.claim(context.Background(), &msg); err != nil {
		t.Fatal(err)
	}
	if err := s.finish(context.Background(), &old, nil); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old finish=%v", err)
	}
	if err := s.finish(context.Background(), &msg, errors.New("temporary")); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&Execution{}).Where("task_id=?", msg.ID).Update("available_at", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	msg.RetryCount = 0
	msg.MaxRetry = 100
	if err := s.claim(context.Background(), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.RetryCount != 1 || msg.MaxRetry != 3 {
		t.Fatalf("budget reset: %+v", msg)
	}
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	persist := context.WithValue(context.Background(), deliveryContextKey{}, parent)
	if err := s.finish(persist, &msg, context.Canceled); err != nil {
		t.Fatal(err)
	}
	if err := s.claim(context.Background(), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.RetryCount != 1 {
		t.Fatal("shutdown spent retry")
	}
	if err := s.finish(context.Background(), &msg, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordFailure(context.Background(), msg, errors.New("late failure")); err != nil {
		t.Fatal(err)
	}
	var failures int64
	s.db.Model(&Failure{}).Where("task_id=?", msg.ID).Count(&failures)
	if failures != 0 {
		t.Fatal("late failure polluted success")
	}
}
func TestWeightedSchedulerFairnessAndIdleWakeup(t *testing.T) {
	s := newDeliveryScheduler(map[string]int{"media": 4, "maintenance": 1, "object": 2})
	for i := 0; i < 100; i++ {
		for _, q := range []string{"media", "maintenance", "object"} {
			s.put(q, amqp.Delivery{MessageId: q})
		}
	}
	counts := map[string]int{}
	for i := 0; i < 70; i++ {
		job, ok := s.take(context.Background())
		if !ok {
			t.Fatal("empty")
		}
		counts[job.queue]++
	}
	if counts["media"] != 40 || counts["object"] != 20 || counts["maintenance"] != 10 {
		t.Fatalf("weights=%v", counts)
	}
	idle := newDeliveryScheduler(map[string]int{"object": 1})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		go func() { _, ok := idle.take(ctx); got <- ok }()
	}
	for i := 0; i < 8; i++ {
		idle.put("object", amqp.Delivery{})
	}
	for i := 0; i < 8; i++ {
		if !<-got {
			t.Fatal("idle worker stranded")
		}
	}
}
func TestStuckHandlerProcessHelper(t *testing.T) {
	if os.Getenv("TASK_STUCK_HELPER") == "" {
		t.Skip("subprocess helper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	invokeBounded(ctx, &Task{ID: "stuck", Kind: "proof"}, func(context.Context, *Task) error { select {} }, 30*time.Millisecond)
	t.Fatal("stuck handler returned")
}
func TestStuckHandlerTerminatesProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStuckHandlerProcessHelper$")
	cmd.Env = append(os.Environ(), "TASK_STUCK_HELPER=1")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 70 {
		t.Fatalf("exit=%v output=%s", err, out)
	}
}
func TestRabbitMQStoreSuppressesConcurrentDuplicate(t *testing.T) {
	s := hardeningStore(t)
	f := fixture(t)
	b := f.broker(t, 4)
	id := uuid.NewString()
	cleanupTask(t, s, id)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	b.ExecutionStore = s
	h := HandlerFunc(func(ctx context.Context, _ *Task) error {
		calls.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	stop := startConsumer(t, b, map[string]int{"object": 1}, map[string]HandlerFunc{"proof": h.ProcessTask})
	msg := Task{ID: id, Kind: "proof", Queue: "object", Body: []byte(`{}`), MaxRetry: 2, Timeout: time.Minute}
	for i := 0; i < 8; i++ {
		if err := b.Publish(context.Background(), msg); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no handler")
	}
	time.Sleep(150 * time.Millisecond)
	close(release)
	deadline := time.Now().Add(6 * time.Second)
	receipt := false
	for time.Now().Before(deadline) {
		var n int64
		s.db.Model(&Result{}).Where("task_id=? AND status='succeeded'", id).Count(&n)
		if n == 1 {
			receipt = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(1300 * time.Millisecond)
	stop()
	if !receipt {
		t.Fatal("duplicate test missing success receipt")
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent calls=%d", calls.Load())
	}
}

func TestStuckStoreHelper(t *testing.T) {
	if os.Getenv("TASK_STUCK_STORE_ID") == "" {
		t.Skip("subprocess helper")
	}
	s := hardeningStore(t).WithStopTimeout(30 * time.Millisecond)
	msg := Task{ID: os.Getenv("TASK_STUCK_STORE_ID"), Kind: "stuck", Queue: "object", Body: []byte(`{}`), Timeout: 20 * time.Millisecond, MaxRetry: 2}
	h := s.Middleware()(HandlerFunc(func(context.Context, *Task) error { select {} }))
	if err := h.ProcessTask(context.Background(), &msg); err != nil {
		t.Fatal(err)
	}
	t.Fatal("stuck store returned")
}
func TestStuckStorePreservesBudgetBeforeExit(t *testing.T) {
	s := hardeningStore(t)
	id := uuid.NewString()
	cleanupTask(t, s, id)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStuckStoreHelper$")
	cmd.Env = append(os.Environ(), "TASK_STUCK_STORE_ID="+id)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 70 {
		t.Fatalf("exit=%v output=%s", err, out)
	}
	var row Execution
	if err := s.db.Where("task_id=?", id).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.RetryCount != 1 || row.MaxRetry != 2 || row.LeaseUntil == nil {
		t.Fatalf("budget lost: %+v", row)
	}
	if err := s.db.Model(&Execution{}).Where("task_id=?", id).Updates(map[string]any{"lease_until": time.Now().Add(-time.Second), "available_at": time.Now().Add(-time.Second)}).Error; err != nil {
		t.Fatal(err)
	}
	msg := Task{ID: id, Kind: "stuck", Queue: "object", Body: []byte(`{}`), Timeout: 20 * time.Millisecond, MaxRetry: 99}
	if err := s.claim(context.Background(), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.RetryCount != 1 || msg.MaxRetry != 2 {
		t.Fatal("restart reset budget")
	}
}
func TestStoreRenewalRejectsLostLease(t *testing.T) {
	s := hardeningStore(t).WithStopTimeout(time.Millisecond)
	msg := Task{ID: uuid.NewString(), Kind: "proof", Queue: "object", Body: []byte(`{}`), Timeout: time.Minute, MaxRetry: 2}
	cleanupTask(t, s, msg.ID)
	if err := s.claim(context.Background(), &msg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	done := make(chan struct{})
	go s.renew(ctx, msg, cancel, done)
	if err := s.db.Model(&Execution{}).Where("task_id=?", msg.ID).Update("lease_until", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), ErrLeaseLost) {
			t.Fatal(context.Cause(ctx))
		}
	case <-time.After(8 * time.Second):
		t.Fatal("lost lease not canceled")
	}
	<-done
	if err := s.finish(context.Background(), &msg, nil); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired completion=%v", err)
	}
}
func TestExecutionDatabaseFailureNeverACKs(t *testing.T) {
	s := hardeningStore(t)
	pool, _ := s.db.DB()
	pool.Close()
	b := New(Config{})
	defer b.Close()
	b.ExecutionStore = s
	ack := &acknowledger{}
	delivery := amqp.Delivery{Acknowledger: ack, MessageId: uuid.NewString(), Type: "proof"}
	err := b.handleDelivery(context.Background(), "object", delivery, map[string]HandlerFunc{"proof": func(context.Context, *Task) error { t.Error("handler ran with DB unavailable"); return nil }}, nil)
	if !errors.Is(err, ErrTaskStorage) || ack.ack.Load() != 0 || ack.nack.Load() != 1 {
		t.Fatalf("err=%v ack=%d nack=%d", err, ack.ack.Load(), ack.nack.Load())
	}
}
