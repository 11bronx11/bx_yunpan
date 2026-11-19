package asyncjobs

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/upload"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestDirectCoreCrashProcessHelper(t *testing.T) {
	path := os.Getenv("CORE_DIRECT_CRASH_FIXTURE")
	if path == "" {
		return
	}
	var task asynccontrol.Task
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &task) != nil {
		t.Fatal("invalid direct core fixture")
	}
	db, err := gorm.Open(postgres.Open(os.Getenv("CORE_DIRECT_CRASH_DSN")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("child business database unavailable")
	}
	pool, _ := db.DB()
	defer pool.Close()
	objects := objectstore.NewService(db)
	c := &Committer{Business: db, Control: &asynccontrol.Store{}, Objects: objects}
	c.Upload = upload.NewService(db, nil, nil, "test", drive.NewService(db), objects, identity.NewService(db, nil, nil, 10000), config.Upload{})
	if _, _, err := c.ApplyDirect(context.Background(), task, asyncprotocol.Output{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".published", []byte("committed"), 0o600); err != nil {
		t.Fatal(err)
	}
	<-time.After(20 * time.Second)
	t.Fatal("parent did not kill direct core process")
}

func TestDirectCoreProcessSIGKILLPreservesQuotaAndReceipt(t *testing.T) {
	for _, window := range []string{"before-receipt", "committed-before-confirmation"} {
		t.Run(window, func(t *testing.T) {
			c, notices, owner := expiryBatch(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var old asynccontrol.Task
			if err := c.Control.DB.First(&old, "id=?", notices[0].TaskID).Error; err != nil {
				t.Fatal(err)
			}
			var controlSchema, businessSchema string
			if err := c.Control.DB.Raw("SELECT current_schema()").Scan(&controlSchema).Error; err != nil {
				t.Fatal(err)
			}
			if err := c.Business.Raw("SELECT current_schema()").Scan(&businessSchema).Error; err != nil {
				t.Fatal(err)
			}
			controlURL, _ := url.Parse(os.Getenv("TEST_CONTROL_POSTGRES_DSN"))
			q := controlURL.Query()
			q.Set("search_path", controlSchema+",public")
			q.Set("options", "-c bx.coordination_protocol=v3")
			controlURL.RawQuery = q.Encode()
			control, err := gorm.Open(postgres.Open(controlURL.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			if err != nil {
				t.Fatal("direct test control connection unavailable")
			}
			cp, _ := control.DB()
			defer cp.Close()
			if err := control.Exec("UPDATE control_settings SET value='v3' WHERE key='coordination_protocol'").Error; err != nil {
				t.Fatal(err)
			}
			c.Control = asynccontrol.New(control)
			c.Control.ProtocolVersion = 3
			task, err := c.Control.Ensure(ctx, asynccontrol.Spec{Kind: old.Kind, SemanticKey: "direct-crash:" + old.SemanticKey, OwnerKey: old.OwnerKey, ResourceKey: old.ResourceKey, ShardKey: old.ShardKey, Input: json.RawMessage(old.Input)}, "crash-fixture")
			if err != nil {
				t.Fatal(err)
			}
			var claimed bool
			task, claimed, err = c.Control.Claim(ctx, task.Notice())
			if err != nil || !claimed {
				t.Fatal(claimed, err)
			}
			pool, _ := c.Business.DB()
			held, err := pool.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			if window == "before-receipt" {
				if err := c.Business.Exec(`CREATE FUNCTION direct_core_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(692310020); RETURN NEW; END $$; CREATE TRIGGER direct_core_barrier BEFORE INSERT ON async_commit_receipts FOR EACH ROW EXECUTE FUNCTION direct_core_barrier()`).Error; err != nil {
					t.Fatal(err)
				}
				if _, err := held.ExecContext(ctx, "SELECT pg_advisory_lock(692310020)"); err != nil {
					t.Fatal(err)
				}
				defer held.ExecContext(context.Background(), "SELECT pg_advisory_unlock(692310020)")
			}
			path := filepath.Join(t.TempDir(), "core.json")
			raw, _ := json.Marshal(task)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			businessURL, _ := url.Parse(os.Getenv("TEST_POSTGRES_DSN"))
			app := "core-direct-crash-" + task.ID.String()
			q = businessURL.Query()
			q.Set("search_path", businessSchema+",public")
			q.Set("application_name", app)
			businessURL.RawQuery = q.Encode()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			child := exec.CommandContext(ctx, executable, "-test.run=^TestDirectCoreCrashProcessHelper$")
			child.Env = append(os.Environ(), "CORE_DIRECT_CRASH_FIXTURE="+path, "CORE_DIRECT_CRASH_DSN="+businessURL.String())
			var output strings.Builder
			child.Stdout, child.Stderr = &output, &output
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = child.Process.Kill() })
			reached := false
			for until := time.Now().Add(5 * time.Second); time.Now().Before(until) && !reached; {
				if window == "before-receipt" {
					var n int64
					if err := c.Business.Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name=? AND wait_event='advisory'", app).Scan(&n).Error; err != nil {
						t.Fatal(err)
					}
					reached = n == 1
				} else {
					_, err := os.Stat(path + ".published")
					reached = err == nil
				}
				if !reached {
					asynccontrol.Pause(ctx, 10*time.Millisecond)
				}
			}
			_ = child.Process.Kill()
			killErr := child.Wait()
			if !reached || killErr == nil {
				t.Fatalf("core SIGKILL barrier not reached: %s", output.String())
			}
			if window == "before-receipt" {
				if _, err := held.ExecContext(ctx, "SELECT pg_advisory_unlock(692310020)"); err != nil {
					t.Fatal(err)
				}
				if err := c.Business.Exec("DROP TRIGGER direct_core_barrier ON async_commit_receipts").Error; err != nil {
					t.Fatal(err)
				}
			}
			var writes, receipts, occupied int64
			if err := c.Business.Table("quota_writes").Count(&writes).Error; err != nil {
				t.Fatal(err)
			}
			if err := c.Business.Table("async_commit_receipts").Where("task_id=?", task.ID).Count(&receipts).Error; err != nil {
				t.Fatal(err)
			}
			if err := c.Business.Table("user_quota_usage").Select("occupied_bytes").Where("owner_id=?", owner).Scan(&occupied).Error; err != nil {
				t.Fatal(err)
			}
			want := int64(1)
			if window == "before-receipt" {
				want = 0
			}
			if writes != want || receipts != want || occupied != 256-128*want {
				t.Fatal("quota and receipt did not commit atomically", writes, receipts, occupied)
			}
			for range 2 {
				task.Generation++
				completion, _, err := c.ApplyDirect(ctx, task, asyncprotocol.Output{})
				if err != nil || completion.Outcome != asynccontrol.Succeeded {
					t.Fatal(completion, err)
				}
			}
			if err := c.Business.Table("quota_writes").Count(&writes).Error; err != nil || writes != 1 {
				t.Fatal("crash recovery released quota more than once", writes, err)
			}
			t.Log("real SIGKILL: quota/receipt rollback or durable commit, followed by duplicate replay, verified")
		})
	}
}
