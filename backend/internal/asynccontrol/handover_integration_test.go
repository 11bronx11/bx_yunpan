package asynccontrol

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/testdb"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"
)

func TestLegacyDrainPersistsProofBeforeAckAndResumes(t *testing.T) {
	s := New(testdb.Control(t))
	b := testBroker(t)
	ctx := context.Background()
	task, err := s.Ensure(ctx, spec("legacy-drain"), "source")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := amqp.Dial(b.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	queue := b.WorkQueue("upload")
	n := task.Notice()
	n.Version, n.ShardKey, n.Shard, n.LayoutVersion = 1, task.ShardKey, task.Shard, task.LayoutVersion
	body, _ := json.Marshal(n)
	if err := ch.PublishWithContext(ctx, "", queue, false, false, amqp.Publishing{Body: body, DeliveryMode: amqp.Persistent}); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Exec("ALTER TABLE coordination_messages RENAME TO paused_messages").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DrainLegacy(ctx, b.URL, b.Prefix); err == nil {
		t.Fatal("ACKed without durable proof")
	}
	if err := s.DB.Exec("ALTER TABLE paused_messages RENAME TO coordination_messages").Error; err != nil {
		t.Fatal(err)
	}
	// A failed drain closes its channel, returning the unacknowledged message.
	deadline := time.Now().Add(3 * time.Second)
	for {
		info, err := ch.QueueInspect(queue)
		if err != nil {
			t.Fatal(err)
		}
		if info.Messages == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unproven message lost")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for range 2 {
		if err := s.DrainLegacy(ctx, b.URL, b.Prefix); err != nil {
			t.Fatal(err)
		}
	}
	var count int64
	if err := s.DB.Table("coordination_messages").Where("disposition='transferred'").Count(&count).Error; err != nil || count != 1 {
		t.Fatal("proof missing", count, err)
	}
	info, err := ch.QueueInspect(queue)
	if err != nil || info.Messages != 0 {
		t.Fatal("drain incomplete", info, err)
	}
}

func TestDatabaseFenceRejectsOldWriters(t *testing.T) {
	s := New(testdb.Control(t))
	task, err := s.Ensure(context.Background(), spec("writer-fence"), "source")
	if err != nil {
		t.Fatal(err)
	}
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL bx.coordination_protocol='v1'").Error; err != nil {
			return err
		}
		return tx.Model(&Task{}).Where("id=?", task.ID).Update("last_error", "old writer").Error
	})
	if err == nil || !strings.Contains(err.Error(), "v2 writer") {
		t.Fatal("old writer admitted", err)
	}
	err = s.DB.Model(&Task{}).Where("id=?", task.ID).Update("protocol_version", 1).Error
	if err == nil || !strings.Contains(err.Error(), "v1 coordination is retired") {
		t.Fatal("old protocol reopened", err)
	}
}

func TestUnrecognizedLegacyMessageRetainsOriginalBytes(t *testing.T) {
	s := New(testdb.Control(t))
	raw := []byte{0, 255, '{', 'x'}
	if err := s.TransferMessage(context.Background(), "old.work", raw); err != nil {
		t.Fatal(err)
	}
	var row struct {
		Notice      json.RawMessage
		Disposition string
	}
	if err := s.DB.Table("coordination_messages").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Raw   []byte `json:"raw_base64"`
		Queue string `json:"queue"`
	}
	if err := json.Unmarshal(row.Notice, &saved); err != nil || string(saved.Raw) != string(raw) || saved.Queue != "old.work" || row.Disposition != "quarantined" {
		t.Fatal("original message evidence lost", saved, err)
	}
}

