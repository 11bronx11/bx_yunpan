package asynccontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/testfault"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestDeadLetterRecoveryWithSingleConnection(t *testing.T) {
	for _, protocol := range []int{2, 3} {
		t.Run(fmt.Sprintf("v%d", protocol), func(t *testing.T) {
			s, _ := testStore(t)
			if protocol == 3 {
				s.ProtocolVersion = 3
				if err := s.DB.Exec("SET bx.coordination_protocol='v3'; UPDATE control_settings SET value='v3' WHERE key='coordination_protocol'").Error; err != nil {
					t.Fatal(err)
				}
			}
			task, err := s.Ensure(context.Background(), spec("dead-single-connection"), "test")
			if err != nil {
				t.Fatal(err)
			}
			reserved, ok, err := s.Reserve(context.Background(), task.Queue)
			if err != nil || !ok {
				t.Fatal(ok, err)
			}
			if err := s.Published(context.Background(), reserved, nil); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(reserved.Notice())
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := s.HandleDead(ctx, reserved.Notice(), raw); err != nil {
				t.Fatal("dead-letter handler needs a second connection or failed recovery", err)
			}
			var current Task
			if err := s.DB.First(&current, "id=?", task.ID).Error; err != nil {
				t.Fatal(err)
			}
			if current.State != Pending || current.DispatchGeneration != reserved.DispatchGeneration+1 || current.Dispatches != 1 || current.Attempts != 0 {
				t.Fatal("recovery changed execution budget or lost durable delivery", current.State, current.DispatchGeneration, current.Dispatches, current.Attempts)
			}
			if err := s.HandleDead(context.Background(), reserved.Notice(), raw); err != nil {
				t.Fatal("duplicate dead message", err)
			}
			var isolated int64
			if err := s.DB.Table("quarantines").Count(&isolated).Error; err != nil || isolated != 1 {
				t.Fatal("dead message evidence is not deduplicated", isolated, err)
			}
		})
	}
}

func TestDirectDeadLetterRecoveryAndEvidenceRollbackTogether(t *testing.T) {
	s := directStore(t)
	ctx := context.Background()
	task, err := s.Ensure(ctx, spec("dead-rollback"), "test")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(task.Notice())
	if err := s.DB.Exec(`CREATE FUNCTION reject_dead_recovery() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected recovery failure'; END $$; CREATE TRIGGER reject_dead_recovery BEFORE UPDATE ON tasks FOR EACH ROW EXECUTE FUNCTION reject_dead_recovery()`).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.HandleDead(ctx, task.Notice(), raw); err == nil {
		t.Fatal("failed recovery was acknowledged as durable")
	}
	var n int64
	if err := s.DB.Table("quarantines").Count(&n).Error; err != nil || n != 0 {
		t.Fatal("evidence escaped recovery rollback", n, err)
	}
	if err := s.DB.Exec("DROP TRIGGER reject_dead_recovery ON tasks").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.HandleDead(ctx, task.Notice(), raw); err != nil {
		t.Fatal(err)
	}
}

func TestDirectDeadLetterRejectsOtherProtocol(t *testing.T) {
	s := directStore(t)
	ctx := context.Background()
	task, err := s.Ensure(ctx, spec("dead-other-protocol"), "test")
	if err != nil {
		t.Fatal(err)
	}
	notice := task.Notice()
	notice.Version = 2
	raw, _ := json.Marshal(notice)
	if err := s.HandleDead(ctx, notice, raw); err != nil {
		t.Fatal(err)
	}
	var current Task
	if err := s.DB.First(&current, "id=?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.DispatchGeneration != task.DispatchGeneration || current.Version != task.Version || current.LastError != "" {
		t.Fatal("other protocol's dead notice changed a v3 task", current.DispatchGeneration, current.LastError)
	}
}

type faultDirectHandler struct {
	lookup  func(context.Context, Task) (Completion, bool, error)
	execute func(context.Context, Task) (Completion, *time.Time, error)
}

