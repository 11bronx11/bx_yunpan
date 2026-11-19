package aisystem

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type directCrashFixture struct {
	Task         asynccontrol.Task
	Mode, Prefix string
}

type directFaultHandler struct {
	publisher *Publisher
	exec      func(context.Context, asynccontrol.Task) (asynccontrol.Completion, *time.Time, error)
}

func (h directFaultHandler) Lookup(ctx context.Context, task asynccontrol.Task) (asynccontrol.Completion, bool, error) {
	return h.publisher.Lookup(ctx, task)
}

func (h directFaultHandler) Execute(ctx context.Context, task asynccontrol.Task) (asynccontrol.Completion, *time.Time, error) {
	return h.exec(ctx, task)
}

func directFaultDSN(t *testing.T, db *gorm.DB, key, application string) string {
	t.Helper()
	var schema string
	if err := db.Raw("SELECT current_schema()").Scan(&schema).Error; err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv(key))
	if err != nil {
		t.Fatal("invalid dedicated database URL")
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	q.Set("options", "-c bx.coordination_protocol=v3 -c bx.ai_protocol=v3")
	q.Set("application_name", application)
	u.RawQuery = q.Encode()
	return u.String()
}

func directSourceValue(task asynccontrol.Task) *ComputedValue {
	return &ComputedValue{OutputID: uuid.NewSHA1(task.ID, []byte("output:"+task.InputDigest)), Value: sourceData{Parsed: ai.ParseResult{Sections: []ai.Section{{Text: "v3 SIGKILL receipt recovery", Kind: "paragraph"}}}, Coverage: "complete"}, RowCount: 1, Coverage: "complete"}
}

// This helper is a separate OS process. The parent observes a database lock,
// a post-COMMIT marker or a real AMQP ACK before sending SIGKILL.
func TestDirectAICrashProcessHelper(t *testing.T) {
	path := os.Getenv("AI_DIRECT_CRASH_FIXTURE")
	if path == "" {
		return
	}
	var f directCrashFixture
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &f) != nil {
		t.Fatal("invalid direct crash fixture")
	}
	open := func(key string) *gorm.DB {
		db, err := gorm.Open(postgres.Open(os.Getenv(key)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatal("child test database unavailable")
		}
		pool, _ := db.DB()
		t.Cleanup(func() { _ = pool.Close() })
		return db
	}
	s := asynccontrol.New(open("AI_DIRECT_CONTROL_DSN"))
	s.ProtocolVersion = 3
	s.Lease = 3 * time.Second
	Register(s.Registry)
	planner := &Planner{}
	s.PlanCompleted, s.Observe, s.AfterTerminal = planner.Completed, planner.Observe, planner.Finish
	p := &Publisher{DB: open("AI_DIRECT_AI_DSN"), Control: s}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if f.Mode == "before-ack" {
		b := asynccontrol.NewBroker(os.Getenv("AI_DIRECT_BROKER"), f.Prefix)
		defer b.Close()
		h := directFaultHandler{publisher: p, exec: func(ctx context.Context, task asynccontrol.Task) (asynccontrol.Completion, *time.Time, error) {
			c, err := p.ApplyDirect(ctx, task, directSourceValue(task))
			return c, nil, err
		}}
		_ = s.RunDirectWorker(ctx, b, f.Task.Queue, 1, h, func(error) bool { return false }, slog.New(slog.NewTextHandler(io.Discard, nil)))
		t.Fatal("parent did not kill direct worker")
	}
	if _, err := p.ApplyDirect(ctx, f.Task, directSourceValue(f.Task)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".published", []byte("committed"), 0o600); err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
	t.Fatal("parent did not kill direct publisher")
}

