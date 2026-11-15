package asynccontrol

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func testStore(t *testing.T) (*Store, *gorm.DB) {
	t.Helper()
	dsn := os.Getenv("TEST_CONTROL_POSTGRES_DSN")
	bizDSN := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" || bizDSN == "" {
		t.Skip("dedicated business and control databases required")
	}
	open := func(dsn string) *gorm.DB {
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatal(err)
		}
		sql, _ := db.DB()
		sql.SetMaxOpenConns(6)
		t.Cleanup(func() { _ = sql.Close() })
		return db
	}
	db, biz := open(dsn), open(bizDSN)
	// Fixtures use an isolated schema in each dedicated test database.
	schema := "ctrl_" + uuid.NewString()[:8]
	for _, d := range []*gorm.DB{db, biz} {
		if err := d.Exec(`CREATE SCHEMA "` + schema + `"`).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []*gorm.DB{db, biz} {
		sql, _ := d.DB()
		sql.SetMaxOpenConns(1)
		if err := d.Exec(`SET search_path TO "` + schema + `"`).Error; err != nil {
			t.Fatal(err)
		}
	}
	paths, err := filepath.Glob("../../control-migrations/*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err = db.Exec(string(b)).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := biz.Exec(`CREATE TABLE outbox_events(id uuid PRIMARY KEY,aggregate_id uuid NOT NULL,event_type text NOT NULL,payload jsonb NOT NULL,trace_context jsonb,aggregate_version bigint,event_xid xid8 NOT NULL DEFAULT pg_current_xact_id(),seq bigserial,created_at timestamptz NOT NULL DEFAULT clock_timestamp()); CREATE INDEX ON outbox_events(event_xid,seq)`).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, d := range []*gorm.DB{db, biz} {
			_ = d.Exec(`DROP SCHEMA "` + schema + `" CASCADE`).Error
		}
	})
	s := New(db)
	s.RetryBase = time.Millisecond
	return s, biz
}

func spec(key string) Spec {
	return Spec{Kind: "multipart.create", SemanticKey: key, OwnerKey: "owner:a", ResourceKey: "session:a", ShardKey: "owner:a", Input: []byte(`{"session":"a"}`)}
}

func dispatch(t *testing.T, s *Store, task Task) Task {
	t.Helper()
	r, ok, err := s.Reserve(context.Background(), task.Queue)
	if err != nil || !ok {
		t.Fatal("reserve", ok, err)
	}
	c, ok, err := s.Claim(context.Background(), Notice{TaskID: r.ID, DispatchGeneration: r.DispatchGeneration, Version: 1})
	if err != nil || !ok {
		t.Fatal("claim", ok, err)
	}
	return c
}