func TestEarlyNoticeRetainsDueDeliveryAndImmutableCheckpoint(t *testing.T) {
	s := New(testdb.Control(t))
	ctx := context.Background()
	sp := spec("early-notice")
	sp.NotBefore = time.Now().Add(time.Hour)
	task, err := s.Ensure(ctx, sp, "source")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Exec("UPDATE task_deliveries SET state='confirmed' WHERE task_id=?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Claim(ctx, task.Notice()); err != nil || ok {
		t.Fatal(ok, err)
	}
	var state string
	if err := s.DB.Table("task_deliveries").Select("state").Where("task_id=?", task.ID).Scan(&state).Error; err != nil || state != "pending" {
		t.Fatal("future delivery lost", state, err)
	}
	if err := s.DB.Exec("UPDATE tasks SET not_before=clock_timestamp() WHERE id=?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := s.Claim(ctx, task.Notice())
	if err != nil || !ok {
		t.Fatal(err)
	}
	r, err := s.SaveResult(ctx, claimed, []byte(`{"upload_id":"accepted"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptedResult(ctx, claimed, r); err != nil {
		t.Fatal(err)
	}
	r.Body = []byte(`{"upload_id":"forged"}`)
	if err := s.AcceptedResult(ctx, claimed, r); err != ErrConflict {
		t.Fatal("mutated checkpoint accepted", err)
	}
}

func TestHandoverRetainsCheckpointStopsAndConsumedBudgets(t *testing.T) {
	s := New(testdb.Control(t))
	ctx := context.Background()
	first, err := s.Ensure(ctx, spec("handover-computed"), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	first = dispatch(t, s, first)
	result, err := s.SaveResult(ctx, first, []byte(`{"upload_id":"accepted"}`))
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := s.Ensure(ctx, spec("handover-stopped"), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Exec("UPDATE control_settings SET value='v1' WHERE key='coordination_protocol'").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Exec("UPDATE tasks SET protocol_version=1,state='computed',token=NULL,lease_until=NULL,attempts=5,dispatches=7,result_sends=3 WHERE id=?", first.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Exec("UPDATE tasks SET protocol_version=1,state='needs_attention',attempts=12,last_error='operator stop' WHERE id=?", stopped.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.CheckProtocol(ctx); err == nil {
		t.Fatal("new workers admitted early")
	}
	for range 3 {
		if _, err := s.HandoverBatch(ctx, 1); err != nil {
			t.Fatal(err)
		}
	}
	var current Task
	if err := s.DB.First(&current, "id=?", first.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.State != Pending || current.Phase != "commit" || current.ResultID == nil || *current.ResultID != result.ID || current.Attempts != 5 || current.Dispatches != 7 || current.Generation != first.Generation {
		t.Fatal("checkpoint or budget changed", current)
	}
	var old Task
	s.DB.First(&old, "id=?", stopped.ID)
	if old.State != Stopped || old.Outcome != NeedsAttention || old.Attempts != 12 || old.LastError != "operator stop" {
		t.Fatal("manual stop reopened", old)
	}
	var count int64
	s.DB.Table("coordination_handover").Count(&count)
	if count != 2 {
		t.Fatal("snapshot missing or duplicated", count)
	}
	n := Notice{Version: 1, TaskID: first.ID, SemanticKey: first.SemanticKey, InputDigest: first.InputDigest, ShardKey: first.ShardKey, Shard: first.Shard, LayoutVersion: first.LayoutVersion, ResultID: &result.ID, EffectTarget: EffectCore}
	body, _ := json.Marshal(n)
	for range 2 {
		if err := s.TransferMessage(ctx, "v1.result", body); err != nil {
			t.Fatal(err)
		}
	}
	s.DB.Table("coordination_messages").Where("disposition='transferred'").Count(&count)
	if count != 1 {
		t.Fatal("message proof not idempotent", count)
	}
	if err := s.Activate(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, ok, err := s.Claim(ctx, current.Notice())
	if err != nil || !ok {
		t.Fatal("checkpoint claim", err)
	}
	if recovered.Attempts != 5 || recovered.Generation != first.Generation {
		t.Fatal("handover recomputed")
	}
}

func TestHandoverRefusesLiveLegacyExecution(t *testing.T) {
	s := New(testdb.Control(t))
	ctx := context.Background()
	task, err := s.Ensure(ctx, spec("live-old"), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	task = dispatch(t, s, task)
	if err := s.DB.Exec("UPDATE control_settings SET value='v1' WHERE key='coordination_protocol'").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Exec("UPDATE tasks SET protocol_version=1 WHERE id=?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandoverBatch(ctx, 10); err == nil {
		t.Fatal("live writer transferred")
	}
}

func TestHandoverDoesNotRenewExhaustedRecoveryBudget(t *testing.T) {
	s := New(testdb.Control(t))
	ctx := context.Background()
	task, err := s.Ensure(ctx, spec("old-recovery-exhausted"), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Exec("UPDATE control_settings SET value='v1' WHERE key='coordination_protocol'").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Exec("UPDATE tasks SET protocol_version=1,state='retry_wait',recoveries=max_recoveries WHERE id=?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.HandoverBatch(ctx, 10); err != nil {
			t.Fatal(err)
		}
	}
	var current Task
	if err := s.DB.First(&current, "id=?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	var children int64
	if err := s.DB.Model(&Task{}).Where("parent_id=? AND kind='failure.converge'", task.ID).Count(&children).Error; err != nil {
		t.Fatal(err)
	}
	if current.State != Stopped || current.Attempts != 0 || current.Recoveries != current.MaxRecoveries || children != 1 {
		t.Fatal("old budget refreshed or convergence lost", current, children)
	}
}

func TestPublishConfirmLossRetainsDeliveryIdentity(t *testing.T) {
	s := New(testdb.Control(t))
	ctx := context.Background()
	task, err := s.Ensure(ctx, spec("confirm-loss"), "source")
	if err != nil {
		t.Fatal(err)
	}
	first, ok, err := s.Reserve(ctx, "upload")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if err := s.DB.Exec("UPDATE task_deliveries SET lease_until=clock_timestamp()-interval '1 second' WHERE task_id=?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	second, ok, err := s.Reserve(ctx, "upload")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if first.DispatchGeneration != second.DispatchGeneration || first.ID != second.ID {
		t.Fatal("confirm loss created new execution identity")
	}
	if err := s.Published(ctx, first, nil); err != nil {
		t.Fatal(err)
	}
	var state string
	s.DB.Table("task_deliveries").Select("state").Where("task_id=?", task.ID).Scan(&state)
	if state != "publishing" {
		t.Fatal("stale publish token won")
	}
	if err := s.Published(ctx, second, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Reserve(ctx, "upload"); err != nil || ok {
		t.Fatal("confirmed message republished", err)
	}
}

func TestLastCommitCrashGetsReceiptOnlyReconciliation(t *testing.T) {
	s := New(testdb.Control(t))
	ctx := context.Background()
	task, err := s.Ensure(ctx, spec("last-commit"), "source")
	if err != nil {
		t.Fatal(err)
	}
	task = dispatch(t, s, task)
	r, err := s.SaveResult(ctx, task, []byte(`{"upload_id":"stable"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BeginCommit(ctx, &task, r); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Exec("UPDATE tasks SET max_commit_attempts=commit_attempts,lease_until=clock_timestamp()-interval '1 second' WHERE id=?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(ctx, 10); err != nil {
		t.Fatal(err)
	}
	var current Task
	s.DB.First(&current, "id=?", task.ID)
	if current.Phase != "reconcile" || current.State != Pending {
		t.Fatal("last crash lost receipt reconciliation", current)
	}
	replay, ok, err := s.Claim(ctx, current.Notice())
	if err != nil || !ok {
		t.Fatal(err)
	}
	if replay.Generation != task.Generation || replay.Attempts != 1 {
		t.Fatal("reconcile recomputed")
	}
	if err := s.Report(ctx, replay, r, Succeeded); err != nil {
		t.Fatal("durable success rejected at budget limit", err)
	}
	if err := s.Recover(ctx, 10); err != nil {
		t.Fatal(err)
	}
	s.DB.First(&current, "id=?", task.ID)
	if current.State != Done || current.Outcome != Succeeded {
		t.Fatal("success rewritten")
	}
}
