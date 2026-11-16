package aisystem

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/testdb"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestAIMigrationCrashHelper(t *testing.T) {
	path := os.Getenv("AI_DEEP_MIGRATION_FIXTURE")
	if path == "" {
		return
	}
	var f struct {
		Run  uuid.UUID
		Mode string
	}
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &f) != nil {
		t.Fatal("invalid migration child fixture")
	}
	open := func(key string) *gorm.DB {
		db, err := gorm.Open(postgres.Open(os.Getenv(key)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatal("migration child DB unavailable")
		}
		pool, _ := db.DB()
		deferClose := func() { _ = pool.Close() }
		t.Cleanup(deferClose)
		return db
	}
	biz := open("AI_DEEP_BUSINESS_DSN")
	m := Migrator{Read: biz, IntentWriter: biz, AI: open("AI_DEEP_AI_DSN"), Control: open("AI_DEEP_CONTROL_DSN"), Profile: testInput().Profile, Revision: "205-fixture", Batch: 100}
	var r MigrationReport
	switch f.Mode {
	case "plan":
		r, err = m.Plan(context.Background(), f.Run)
	case "handoff":
		r, err = m.Handoff(context.Background(), f.Run)
	case "enqueue":
		r, err = m.Enqueue(context.Background(), f.Run)
	default:
		t.Fatal("invalid child phase")
	}
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(r)
	if err := os.WriteFile(path+".committed", raw, 0600); err != nil {
		t.Fatal(err)
	}
	<-time.After(25 * time.Second)
	t.Fatal("parent did not kill migration child")
}

func migrationCrashBatch(t *testing.T, m *Migrator, run uuid.UUID, mode string) MigrationReport {
	t.Helper()
	path := filepath.Join(t.TempDir(), "migration.json")
	raw, _ := json.Marshal(struct {
		Run  uuid.UUID
		Mode string
	}{run, mode})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := m.Read.Raw("SELECT current_schema()").Scan(&schema).Error; err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	executable, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable, "-test.run=^TestAIMigrationCrashHelper$")
	child.Env = append(os.Environ(), "AI_DEEP_MIGRATION_FIXTURE="+path, "AI_DEEP_BUSINESS_DSN="+u.String(), "AI_DEEP_AI_DSN="+scopedAIConnection(t, m.AI), "AI_DEEP_CONTROL_DSN="+scopedControlConnection(t, asynccontrol.New(m.Control)))
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill() })
	var r MigrationReport
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		raw, err = os.ReadFile(path + ".committed")
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatal("migration child did not reach committed batch")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("migration child not killed")
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// G02/G03/G04: 205 retained objects cross two 100-row boundaries. Reconstruct
// the tool between calls and conserve mutually exclusive per-object outcomes.
func TestMigration205ObjectsResumeAndConserveOutcomes(t *testing.T) {
	biz := testdb.Business(t)
	db, store := databases(t)
	ctx := context.Background()
	profile := testInput().Profile
	run := uuid.New()
	newTool := func() *Migrator {
		return &Migrator{Read: biz, AI: db, Control: store.DB, IntentWriter: biz, Profile: profile, Revision: "205-fixture", Batch: 100}
	}
	t.Setenv("AI_LEGACY_WRITERS_STOPPED", "")
	if _, err := newTool().Plan(ctx, run); err == nil {
		t.Fatal("missing stopped flag accepted")
	}
	t.Setenv("AI_LEGACY_WRITERS_STOPPED", "true")
	for n := range 205 {
		o := objectstore.Object{ID: uuid.New(), SHA256: aiprotocol.Hash([]byte(fmt.Sprint(n))), SizeBytes: 4, MimeType: "text/plain", Bucket: "fixture", ObjectKey: fmt.Sprintf("old/%d", n), Status: "ready", ReferenceCount: 1, Version: 1}
		switch n % 5 {
		case 1:
			o.MimeType = "application/octet-stream"
		case 2:
			o.ReferenceCount = 0
		case 4:
			o.Status = "deleted"
		}
		if err := biz.Create(&o).Error; err != nil {
			t.Fatal(err)
		}
		if n%5 == 3 {
			input, _ := json.Marshal(map[string]any{"object_id": o.ID, "content_generation": 1})
			task, err := store.Ensure(ctx, asynccontrol.Spec{Kind: "ai.index", SemanticKey: "legacy:" + o.ID.String(), ResourceKey: "object:" + o.ID.String(), ShardKey: "object:" + o.ID.String(), Input: input, Attempts: 3, Dispatches: 4, Recoveries: 2, Deadline: time.Now().Add(time.Hour)}, "205-fixture")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.DB.Model(&asynccontrol.Task{}).Where("id=?", task.ID).Update("state", asynccontrol.NeedsAttention).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	first := migrationCrashBatch(t, newTool(), run, "plan")
	checkpoints := []int64{first.Items}
	for {
		r, err := newTool().Plan(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		checkpoints = append(checkpoints, r.Items)
		if !r.HasMore {
			break
		}
	}
	if fmt.Sprint(checkpoints) != "[100 200 205]" {
		t.Fatal("bad plan boundaries", checkpoints)
	}
	migrationCrashBatch(t, newTool(), run, "handoff")
	for {
		r, err := newTool().Handoff(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		if !r.HasMore {
			break
		}
	}
	migrationCrashBatch(t, newTool(), run, "enqueue")
	for {
		r, err := newTool().Enqueue(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		if !r.HasMore {
			break
		}
	}
	if _, err := newTool().Enqueue(ctx, run); err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Conclusion string
		Count      int64
	}
	if err := db.Table("ai_migration_items").Select("conclusion,count(*)").Where("run_id=?", run).Group("conclusion").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	var total int64
	for _, row := range rows {
		got[row.Conclusion] = row.Count
		total += row.Count
	}
	for _, conclusion := range []string{"queued", "skip_unsupported", "skip_unreferenced", "stopped_preserved", "skip_deleted"} {
		if got[conclusion] != 41 {
			t.Fatalf("outcome not conserved: %s=%d all=%v", conclusion, got[conclusion], got)
		}
	}
	if total != 205 {
		t.Fatal("object manifest lost/duplicated", total)
	}
	var events int64
	if err := biz.Table("outbox_events").Where("event_type=?", aiprotocol.IntentEvent).Count(&events).Error; err != nil {
		t.Fatal(err)
	}
	if events != 41 {
		t.Fatal("duplicate/reopened intent", events)
	}
	if _, err := newTool().CutoverCheck(ctx, run); err == nil {
		t.Fatal("unfinished/untransferred migration archived")
	}
}