func TestLeaseResultsAndDurableBudget(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	task, err := s.Ensure(ctx, spec("work"), "one")
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := s.Ensure(ctx, spec("work"), "two")
	if err != nil || duplicate.ID != task.ID {
		t.Fatal("identity", err)
	}
	first := dispatch(t, s, task)
	if _, ok, err := s.Claim(ctx, Notice{TaskID: first.ID, DispatchGeneration: first.DispatchGeneration, Version: 1}); err != nil || ok {
		t.Fatal("duplicate claimed", err)
	}
	if err := s.DB.Exec("UPDATE tasks SET lease_until=clock_timestamp()-interval '1 second' WHERE id=?", first.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(ctx, 10); err != nil {
		t.Fatal(err)
	}
	_ = s.DB.Exec("UPDATE tasks SET not_before=clock_timestamp() WHERE id=?", first.ID).Error
	second := dispatch(t, s, task)
	if second.Attempts != 2 || second.Generation <= first.Generation {
		t.Fatal("budget reset")
	}
	if err := s.Renew(ctx, first); err != ErrLost {
		t.Fatal("old renew", err)
	}
	if _, err := s.SaveResult(ctx, first, []byte(`{"upload_id":"old"}`)); err != ErrLost {
		t.Fatal("old result", err)
	}
	r, err := s.SaveResult(ctx, second, []byte(`{"upload_id":"valid"}`))
	if err != nil {
		t.Fatal(err)
	}
	n := Notice{TaskID: task.ID, ResultID: &r.ID, Version: 1, Shard: task.Shard}
	loaded, got, err := s.LoadResult(ctx, n)
	if err != nil || got.ResultDigest != r.ResultDigest {
		t.Fatal(err)
	}
	if loaded.State != Computed {
		t.Fatal("premature success")
	}
	if _, ok, err := s.NextResult(ctx); err != nil || !ok {
		t.Fatal("durable publication", err)
	}
	if err := s.Report(ctx, loaded, got, Succeeded); err != nil {
		t.Fatal(err)
	}
	if err := s.Report(ctx, loaded, got, Succeeded); err != nil {
		t.Fatal("duplicate report", err)
	}
	if err := s.Fail(ctx, second, fmt.Errorf("late error"), true); err != ErrLost {
		t.Fatal("success overwritten", err)
	}
}

func TestNoticeIdentityAndGenerationFenceExecutionAndResults(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	if _, err := s.Ensure(ctx, spec("wire-identity"), "event"); err != nil {
		t.Fatal(err)
	}
	reserved, ok, err := s.Reserve(ctx, "upload")
	if err != nil || !ok {
		t.Fatal("reserve", ok, err)
	}
	mutations := map[string]func(*Notice){
		"semantic operation": func(n *Notice) { n.SemanticKey += ":other" },
		"input":              func(n *Notice) { n.InputDigest = "different" },
		"commit domain":      func(n *Notice) { n.ShardKey = "owner:other" },
		"layout":             func(n *Notice) { n.LayoutVersion++ },
		"shard":              func(n *Notice) { n.Shard = (n.Shard + 1) % s.Shards },
		"execution":          func(n *Notice) { n.Generation++ },
	}
	for name, mutate := range mutations {
		n := reserved.Notice()
		mutate(&n)
		if _, ok, err := s.Claim(ctx, n); err != nil || ok {
			t.Fatalf("%s claimed another identity: %v %v", name, ok, err)
		}
	}
	claimed, ok, err := s.Claim(ctx, reserved.Notice())
	if err != nil || !ok || claimed.Attempts != 1 || claimed.Generation != reserved.Generation+1 {
		t.Fatal("valid notice did not preserve the single execution budget", ok, err)
	}
	if _, err := s.SaveResult(ctx, claimed, []byte(`{"upload_id":"current"}`)); err != nil {
		t.Fatal(err)
	}
	computed, ok, err := s.NextResult(ctx)
	if err != nil || !ok {
		t.Fatal("persistent result", ok, err)
	}
	for name, mutate := range mutations {
		n := computed.Notice()
		mutate(&n)
		if _, _, err := s.LoadResult(ctx, n); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s result notice accepted: %v", name, err)
		}
	}
	if _, r, err := s.LoadResult(ctx, computed.Notice()); err != nil || r.Generation != claimed.Generation {
		t.Fatal("valid result identity or generation lost", err)
	}
}

