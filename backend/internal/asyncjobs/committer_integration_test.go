package asyncjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/testdb"
	"github.com/11bronx11/bx_yunpan/backend/internal/upload"
)

func expiryBatch(t *testing.T) (*Committer, []asynccontrol.Notice, uuid.UUID) {
	t.Helper()
	biz := testdb.Business(t)
	s := asynccontrol.New(testdb.Control(t))
	owner := uuid.New()
	u := identity.User{ID: owner, Username: owner.String(), UsernameNormalized: owner.String(), PasswordHash: "fixture", Status: 1, QuotaBytes: 10000}
	if err := biz.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	d := drive.NewService(biz)
	if err := d.ProvisionUser(biz, owner); err != nil {
		t.Fatal(err)
	}
	root, err := d.Root(owner)
	if err != nil {
		t.Fatal(err)
	}
	q := identity.NewService(biz, nil, nil, 10000)
	objects := objectstore.NewService(biz)
	c := &Committer{Business: biz, Control: s, Upload: upload.NewService(biz, nil, nil, "test", d, objects, q, config.Upload{}), Objects: objects}
	var notices []asynccontrol.Notice
	for i := 0; i < 2; i++ {
		session := upload.Session{ID: uuid.New(), UserID: owner, FolderID: root.ID, Filename: fmt.Sprintf("expired-%d.bin", i), FilenameNormalized: fmt.Sprintf("expired-%d.bin", i), DeclaredSHA256: fmt.Sprintf("%064d", i), MimeType: "application/octet-stream", SizeBytes: 128, ReservedBytes: 128, Bucket: "test", ObjectKey: "staging/" + uuid.NewString(), Status: upload.StatusCreated, IdempotencyKey: uuid.NewString(), ExpiresAt: time.Now().Add(-time.Hour), PartSize: 128, PartCount: 1, Version: 1}
		if err := biz.Create(&session).Error; err != nil {
			t.Fatal(err)
		}
		if err := q.ReserveQuota(biz, owner, 128); err != nil {
			t.Fatal(err)
		}
		in, _ := json.Marshal(asyncprotocol.Input{SessionID: session.ID, OwnerID: owner})
		task, err := s.Ensure(context.Background(), asynccontrol.Spec{Kind: "upload.expire", SemanticKey: "upload.expire:" + session.ID.String(), OwnerKey: "owner:" + owner.String(), ResourceKey: "session:" + session.ID.String(), ShardKey: "owner:" + owner.String(), Input: in}, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		notices = append(notices, asynccontrol.Notice{TaskID: task.ID, Version: 1, Shard: task.Shard})
	}
	for i := range notices {
		var task asynccontrol.Task
		if err := s.DB.First(&task, "id=?", notices[i].TaskID).Error; err != nil {
			t.Fatal(err)
		}
		// Retained v1 command checkpoints are supported during handover.
		if err := s.DB.Transaction(func(tx *gorm.DB) error { return s.ImportState(tx, task, asynccontrol.Computed, []byte(`{}`), "") }); err != nil {
			t.Fatal(err)
		}
		claimed, ok, err := s.Claim(context.Background(), task.Notice())
		if err != nil || !ok {
			t.Fatal("command claim", err)
		}
		notices[i] = claimed.Notice()
		notices[i].ResultID = claimed.ResultID
	}
	if err := biz.Exec(`CREATE TABLE quota_writes(id bigserial);CREATE FUNCTION count_quota_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO quota_writes DEFAULT VALUES;RETURN NEW;END $$;CREATE TRIGGER count_quota_write AFTER UPDATE ON user_quota_usage FOR EACH ROW EXECUTE FUNCTION count_quota_write()`).Error; err != nil {
		t.Fatal(err)
	}
	return c, notices, owner
}

func TestIndependentCommitsSettleQuotaAndReplayReceipts(t *testing.T) {
	c, notices, owner := expiryBatch(t)
	for _, err := range commitIndependently(c, notices) {
		if err != nil {
			t.Fatal(err)
		}
	}
	var occupied, writes int64
	_ = c.Business.Table("user_quota_usage").Select("occupied_bytes").Where("owner_id=?", owner).Scan(&occupied).Error
	_ = c.Business.Table("quota_writes").Count(&writes).Error
	if occupied != 0 || writes != 2 {
		t.Fatal("independent releases missing", occupied, writes)
	}
	for _, err := range commitIndependently(c, notices) {
		if err != nil {
			t.Fatal(err)
		}
	}
	_ = c.Business.Table("quota_writes").Count(&writes).Error
	if writes != 2 {
		t.Fatal("duplicate result released quota")
	}
}

func TestLongProviderErrorConvergesAndReleasesOnce(t *testing.T) {
	c, notices, owner := expiryBatch(t)
	task, result, err := c.Control.LoadResult(context.Background(), notices[0])
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(asyncprotocol.Output{Convergence: true, Error: strings.Repeat("remote storage error: ", 32)})
	if err != nil {
		t.Fatal(err)
	}
	digest, canonical, err := asynccontrol.Digest(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Control.DB.Model(&asynccontrol.Result{}).Where("id=?", result.ID).Updates(map[string]any{"body": string(canonical), "result_digest": digest}).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := c.Commit(context.Background(), notices[0]); err != nil {
			t.Fatal("provider error prevented durable failure convergence", err)
		}
	}
	var in asyncprotocol.Input
	if err := json.Unmarshal(task.Input, &in); err != nil {
		t.Fatal(err)
	}
	var session upload.Session
	if err := c.Business.First(&session, "id=?", in.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	if session.Status != upload.StatusFailed || session.ReservedBytes != 0 || session.ErrorCode == nil || *session.ErrorCode != "upload.async_exhausted" {
		t.Fatal("failure state or stable error code", session.Status, session.ReservedBytes, session.ErrorCode)
	}
	var occupied, receipts, writes int64
	if err := c.Business.Table("user_quota_usage").Select("occupied_bytes").Where("owner_id=?", owner).Scan(&occupied).Error; err != nil {
		t.Fatal(err)
	}
	_ = c.Business.Table("async_commit_receipts").Where("semantic_key=?", task.SemanticKey).Count(&receipts).Error
	_ = c.Business.Table("quota_writes").Count(&writes).Error
	if occupied != 128 || receipts != 1 || writes != 1 {
		t.Fatal("failure settlement repeated", occupied, receipts, writes)
	}
}

func TestTransactionRollbackKeepsFailedCheckpointRecoverable(t *testing.T) {
	c, notices, owner := expiryBatch(t)
	var task asynccontrol.Task
	if err := c.Control.DB.First(&task, "id=?", notices[0].TaskID).Error; err != nil {
		t.Fatal(err)
	}
	sql := fmt.Sprintf(`CREATE FUNCTION reject_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.semantic_key='%s' THEN RAISE EXCEPTION 'injected receipt failure';END IF;RETURN NEW;END $$;CREATE TRIGGER reject_receipt BEFORE INSERT ON async_commit_receipts FOR EACH ROW EXECUTE FUNCTION reject_receipt()`, task.SemanticKey)
	if err := c.Business.Exec(sql).Error; err != nil {
		t.Fatal(err)
	}
	errors := commitIndependently(c, notices)
	if errors[0] == nil || errors[1] != nil {
		t.Fatal("batch failure poisoned sibling", errors)
	}
	var occupied int64
	_ = c.Business.Table("user_quota_usage").Select("occupied_bytes").Where("owner_id=?", owner).Scan(&occupied).Error
	if occupied != 128 {
		t.Fatal("rollback lost reservation", occupied)
	}
	var failed asynccontrol.Task
	_ = c.Control.DB.First(&failed, "id=?", task.ID).Error
	if failed.State != asynccontrol.Running {
		t.Fatal("failed item reported committed")
	}
	if err := c.Business.Exec("DROP TRIGGER reject_receipt ON async_commit_receipts").Error; err != nil {
		t.Fatal(err)
	}
	if err := c.Commit(context.Background(), notices[0]); err != nil {
		t.Fatal(err)
	}
	_ = c.Business.Table("user_quota_usage").Select("occupied_bytes").Where("owner_id=?", owner).Scan(&occupied).Error
	if occupied != 0 {
		t.Fatal("recovery failed", occupied)
	}
}

func TestReceiptConflictIsolatedWithoutPoisoningSibling(t *testing.T) {
	c, notices, _ := expiryBatch(t)
	task, result, err := c.Control.LoadResult(context.Background(), notices[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Business.Create(&receipt{SemanticKey: task.SemanticKey, InputDigest: result.InputDigest, ResultDigest: "conflicting-result", ResultID: uuid.New(), Outcome: asynccontrol.Succeeded}).Error; err != nil {
		t.Fatal(err)
	}
	for _, err := range commitIndependently(c, notices) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Control.DB.First(&task, "id=?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if task.State != asynccontrol.Stopped || task.Outcome != asynccontrol.Quarantined {
		t.Fatal("permanent receipt conflict not isolated", task.State)
	}
	var sibling asynccontrol.Task
	if err := c.Control.DB.First(&sibling, "id=?", notices[1].TaskID).Error; err != nil {
		t.Fatal(err)
	}
	if sibling.State != asynccontrol.Done || sibling.Outcome != asynccontrol.Succeeded {
		t.Fatal("conflict poisoned sibling")
	}
}

func TestExpiryScanRetainsComputedAndStoppedWork(t *testing.T) {
	c, notices, _ := expiryBatch(t)
	ctx := context.Background()
	token, ok, err := c.Control.AcquireModule(ctx, "expiry-scan", "fixture")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if err := c.Control.ScanExpiry(ctx, c.Business, token, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.Control.ScanExpiry(ctx, c.Business, token, 1); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := c.Control.DB.Table("tasks").Count(&count).Error; err != nil || count != 2 {
		t.Fatal("computed work duplicated", count, err)
	}
	if err := c.Commit(ctx, notices[0]); err != nil {
		t.Fatal(err)
	}
	task, _, err := c.Control.LoadResult(ctx, notices[0])
	if err != nil {
		t.Fatal(err)
	}
	var in asyncprotocol.Input
	if err := json.Unmarshal(task.Input, &in); err != nil {
		t.Fatal(err)
	}
	// A contradictory business row needs review; the stopped work is never reopened.
	if err := c.Business.Table("upload_sessions").Where("id=?", in.SessionID).Update("status", upload.StatusCreated).Error; err != nil {
		t.Fatal(err)
	}
	if err := c.Control.ScanExpiry(ctx, c.Business, token, 100); err != nil {
		t.Fatal(err)
	} // wraps cursor
	if err := c.Control.ScanExpiry(ctx, c.Business, token, 100); err != nil {
		t.Fatal(err)
	}
	if err := c.Control.DB.First(&task, "id=?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if task.State != asynccontrol.Done || task.Outcome != asynccontrol.Succeeded {
		t.Fatal("terminal expiry task reopened")
	}
	if err := c.Control.DB.Table("quarantines").Where("identity LIKE 'expiry-contradiction:%'").Count(&count).Error; err != nil || count != 1 {
		t.Fatal("contradiction not recorded", count, err)
	}
	if err := c.Control.ScanExpiry(ctx, c.Business, uuid.New(), 100); !errors.Is(err, asynccontrol.ErrLost) {
		t.Fatal("stale scanner accepted", err)
	}
}

func commitIndependently(c *Committer, notices []asynccontrol.Notice) []error {
	errs := make([]error, len(notices))
	for i, n := range notices {
		errs[i] = c.Commit(context.Background(), n)
	}
	return errs
}
