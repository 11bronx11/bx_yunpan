package asynccontrol

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"gorm.io/gorm"
)

func directStore(t *testing.T) *Store {
	t.Helper()
	s, _ := testStore(t)
	s.ProtocolVersion = 3
	if e := s.DB.Exec("SET bx.coordination_protocol='v3'; UPDATE control_settings SET value='v3' WHERE key='coordination_protocol'").Error; e != nil {
		t.Fatal(e)
	}
	return s
}

func TestDirectHandoverPreservesBudgetsAndV2MessageEvidence(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	sp := spec("v2-to-direct")
	sp.Attempts, sp.Dispatches = 4, 5
	old, e := s.Ensure(ctx, sp, "history")
	if e != nil {
		t.Fatal(e)
	}
	message, e := json.Marshal(old.Notice())
	if e != nil {
		t.Fatal(e)
	}
	s.ProtocolVersion = 3
	if e = s.DB.Exec("SET bx.coordination_protocol='v3'").Error; e != nil {
		t.Fatal(e)
	}
	if n, e := s.HandoverDirectBatch(ctx, 100); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if n, e := s.HandoverDirectBatch(ctx, 100); e != nil || n != 0 {
		t.Fatal("handover replay", n, e)
	}
	if e = s.TransferMessage(ctx, "v2.work.upload", message); e != nil {
		t.Fatal(e)
	}
	var disposition string
	if e = s.DB.Table("coordination_messages").Select("disposition").Scan(&disposition).Error; e != nil || disposition != "transferred" {
		t.Fatal("v2 message not durably transferred", disposition, e)
	}
	if e = s.ActivateDirect(ctx); e != nil {
		t.Fatal(e)
	}
	var current Task
	if e = s.DB.First(&current, "id=?", old.ID).Error; e != nil || current.Attempts != old.Attempts || current.Dispatches != old.Dispatches || !current.Deadline.Equal(old.Deadline.Round(time.Microsecond)) {
		t.Fatal("handover refreshed budget or deadline", e)
	}
	if e = s.DB.Exec("SET bx.coordination_protocol='v2'").Error; e != nil {
		t.Fatal(e)
	}
	if e = s.DB.Model(&Task{}).Where("id=?", old.ID).Update("attempts", 0).Error; e == nil {
		t.Fatal("old writer crossed protocol fence")
	}
}

func completion(t Task) Completion {
	return Completion{TaskID: t.ID, InputDigest: t.InputDigest, ContractVersion: t.ContractVersion, Outcome: Succeeded}
}

func TestDirectPublicationRacesAndNoLegacyTables(t *testing.T) {
	s := directStore(t)
	ctx := context.Background()
	for _, name := range []string{"results", "executions", "task_continuations", "task_deliveries"} {
		if e := s.DB.Exec("ALTER TABLE " + name + " RENAME TO legacy_" + name).Error; e != nil {
			t.Fatal(e)
		}
	}
	task, e := s.Ensure(ctx, spec("direct-races"), "event:test")
	if e != nil {
		t.Fatal(e)
	}
	reserved, ok, e := s.Reserve(ctx, task.Queue)
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	current, ok, e := s.Claim(ctx, reserved.Notice())
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	if e = s.Complete(ctx, current, completion(current)); e != nil {
		t.Fatal(e)
	}
	if e = s.Published(ctx, reserved, nil); e != nil {
		t.Fatal(e)
	}
	var got Task
	if e = s.DB.First(&got, "id=?", task.ID).Error; e != nil {
		t.Fatal(e)
	}
	if got.State != Done || got.DispatchState != "consumed" || got.PublishToken != nil {
		t.Fatalf("late confirm regressed task: %+v", got)
	}
	if _, ok, e = s.Claim(ctx, reserved.Notice()); e != nil || ok {
		t.Fatal("duplicate claim", ok, e)
	}
	if e = s.Maintain(ctx, 10, time.Hour); e != nil {
		t.Fatal(e)
	}
}

func TestDirectEarlyNoticeAndPublisherFencing(t *testing.T) {
	s := directStore(t)
	ctx := context.Background()
	sp := spec("early-direct")
	sp.NotBefore = time.Now().Add(time.Hour)
	task, e := s.Ensure(ctx, sp, "test")
	if e != nil {
		t.Fatal(e)
	}
	if _, ok, e := s.Claim(ctx, task.Notice()); e != nil || ok {
		t.Fatal(ok, e)
	}
	var row Task
	s.DB.First(&row, "id=?", task.ID)
	if delta := row.DispatchNotBefore.Sub(task.NotBefore); row.DispatchState != "pending" || delta < -time.Microsecond || delta > time.Microsecond {
		t.Fatal("future notice lost")
	}
	if _, ok, e := s.Reserve(ctx, task.Queue); e != nil || ok {
		t.Fatal("early publication", ok, e)
	}
	s.DB.Model(&Task{}).Where("id=?", task.ID).Updates(map[string]any{"not_before": gorm.Expr("clock_timestamp()-interval '1 second'"), "dispatch_not_before": gorm.Expr("clock_timestamp()-interval '1 second'")})
	first, ok, e := s.Reserve(ctx, task.Queue)
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	s.DB.Model(&Task{}).Where("id=?", task.ID).Update("publish_lease_until", gorm.Expr("clock_timestamp()-interval '1 second'"))
	second, ok, e := s.Reserve(ctx, task.Queue)
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	if e = s.Published(ctx, first, nil); e != nil {
		t.Fatal(e)
	}
	s.DB.First(&row, "id=?", task.ID)
	if row.DispatchState != "publishing" || row.PublishToken == nil || *row.PublishToken != *second.DeliveryToken {
		t.Fatal("stale publisher changed state")
	}
	if e = s.Published(ctx, second, nil); e != nil {
		t.Fatal(e)
	}
	if _, ok, e = s.Reserve(ctx, task.Queue); e != nil || ok {
		t.Fatal("confirmed work republished", ok, e)
	}
}