func TestFinalCrashedAttemptConverges(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	sp := spec("final")
	sp.Attempts = 11
	task, err := s.Ensure(ctx, sp, "import")
	if err != nil {
		t.Fatal(err)
	}
	task = dispatch(t, s, task)
	_ = s.DB.Exec("UPDATE tasks SET lease_until=clock_timestamp()-interval '1 second' WHERE id=?", task.ID).Error
	if err := s.Recover(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.First(&task, "id=?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if task.State != Converging || task.Attempts != 12 || task.ResultID == nil {
		t.Fatalf("lost convergence: %+v", task)
	}
	for i := 0; i < 3; i++ {
		if err := s.Recover(ctx, 10); err != nil {
			t.Fatal(err)
		}
	}
	var count int64
	_ = s.DB.Model(&Result{}).Where("task_id=?", task.ID).Count(&count).Error
	if count != 1 {
		t.Fatal("recreated convergence")
	}
}

func TestRelaySafeWatermarkAndAtomicTakeover(t *testing.T) {
	s, biz := testStore(t)
	ctx := context.Background()
	sqlBiz, _ := biz.DB()
	sqlBiz.SetMaxOpenConns(3)
	// Pin independent sessions to the fixture schema before beginning transactions.
	var schema string
	if err := biz.Raw("SELECT current_schema()").Scan(&schema).Error; err != nil {
		t.Fatal(err)
	}
	connA, err := sqlBiz.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connA.Close()
	_, err = connA.ExecContext(ctx, `SET search_path TO "`+schema+`"`)
	if err != nil {
		t.Fatal(err)
	}
	a, err := connA.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Rollback()
	insert := `INSERT INTO outbox_events(id,aggregate_id,event_type,payload) VALUES($1,$2,'test','{}')`
	aID, bID, cID := uuid.New(), uuid.New(), uuid.New()
	aggregate := uuid.New()
	if _, err = a.ExecContext(ctx, insert, aID, aggregate); err != nil {
		t.Fatal(err)
	}
	connB, err := sqlBiz.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connB.Close()
	_, err = connB.ExecContext(ctx, `SET search_path TO "`+schema+`"`)
	if err != nil {
		t.Fatal(err)
	}
	b, err := connB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.ExecContext(ctx, insert, bID, aggregate); err != nil {
		t.Fatal(err)
	}
	if err = b.Commit(); err != nil {
		t.Fatal(err)
	}
	// Use this pinned connection for relay reads: one statement includes its xmin.
	read := biz.Session(&gorm.Session{NewDB: true})
	read.Statement.ConnPool = connB
	relay := Relay{Control: s, Business: read, Source: "primary", Rows: 200, Bytes: 1 << 20, Plan: func(_ context.Context, e Event) ([]Spec, error) { return []Spec{spec("event:" + e.ID.String())}, nil }}
	token, ok, err := s.AcquireModule(ctx, "relay", "one")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if n, err := relay.Step(ctx, token); err != nil || n != 0 {
		t.Fatal("advanced beyond live xmin", n, err)
	}
	// Subtransaction insert uses the top-level xid; rolled back events disappear.
	if _, err = a.ExecContext(ctx, "SAVEPOINT child"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ExecContext(ctx, insert, cID, aggregate); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ExecContext(ctx, "SAVEPOINT discarded"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ExecContext(ctx, insert, uuid.New(), aggregate); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ExecContext(ctx, "ROLLBACK TO discarded"); err != nil {
		t.Fatal(err)
	}
	if err = a.Commit(); err != nil {
		t.Fatal(err)
	}
	// Inject control-side failure after inbox writes. Position and tasks must roll back.
	if err := s.DB.Exec(`CREATE FUNCTION reject_position() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected'; END $$; CREATE TRIGGER reject_position BEFORE UPDATE ON relay_positions FOR EACH ROW EXECUTE FUNCTION reject_position()`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := relay.Step(ctx, token); err == nil {
		t.Fatal("injection failed")
	}
	var count int64
	_ = s.DB.Table("event_inbox").Count(&count).Error
	if count != 0 {
		t.Fatal("partial takeover")
	}
	_ = s.DB.Exec("DROP TRIGGER reject_position ON relay_positions").Error
	if n, err := relay.Step(ctx, token); err != nil || n != 3 {
		t.Fatal("events missed", n, err)
	}
	if n, err := relay.Step(ctx, token); err != nil || n != 0 {
		t.Fatal("duplicate read", n, err)
	}
	_ = s.DB.Model(&Task{}).Count(&count).Error
	if count != 3 {
		t.Fatal("duplicate tasks", count)
	}
	// An obsolete relay token cannot advance a restarted position.
	_ = s.DB.Exec("UPDATE module_leases SET lease_until=clock_timestamp()-interval '1 second' WHERE name='relay'").Error
	if _, ok, err := s.AcquireModule(ctx, "relay", "two"); err != nil || !ok {
		t.Fatal("takeover", err)
	}
	_ = s.DB.Exec("UPDATE relay_positions SET last_xid='0',last_seq=0").Error
	if _, err := relay.Step(ctx, token); err != ErrLost {
		t.Fatal("stale relay advanced", err)
	}
}

func TestConflictIsolationFairnessAndGlobalBudgets(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	original, err := s.Ensure(ctx, spec("conflict"), "one")
	if err != nil {
		t.Fatal(err)
	}
	conflicting := spec("conflict")
	conflicting.Input = []byte(`{"changed":true}`)
	if _, err := s.Ensure(ctx, conflicting, "two"); err != ErrConflict {
		t.Fatal("conflict accepted", err)
	}
	var isolated int64
	if err := s.DB.Table("quarantines").Where("task_id=?", original.ID).Count(&isolated).Error; err != nil || isolated != 1 {
		t.Fatal("isolation not durable", err, isolated)
	}
	if err := s.DB.Exec("UPDATE queue_budgets SET owner_limit=1,inflight_limit=2 WHERE queue='upload'").Error; err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a1", "a2", "b1"} {
		sp := spec(key)
		sp.OwnerKey = "owner:a"
		if key == "b1" {
			sp.OwnerKey = "owner:b"
		}
		if _, err := s.Ensure(ctx, sp, key); err != nil {
			t.Fatal(err)
		}
	}
	// Exclude the identity-conflict example from this scheduling scenario.
	_ = s.DB.Model(&Task{}).Where("id=?", original.ID).Update("state", Quarantined).Error
	first, ok, err := s.Reserve(ctx, "upload")
	if err != nil || !ok {
		t.Fatal(err)
	}
	second, ok, err := s.Reserve(ctx, "upload")
	if err != nil || !ok || first.OwnerKey == second.OwnerKey {
		t.Fatal("owner starved", first.OwnerKey, second.OwnerKey, err)
	}
	if _, ok, err := s.Reserve(ctx, "upload"); err != nil || ok {
		t.Fatal("global inflight budget exceeded", err)
	}
	_ = s.DB.Exec("UPDATE write_budget SET concurrent_limit=1,available_tokens=128").Error
	permit, ok, err := s.WritePermit(ctx, 0)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, ok, err := s.WritePermit(ctx, 1); err != nil || ok {
		t.Fatal("second instance multiplied write budget", err)
	}
	_ = s.DB.Exec("DELETE FROM write_permits WHERE token=?", permit).Error
	if _, ok, err := s.WritePermit(ctx, 1); err != nil || !ok {
		t.Fatal("permit not reusable", err)
	}
}

func TestPriorityMinimumShareAndFencedScheduler(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	_ = s.DB.Exec("UPDATE queue_budgets SET owner_limit=100,inflight_limit=100 WHERE queue='upload'").Error
	var low Task
	for i := 0; i < 8; i++ {
		sp := spec(fmt.Sprintf("priority-%d", i))
		sp.OwnerKey = "one"
		task, err := s.Ensure(ctx, sp, "priority")
		if err != nil {
			t.Fatal(err)
		}
		if i == 7 {
			low = task
			_ = s.DB.Model(&Task{}).Where("id=?", task.ID).Update("priority", 2).Error
		}
	}
	for i := 0; i < 5; i++ {
		task, ok, err := s.Reserve(ctx, "upload")
		if err != nil || !ok {
			t.Fatal(err)
		}
		if i == 4 && task.ID != low.ID {
			t.Fatal("low priority work starved")
		}
	}
	token, ok, err := s.AcquireModule(ctx, "scheduler", "old")
	if err != nil || !ok {
		t.Fatal(err)
	}
	_ = s.DB.Exec("UPDATE module_leases SET lease_until=clock_timestamp()-interval '1 second' WHERE name='scheduler'").Error
	if _, ok, err := s.AcquireModule(ctx, "scheduler", "new"); err != nil || !ok {
		t.Fatal(err)
	}
	if _, _, err := s.Reserve(ctx, "upload", token); err != ErrLost {
		t.Fatal("stale scheduler advanced", err)
	}
	var active int64
	_ = s.DB.Model(&Task{}).Where("state='dispatched'").Count(&active).Error
	if active != 5 {
		t.Fatal("fencing did not roll back reservation", active)
	}
}

func TestStoppedRedoAndResultRetention(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	task, err := s.Ensure(ctx, spec("redo"), "original")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.DB.Model(&Task{}).Where("id=?", task.ID).Updates(map[string]any{"state": NeedsAttention, "attempts": 12}).Error
	redo, err := s.Redo(ctx, task.ID, "storage dependency repaired", func(Task) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := s.Redo(ctx, task.ID, "repeat same operator command", func(Task) error { return fmt.Errorf("must not revalidate existing child") })
	if err != nil || duplicate.ID != redo.ID {
		t.Fatal("redo duplicated budget", err)
	}
	var old Task
	_ = s.DB.First(&old, "id=?", task.ID).Error
	if old.Attempts != 12 || old.State != Quarantined {
		t.Fatal("old stop erased")
	}
	claimed := dispatch(t, s, redo)
	result, err := s.SaveResult(ctx, claimed, []byte(`{"upload_id":"retained"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.DB.Exec("UPDATE results SET created_at=clock_timestamp()-interval '30 days' WHERE id=?", result.ID).Error
	if err := s.Maintain(ctx, 100, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var got Result
	_ = s.DB.First(&got, "id=?", result.ID).Error
	if string(got.Body) != `{"upload_id": "retained"}` {
		t.Fatalf("uncommitted result removed: %s", got.Body)
	}
	if err := s.DB.Exec("UPDATE tasks SET updated_at=clock_timestamp()-interval '30 days' WHERE id=?", redo.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Maintain(ctx, 100, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var pending Task
	if err := s.DB.First(&pending, "id=?", redo.ID).Error; err != nil {
		t.Fatal(err)
	}
	if string(pending.Input) == `{"archived": true}` {
		t.Fatal("uncommitted task input archived")
	}
	if err := s.Report(ctx, claimed, result, Succeeded); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Exec("UPDATE tasks SET updated_at=clock_timestamp()-interval '30 days' WHERE id=?", redo.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Exec("UPDATE results SET committed_at=clock_timestamp()-interval '30 days' WHERE id=?", result.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Maintain(ctx, 100, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.First(&pending, "id=?", redo.ID).Error; err != nil {
		t.Fatal(err)
	}
	if string(pending.Input) != `{"archived": true}` {
		t.Fatal("terminal task input not archived")
	}
	duplicate, err = s.Ensure(ctx, Spec{Kind: pending.Kind, SemanticKey: pending.SemanticKey, ShardKey: pending.ShardKey, OwnerKey: pending.OwnerKey, ResourceKey: pending.ResourceKey, Input: spec("redo").Input}, "replay")
	if err != nil || duplicate.State != Succeeded || duplicate.Attempts != pending.Attempts {
		t.Fatal("archive erased semantic barrier", err)
	}
}
