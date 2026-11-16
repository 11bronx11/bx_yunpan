package aisystem

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type crashPayload struct {
	Task   asynccontrol.Task
	Result asynccontrol.Result
}

// B06: independent PostgreSQL sessions race on the same accepted result.
func TestConcurrentAICommittersPublishOneReceipt(t *testing.T) {
	db, store := databases(t)
	in := testInput()
	seedObject(t, db, in)
	task := claim(t, store, "ai.source.prepare", in)
	result, _ := stage(t, db, store, task, in, sourceData{Parsed: ai.ParseResult{Sections: []rag.Section{{Text: "concurrent commit", Kind: "text"}}}, Coverage: "complete"}, uuid.New(), 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	errs := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		pool, err := gorm.Open(postgres.Open(scopedAIConnection(t, db)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatal("independent connection unavailable")
		}
		sql, _ := pool.DB()
		t.Cleanup(func() { _ = sql.Close() })
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			outcome, err := (&Publisher{DB: pool, Control: store}).Publish(ctx, task, result)
			if err == nil && outcome != asynccontrol.Succeeded {
				err = asynccontrol.ErrConflict
			}
			errs <- err
		}()
	}
	close(start)
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := store.Report(ctx, task, result, asynccontrol.Succeeded); err != nil {
			t.Fatal(err)
		}
	}
	var receipts, sources, children int64
	if err := db.Table("ai_commit_receipts").Count(&receipts).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("ai_sources").Count(&sources).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB.Table("tasks").Where("parent_id=?", task.ID).Count(&children).Error; err != nil {
		t.Fatal(err)
	}
	if receipts != 1 || sources != 1 || children != 2 {
		t.Fatal("duplicate publication or successors", receipts, sources, children)
	}
}

// Invoked only by TestAICommitterSIGKILLWindows in a fresh OS process. No
// production hooks or alternate task authority are installed.
func TestAICrashProcessHelper(t *testing.T) {
	fixture := os.Getenv("AI_DEEP_CRASH_FIXTURE")
	if fixture == "" {
		return
	}
	var payload crashPayload
	b, err := os.ReadFile(fixture)
	if err != nil || json.Unmarshal(b, &payload) != nil {
		t.Fatal("invalid child fixture")
	}
	db, err := gorm.Open(postgres.Open(os.Getenv("AI_DEEP_CRASH_DSN")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("child database unavailable")
	}
	pool, _ := db.DB()
	defer pool.Close()
	publisher := Publisher{DB: db}
	if _, err := publisher.Publish(context.Background(), payload.Task, payload.Result); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture+".published", []byte("published"), 0600); err != nil {
		t.Fatal(err)
	}
	// The parent kills us at this exact post-commit/pre-Report barrier.
	select {
	case <-time.After(25 * time.Second):
		t.Fatal("parent failed to kill child")
	}
}

func scopedAIConnection(t *testing.T, db *gorm.DB) string {
	t.Helper()
	var schema string
	if err := db.Raw("SELECT current_schema()").Scan(&schema).Error; err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("TEST_AI_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	q.Set("application_name", "ai-deep-crash")
	u.RawQuery = q.Encode()
	return u.String()
}

// B04/B05: actual SIGKILL inside the AI transaction and after its COMMIT.
// An advisory-lock trigger gives an exact, observable pre-receipt barrier.
func TestAICommitterSIGKILLWindows(t *testing.T) {
	for _, window := range []string{"transaction-before-receipt", "committed-before-report"} {
		t.Run(window, func(t *testing.T) {
			db, store := databases(t)
			in := testInput()
			seedObject(t, db, in)
			task := claim(t, store, "ai.source.prepare", in)
			result, _ := stage(t, db, store, task, in, sourceData{Parsed: ai.ParseResult{Sections: []rag.Section{{Text: "SIGKILL publication barrier", Kind: "text"}}}, Coverage: "complete"}, uuid.New(), 1)
			if window == "transaction-before-receipt" {
				if err := db.Exec(`CREATE FUNCTION receipt_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(756112017); RETURN NEW; END $$; CREATE TRIGGER receipt_barrier BEFORE INSERT ON ai_commit_receipts FOR EACH ROW EXECUTE FUNCTION receipt_barrier(); SELECT pg_advisory_lock(756112017)`).Error; err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(t.TempDir(), "payload.json")
			b, _ := json.Marshal(crashPayload{task, result})
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, executable, "-test.run=^TestAICrashProcessHelper$", "-test.v")
			child.Env = append(os.Environ(), "AI_DEEP_CRASH_FIXTURE="+path, "AI_DEEP_CRASH_DSN="+scopedAIConnection(t, db))
			var childOutput strings.Builder
			child.Stdout = &childOutput
			child.Stderr = &childOutput
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = child.Process.Kill() })
			deadline := time.Now().Add(10 * time.Second)
			reached := false
			for time.Now().Before(deadline) {
				if window == "transaction-before-receipt" {
					var n int64
					if err := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name='ai-deep-crash' AND wait_event='advisory'").Scan(&n).Error; err != nil {
						t.Fatal(err)
					}
					reached = n > 0
				} else {
					_, err := os.Stat(path + ".published")
					reached = err == nil
				}
				if reached {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !reached {
				_ = child.Process.Kill()
				_ = child.Wait()
				t.Fatalf("barrier not reached: %s", childOutput.String())
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := child.Wait(); err == nil {
				t.Fatal("child was not killed")
			}
			if window == "transaction-before-receipt" {
				if err := db.Exec("SELECT pg_advisory_unlock(756112017); DROP TRIGGER receipt_barrier ON ai_commit_receipts").Error; err != nil {
					t.Fatal(err)
				}
			}
			var receipts, sources int64
			if err := db.Table("ai_commit_receipts").Count(&receipts).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Table("ai_sources").Count(&sources).Error; err != nil {
				t.Fatal(err)
			}
			var object Object
			if err := db.First(&object, "object_id=?", in.Intent.ObjectID).Error; err != nil {
				t.Fatal(err)
			}
			if window == "transaction-before-receipt" && (receipts != 0 || sources != 0 || object.ActiveSourceID != nil) {
				t.Fatal("SIGKILL leaked partial transaction")
			}
			if window == "committed-before-report" && (receipts != 1 || sources != 1 || object.ActiveSourceID == nil) {
				t.Fatal("committed effects lost")
			}
			publisher := Publisher{DB: db, Control: store}
			for range 2 {
				publish(t, &publisher, task, result)
			}
			if err := db.Table("ai_commit_receipts").Count(&receipts).Error; err != nil {
				t.Fatal(err)
			}
			var children int64
			if err := store.DB.Table("tasks").Where("parent_id=?", task.ID).Count(&children).Error; err != nil {
				t.Fatal(err)
			}
			if receipts != 1 || children != 2 {
				t.Fatal("recovery duplicated receipt/continuations", receipts, children)
			}
		})
	}
}