func TestDirectContractKeepsTenTablesAndRunsNewTasks(t *testing.T) {
	s := directStore(t)
	ctx := context.Background()
	contract, e := os.ReadFile("../../control-contract-migrations/000001_archive_v2.up.sql")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.DB.Exec(string(contract)).Error; e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.DB.Exec("DROP SCHEMA async_v2_archive CASCADE") })
	var count int64
	if e = s.DB.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_type='BASE TABLE'").Scan(&count).Error; e != nil || count != 10 {
		t.Fatal("contracted table count", count, e)
	}
	task, e := s.Ensure(ctx, spec("after-contract"), "contract-test")
	if e != nil {
		t.Fatal(e)
	}
	task = dispatch(t, s, task)
	if e = s.Complete(ctx, task, completion(task)); e != nil {
		t.Fatal(e)
	}
	if e = s.Maintain(ctx, 10, time.Hour); e != nil {
		t.Fatal(e)
	}
	if e = s.Recover(ctx, 10); e != nil {
		t.Fatal(e)
	}
	if n, e := s.HandoverDirectBatch(ctx, 100); e != nil || n != 0 {
		t.Fatal("contracted restart handover", n, e)
	}
	if e = s.ActivateDirect(ctx); e != nil {
		t.Fatal("contracted restart activation", e)
	}
}

func TestDirectFinalReceiptCheckIsReadOnlyAndClaimedOnce(t *testing.T) {
	s := directStore(t)
	ctx := context.Background()
	sp := spec("last-crash")
	sp.MaxAttempts = 1
	task, e := s.Ensure(ctx, sp, "test")
	if e != nil {
		t.Fatal(e)
	}
	task = dispatch(t, s, task)
	s.DB.Model(&Task{}).Where("id=?", task.ID).Update("lease_until", gorm.Expr("clock_timestamp()-interval '1 second'"))
	if e = s.Recover(ctx, 10); e != nil {
		t.Fatal(e)
	}
	var current Task
	s.DB.First(&current, "id=?", task.ID)
	current = dispatch(t, s, current)
	if current.Phase != "reconcile" || !current.ReceiptCheckClaimed || current.Attempts != 1 {
		t.Fatalf("incorrect receipt check: %+v", current)
	}
	handler := &directTestHandler{}
	if e = s.executeDirect(ctx, current, handler); !errors.Is(e, ErrReceiptMissing) {
		t.Fatal(e)
	}
	if handler.executes != 0 {
		t.Fatal("final check recomputed")
	}
	s.DB.Model(&Task{}).Where("id=?", task.ID).Update("lease_until", gorm.Expr("clock_timestamp()-interval '1 second'"))
	if e = s.Recover(ctx, 10); e != nil {
		t.Fatal(e)
	}
	s.DB.First(&current, "id=?", task.ID)
	if current.State != Stopped || current.Outcome != NeedsAttention {
		t.Fatal("final crash was retried", current.State)
	}
}

type directTestHandler struct{ executes int }

func (h *directTestHandler) Lookup(context.Context, Task) (Completion, bool, error) {
	return Completion{}, false, nil
}

func (h *directTestHandler) Execute(context.Context, Task) (Completion, *time.Time, error) {
	h.executes++
	return Completion{}, nil, nil
}

func TestDirectCompletionSuccessorsAreAtomic(t *testing.T) {
	s := directStore(t)
	ctx := context.Background()
	task, e := s.Ensure(ctx, spec("atomic-parent"), "test")
	if e != nil {
		t.Fatal(e)
	}
	task = dispatch(t, s, task)
	s.PlanCompleted = func(parent Task, c Completion) ([]Spec, error) {
		return []Spec{spec("child-good"), {Kind: "unknown"}}, nil
	}
	if e = s.Complete(ctx, task, completion(task)); e == nil {
		t.Fatal("invalid successor accepted")
	}
	var count int64
	s.DB.Model(&Task{}).Where("semantic_key=?", "child-good").Count(&count)
	if count != 0 {
		t.Fatal("partial successor escaped rollback")
	}
	var current Task
	s.DB.First(&current, "id=?", task.ID)
	if current.State != Running {
		t.Fatal("parent committed without children")
	}
	s.PlanCompleted = func(parent Task, c Completion) ([]Spec, error) { return []Spec{spec("child-good")}, nil }
	if e = s.Complete(ctx, task, completion(task)); e != nil {
		t.Fatal(e)
	}
	if e = s.Complete(ctx, task, completion(task)); e != nil {
		t.Fatal(e)
	}
	s.DB.Model(&Task{}).Where("semantic_key=?", "child-good").Count(&count)
	if count != 1 {
		t.Fatal("duplicate successor", count)
	}
}