func (h faultDirectHandler) Lookup(ctx context.Context, task Task) (Completion, bool, error) {
	return h.lookup(ctx, task)
}

func (h faultDirectHandler) Execute(ctx context.Context, task Task) (Completion, *time.Time, error) {
	return h.execute(ctx, task)
}

func faultControlPool(t *testing.T, s *Store, address string) *gorm.DB {
	t.Helper()
	var schema string
	if err := s.DB.Raw("SELECT current_schema()").Scan(&schema).Error; err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("TEST_CONTROL_POSTGRES_DSN"))
	if err != nil {
		t.Fatal("invalid test control DSN")
	}
	if address != "" {
		u.Host = address
	}
	q := u.Query()
	q.Set("search_path", schema)
	q.Set("options", "-c bx.coordination_protocol=v3")
	q.Set("connect_timeout", "1")
	u.RawQuery = q.Encode()
	db, err := gorm.Open(postgres.Open(u.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("test control pool unavailable")
	}
	pool, _ := db.DB()
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	return db
}

func awaitDirectTask(t *testing.T, ctx context.Context, s *Store, task Task, state string) Task {
	t.Helper()
	for ctx.Err() == nil {
		var current Task
		if err := s.DB.WithContext(ctx).First(&current, "id=?", task.ID).Error; err != nil {
			t.Fatal(err)
		}
		if current.State == state {
			return current
		}
		Pause(ctx, 10*time.Millisecond)
	}
	t.Fatal("task did not reach", state)
	return Task{}
}