func TestDirectAIProcessSIGKILLWindows(t *testing.T) {
	for _, window := range []string{"before-receipt", "committed-before-complete", "before-ack"} {
		t.Run(window, func(t *testing.T) {
			db, s := directDatabases(t)
			s.Lease = 3 * time.Second
			s.RetryBase = time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			in := testInput()
			p := &Publisher{DB: db, Control: s}
			applyDirectIntent(t, p, in)
			sp := spec("ai.source.prepare", in)
			sp.MaxAttempts = 1
			task, err := s.Ensure(ctx, sp, "sigkill")
			if err != nil {
				t.Fatal(err)
			}
			if window != "before-ack" {
				var ok bool
				task, ok, err = s.Claim(ctx, task.Notice())
				if err != nil || !ok {
					t.Fatal(ok, err)
				}
			}
			if window == "before-receipt" {
				if err := db.Exec(`CREATE FUNCTION direct_receipt_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(692310019); RETURN NEW; END $$; CREATE TRIGGER direct_receipt_barrier BEFORE INSERT ON ai_commit_receipts FOR EACH ROW EXECUTE FUNCTION direct_receipt_barrier(); SELECT pg_advisory_lock(692310019)`).Error; err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Exec("SELECT pg_advisory_unlock(692310019)").Error })
			}
			b := ackBroker(t)
			brokerURL, ackReached, closeGate := ackGate(t, true)
			if window == "before-ack" {
				if err := b.Publish(ctx, b.WorkQueue(task.Queue), task.Notice()); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(t.TempDir(), "direct.json")
			raw, _ := json.Marshal(directCrashFixture{Task: task, Mode: window, Prefix: b.Prefix})
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			app := "direct-crash-" + task.ID.String()
			child := exec.CommandContext(ctx, executable, "-test.run=^TestDirectAICrashProcessHelper$")
			child.Env = append(os.Environ(), "AI_DIRECT_CRASH_FIXTURE="+path, "AI_DIRECT_AI_DSN="+directFaultDSN(t, db, "TEST_AI_POSTGRES_DSN", app), "AI_DIRECT_CONTROL_DSN="+directFaultDSN(t, s.DB, "TEST_CONTROL_POSTGRES_DSN", app), "AI_DIRECT_BROKER="+brokerURL)
			var output strings.Builder
			child.Stdout, child.Stderr = &output, &output
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = child.Process.Kill() })
			reached := false
			for until := time.Now().Add(10 * time.Second); time.Now().Before(until) && !reached; {
				switch window {
				case "before-receipt":
					var n int64
					if err := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event='advisory'", app).Scan(&n).Error; err != nil {
						t.Fatal(err)
					}
					reached = n == 1
				case "before-ack":
					select {
					case <-ackReached:
						reached = true
					default:
					}
				default:
					_, err := os.Stat(path + ".published")
					reached = err == nil
				}
				if !reached {
					asynccontrol.Pause(ctx, 10*time.Millisecond)
				}
			}
			_ = child.Process.Kill()
			killErr := child.Wait()
			closeGate()
			if !reached || killErr == nil {
				t.Fatalf("SIGKILL barrier not reached: %s", output.String())
			}
			if window == "before-receipt" {
				if err := db.Exec("SELECT pg_advisory_unlock(692310019); DROP TRIGGER direct_receipt_barrier ON ai_commit_receipts").Error; err != nil {
					t.Fatal(err)
				}
			}
			var receipts, sources int64
			if err := db.Table("ai_commit_receipts").Where("task_id=?", task.ID).Count(&receipts).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Table("ai_sources").Count(&sources).Error; err != nil {
				t.Fatal(err)
			}
			want := int64(1)
			if window == "before-receipt" {
				want = 0
			}
			if receipts != want || sources != want {
				t.Fatal("crash leaked partial publication", receipts, sources)
			}
			if window != "before-ack" {
				if err := s.DB.Model(&asynccontrol.Task{}).Where("id=?", task.ID).Update("lease_until", time.Now().Add(-time.Second)).Error; err != nil {
					t.Fatal(err)
				}
				if err := s.Recover(ctx, 100); err != nil {
					t.Fatal(err)
				}
				var current asynccontrol.Task
				if err := s.DB.First(&current, "id=?", task.ID).Error; err != nil {
					t.Fatal(err)
				}
				if current.Phase != "reconcile" || current.Attempts != 1 {
					t.Fatal("last crash did not become read-only recovery", current.Phase, current.Attempts)
				}
				if err := b.Publish(ctx, b.WorkQueue(task.Queue), current.Notice()); err != nil {
					t.Fatal(err)
				}
			}
			replayURL, replayAck, closeReplay := ackGate(t, false)
			defer closeReplay()
			replayBroker := asynccontrol.NewBroker(replayURL, b.Prefix)
			defer replayBroker.Close()
			var recomputes atomic.Int32
			h := directFaultHandler{publisher: p, exec: func(context.Context, asynccontrol.Task) (asynccontrol.Completion, *time.Time, error) {
				recomputes.Add(1)
				return asynccontrol.Completion{}, nil, fmt.Errorf("recovery must only read receipts")
			}}
			live, stop := context.WithCancel(ctx)
			finished := make(chan error, 1)
			go func() {
				finished <- s.RunDirectWorker(live, replayBroker, task.Queue, 1, h, func(error) bool { return false }, slog.New(slog.NewTextHandler(io.Discard, nil)))
			}()
			select {
			case <-replayAck:
			case <-ctx.Done():
				stop()
				<-finished
				t.Fatal("crashed notification was not recovered and ACKed")
			}
			stop()
			<-finished
			var current asynccontrol.Task
			if err := s.DB.First(&current, "id=?", task.ID).Error; err != nil {
				t.Fatal(err)
			}
			wantState, wantOutcome := asynccontrol.Done, asynccontrol.Succeeded
			if window == "before-receipt" {
				wantState, wantOutcome = asynccontrol.Stopped, asynccontrol.NeedsAttention
			}
			if current.State != wantState || current.Outcome != wantOutcome || current.Attempts != 1 || recomputes.Load() != 0 {
				t.Fatal("incorrect crash recovery", current.State, current.Outcome, current.Attempts, recomputes.Load())
			}
			var children int64
			if err := s.DB.Table("tasks").Where("parent_id=?", task.ID).Count(&children).Error; err != nil {
				t.Fatal(err)
			}
			if children != 2*want {
				t.Fatal("crash duplicated or released uncommitted successors", children)
			}
			t.Log("SIGKILL, receipt visibility, bounded recovery, successor count and actual ACK verified")
		})
	}
}
