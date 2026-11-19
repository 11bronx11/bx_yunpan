package asyncjobs

import (
	"context"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
	"github.com/google/uuid"
)

func TestDirectCoreReceiptReplaySettlesQuotaOnce(t *testing.T) {
	c, notices, owner := expiryBatch(t)
	ctx := context.Background()
	var old asynccontrol.Task
	if e := c.Control.DB.First(&old, "id=?", notices[0].TaskID).Error; e != nil {
		t.Fatal(e)
	}
	task := old
	task.ProtocolVersion = 3
	task.ID = uuid.New()
	task.SemanticKey = "direct:" + old.SemanticKey
	task.ResultID = nil
	task.Phase = "execute"
	task.Deadline = time.Now().Add(time.Hour)
	first, due, e := c.ApplyDirect(ctx, task, asyncprotocol.Output{})
	if e != nil || due != nil || first.Outcome != asynccontrol.Succeeded {
		t.Fatal(first, due, e)
	}
	task.Generation++
	again, _, e := c.ApplyDirect(ctx, task, asyncprotocol.Output{Error: "losing retry"})
	if e != nil || again.Outcome != first.Outcome {
		t.Fatal(again, e)
	}
	_, found, e := c.Lookup(ctx, task)
	if e != nil || !found {
		t.Fatal(found, e)
	}
	var writes, occupied int64
	c.Business.Table("quota_writes").Count(&writes)
	c.Business.Table("user_quota_usage").Select("occupied_bytes").Where("owner_id=?", owner).Scan(&occupied)
	if writes != 1 || occupied != 128 {
		t.Fatal("duplicate domain effect", writes, occupied)
	}
	task.InputDigest = "conflict"
	if _, _, e = c.Lookup(ctx, task); e == nil {
		t.Fatal("input conflict admitted")
	}
}