func TestDirectRealWorkersRetryWithoutRepeatingCommittedEffects(t *testing.T) {
	s, business := testStore(t)
	s.ProtocolVersion = 3
	if err := s.DB.Exec("SET bx.coordination_protocol='v3'; UPDATE control_settings SET value='v3' WHERE key='coordination_protocol'").Error; err != nil {
		t.Fatal(err)
	}
	if err := business.Exec("CREATE TABLE direct_effects(task_id uuid PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	s.Critical = faultControlPool(t, s, "")
	s.Lease = 3 * time.Second
	b := testBroker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var executions atomic.Int32
	h := faultDirectHandler{
		lookup: func(ctx context.Context, task Task) (Completion, bool, error) {
			var n int64
			err := business.WithContext(ctx).Table("direct_effects").Where("task_id=?", task.ID).Count(&n).Error
			return completion(task), n == 1, err
		},
		execute: func(ctx context.Context, task Task) (Completion, *time.Time, error) {
			if executions.Add(1) == 1 {
				return Completion{}, nil, errors.New("injected temporary provider outage")
			}
			if err := business.WithContext(ctx).Exec("INSERT INTO direct_effects VALUES(?)", task.ID).Error; err != nil {
				return Completion{}, nil, err
			}
			return Completion{}, nil, errors.New("injected lost response after domain commit")
		},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	finished := make(chan error, 3)
	for range 2 {
		go func() { finished <- s.RunDirectWorker(ctx, b, "upload", 1, h, func(error) bool { return false }, log) }()
	}
	go func() { finished <- s.RunScheduler(ctx, b, "fault-publisher", log) }()
	defer func() {
		cancel()
		for range 3 {
			<-finished
		}
	}()
	task, err := s.Ensure(ctx, spec("real-direct-retries"), "first")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		duplicate, err := s.Ensure(ctx, spec("real-direct-retries"), fmt.Sprint(i))
		if err != nil || duplicate.ID != task.ID {
			t.Fatal("source duplication changed task", err)
		}
		if err := b.Publish(ctx, b.WorkQueue(task.Queue), task.Notice()); err != nil {
			t.Fatal(err)
		}
	}
	current := awaitDirectTask(t, ctx, s, task, Done)
	var effects int64
	if err := business.Table("direct_effects").Count(&effects).Error; err != nil {
		t.Fatal(err)
	}
	if current.Outcome != Succeeded || current.Attempts != 3 || executions.Load() != 2 || effects != 1 || s.ReceiptHits.Load() != 1 {
		t.Fatal("retry or duplicate delivery repeated effects", current.Attempts, executions.Load(), effects, s.ReceiptHits.Load())
	}
}

func TestDirectCriticalPoolSurvivesOrdinaryPoolExhaustion(t *testing.T) {
	s := directStore(t)
	s.Critical = faultControlPool(t, s, "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	task, err := s.Ensure(ctx, spec("critical-pool"), "test")
	if err != nil {
		t.Fatal(err)
	}
	task = dispatch(t, s, task)
	pool, _ := s.DB.DB()
	held, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	blocked, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	if err := s.DB.WithContext(blocked).Exec("SELECT 1").Error; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("normal pool was not saturated", err)
	}
	if err := s.Renew(ctx, task); err != nil {
		t.Fatal("reserved renewal connection starved", err)
	}
	if err := s.Complete(ctx, task, completion(task)); err != nil {
		t.Fatal("reserved confirmation connection starved", err)
	}
}

func TestDirectControlConnectionCutAfterDomainCommit(t *testing.T) {
	base, business := testStore(t)
	base.ProtocolVersion = 3
	if err := base.DB.Exec("SET bx.coordination_protocol='v3'; UPDATE control_settings SET value='v3' WHERE key='coordination_protocol'").Error; err != nil {
		t.Fatal(err)
	}
	if err := business.Exec("CREATE TABLE direct_effects(task_id uuid PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(os.Getenv("TEST_CONTROL_POSTGRES_DSN"))
	proxy := testfault.NewTCP(t, u.Host)
	s := New(faultControlPool(t, base, proxy.Address))
	s.ProtocolVersion = 3
	s.Critical = faultControlPool(t, base, proxy.Address)
	s.Lease = time.Second
	s.RetryBase = time.Millisecond
	b := testBroker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	var effects atomic.Int32
	committed := make(chan struct{})
	h := faultDirectHandler{
		lookup: func(ctx context.Context, task Task) (Completion, bool, error) {
			var count int64
			err := business.WithContext(ctx).Table("direct_effects").Where("task_id=?", task.ID).Count(&count).Error
			return completion(task), count == 1, err
		},
		execute: func(ctx context.Context, task Task) (Completion, *time.Time, error) {
			if err := business.WithContext(ctx).Exec("INSERT INTO direct_effects VALUES(?)", task.ID).Error; err != nil {
				return Completion{}, nil, err
			}
			effects.Add(1)
			proxy.Cut()
			close(committed)
			return completion(task), nil, nil
		},
	}
	task, err := s.Ensure(ctx, spec("control-cut"), "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(ctx, b.WorkQueue(task.Queue), task.Notice()); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		finished <- s.RunDirectWorker(ctx, b, task.Queue, 1, h, func(error) bool { return false }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	defer func() { cancel(); <-finished }()
	select {
	case <-committed:
	case <-ctx.Done():
		t.Fatal("domain commit barrier not reached")
	}
	var current Task
	if err := base.DB.First(&current, "id=?", task.ID).Error; err != nil || current.State != Running {
		t.Fatal("control failure fabricated completion", current.State, err)
	}
	for s.CompletionErrors.Load() == 0 && ctx.Err() == nil {
		Pause(ctx, time.Millisecond)
	}
	if s.CompletionErrors.Load() == 0 {
		t.Fatal("control confirmation did not encounter the injected outage")
	}
	proxy.Restore()
	if err := base.DB.Model(&Task{}).Where("id=?", task.ID).Update("lease_until", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if err := base.Recover(ctx, 10); err != nil {
		t.Fatal(err)
	}
	Pause(ctx, 5*time.Millisecond)
	reserved, ok, err := base.Reserve(ctx, task.Queue)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := b.Publish(ctx, b.WorkQueue(task.Queue), reserved.Notice()); err != nil {
		t.Fatal(err)
	}
	current = awaitDirectTask(t, ctx, base, task, Done)
	if current.Outcome != Succeeded || effects.Load() != 1 || current.Attempts != 2 {
		t.Fatal("lost control connection repeated committed effect", current.State, effects.Load(), current.Attempts)
	}
}

func TestDirectMQOutageAndDeadLetterRecovery(t *testing.T) {
	s := directStore(t)
	s.Lease = 3 * time.Second
	s.RetryBase = 100 * time.Millisecond
	b := testBroker(t)
	u, _ := url.Parse(os.Getenv("TEST_RABBITMQ_URL"))
	proxy := testfault.NewTCP(t, u.Host)
	u.Host = proxy.Address
	failedBroker := NewBroker(u.String(), b.Prefix)
	failedBroker.Timeout = 500 * time.Millisecond
	defer failedBroker.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	task, err := s.Ensure(ctx, spec("mq-faults"), "test")
	if err != nil {
		t.Fatal(err)
	}
	reserved, ok, err := s.Reserve(ctx, task.Queue)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	proxy.Cut()
	publishErr := failedBroker.Publish(ctx, failedBroker.WorkQueue(task.Queue), reserved.Notice())
	if publishErr == nil {
		t.Fatal("MQ connection fault did not prevent publication")
	}
	if err := s.Published(ctx, reserved, publishErr); err != nil {
		t.Fatal(err)
	}
	var current Task
	if err := s.DB.First(&current, "id=?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.State != Pending || current.DispatchState != "pending" || current.Dispatches != 1 || current.Attempts != 0 {
		t.Fatal("MQ failure lost durable retry or spent execution budget")
	}
	proxy.Restore()
	Pause(ctx, 120*time.Millisecond)
	reserved, ok, err = s.Reserve(ctx, task.Queue)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := failedBroker.Publish(ctx, b.WorkQueue(task.Queue), reserved.Notice()); err != nil {
		t.Fatal(err)
	}
	if err := s.Published(ctx, reserved, nil); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := s.Recover(ctx, 100); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := s.Reserve(ctx, task.Queue); err != nil || ok {
			t.Fatal("confirmed queue wait was republished", ok, err)
		}
	}
	// Inject an exhausted transport notification into the real dead queue. The
	// original work notice remains queued and must become harmlessly obsolete.
	if err := b.Publish(ctx, b.Prefix+".dead", reserved.Notice()); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	finished := make(chan error, 3)
	go func() { finished <- s.RunDeadLetters(ctx, b, log) }()
	for ctx.Err() == nil {
		if err := s.DB.First(&current, "id=?", task.ID).Error; err != nil {
			cancel()
			<-finished
			t.Fatal(err)
		}
		if current.DispatchGeneration > reserved.DispatchGeneration {
			break
		}
		Pause(ctx, 10*time.Millisecond)
	}
	var executes atomic.Int32
	h := faultDirectHandler{
		lookup: func(_ context.Context, task Task) (Completion, bool, error) {
			return completion(task), executes.Load() > 0, nil
		},
		execute: func(_ context.Context, task Task) (Completion, *time.Time, error) {
			executes.Add(1)
			return completion(task), nil, nil
		},
	}
	go func() {
		finished <- s.RunDirectWorker(ctx, b, task.Queue, 1, h, func(error) bool { return false }, log)
	}()
	go func() { finished <- s.RunScheduler(ctx, b, "dead-recovery", log) }()
	defer func() {
		cancel()
		for range 3 {
			<-finished
		}
	}()
	current = awaitDirectTask(t, ctx, s, task, Done)
	if current.Attempts != 1 || current.Dispatches != 3 || executes.Load() != 1 {
		t.Fatal("transport recovery refreshed budget or executed stale notification", current.Attempts, current.Dispatches, executes.Load())
	}
}
