package asyncjobs

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/media"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/ownerepoch"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dblock"
	"github.com/11bronx11/bx_yunpan/backend/internal/upload"
)

type Committer struct {
	Business      *gorm.DB
	Control       *asynccontrol.Store
	Upload        *upload.Service
	Objects       *objectstore.Service
	Media         *media.Service
	GCDelay       time.Duration
	CommitTimeout time.Duration
	Auxiliary     func(*gorm.DB, asynccontrol.Task, asyncprotocol.Input, asyncprotocol.Output) (asyncprotocol.Decision, error)
	// Test-only injection at the real crash window, after the business commit.
	AfterCommit func() error
}
type receipt struct {
	SemanticKey, InputDigest, ResultDigest string
	ResultID                               uuid.UUID
	Outcome                                string
}

func (receipt) TableName() string { return "async_commit_receipts" }

// Apply commits one domain effect and its receipt in one short transaction.
// It owns neither MQ acknowledgement nor control completion.
func (c *Committer) Apply(ctx context.Context, t asynccontrol.Task, r asynccontrol.Result) (string, *time.Time, error) {
	if err := c.Control.AcceptedResult(ctx, t, r); err != nil {
		return "", nil, err
	}
	started := time.Now()
	defer func() { c.Control.CommitCalls.Add(1); c.Control.CommitNanos.Add(uint64(time.Since(started))) }()
	var in asyncprotocol.Input
	var out asyncprotocol.Output
	if json.Unmarshal(t.Input, &in) != nil || (t.ResultID != nil && json.Unmarshal(r.Body, &out) != nil) {
		return "", nil, asynccontrol.ErrInvalid
	}
	if t.Kind == "failure.converge" {
		out.Convergence = true
		out.Error = "upload.async_exhausted"
	}
	var decision asyncprotocol.Decision
	apply := func(out asyncprotocol.Output) error {
		bounded, cancel := context.WithTimeout(ownerepoch.Background(ctx), c.timeout())
		defer cancel()
		return c.Business.WithContext(bounded).Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SET LOCAL lock_timeout='2s'; SET LOCAL statement_timeout='10s'").Error; err != nil {
				return err
			}
			var err error
			decision, err = c.commitTx(tx, t, r, in, out)
			return err
		})
	}
	err := apply(out)
	if errors.Is(err, upload.ErrNameConflict) || errors.Is(err, upload.ErrNotFound) || errors.Is(err, drive.ErrConflict) || errors.Is(err, drive.ErrNotFound) || errors.Is(err, gorm.ErrDuplicatedKey) {
		out.Convergence = true
		out.Error = "upload.commit_precondition_failed"
		err = apply(out)
	}
	if err == nil && decision.RescheduleAt == nil && c.AfterCommit != nil {
		err = c.AfterCommit()
	}
	return decision.Outcome, decision.RescheduleAt, err
}

func (c *Committer) timeout() time.Duration {
	if c.CommitTimeout > 0 {
		return c.CommitTimeout
	}
	return 10 * time.Second
}

// commitTx serializes one semantic effect and validates its immutable receipt.
func (c *Committer) commitTx(tx *gorm.DB, t asynccontrol.Task, r asynccontrol.Result, in asyncprotocol.Input, out asyncprotocol.Output) (decision asyncprotocol.Decision, err error) {
	if err := dblock.Transaction(tx, "async:commit:"+t.SemanticKey); err != nil {
		return decision, err
	}
	var existing receipt
	q := tx.Where("semantic_key=?", t.SemanticKey).Limit(1).Find(&existing)
	if q.Error != nil {
		return decision, q.Error
	}
	if q.RowsAffected > 0 {
		if existing.InputDigest != r.InputDigest || existing.ResultDigest != r.ResultDigest || existing.ResultID != r.ID {
			return decision, asynccontrol.ErrConflict
		}
		decision.Outcome = existing.Outcome
		c.Control.ReceiptHits.Add(1)
		return decision, nil
	}
	if t.Phase == "reconcile" {
		return decision, asynccontrol.ErrReceiptMissing
	}
	if in.SessionID != uuid.Nil {
		decision, err = c.Upload.CommitAsync(tx, t.Kind, in, out)
	} else {
		switch t.Kind {
		case "ai.job.finish":
			var finish aiprotocol.JobFinish
			if json.Unmarshal(t.Input, &finish) != nil || finish.JobID == uuid.Nil {
				return decision, asynccontrol.ErrInvalid
			}
			body, _ := json.Marshal(finish.Outcomes)
			versions := finish.VersionRefs
			if versions == nil {
				versions = map[string]uuid.UUID{}
			}
			refs, _ := json.Marshal(versions)
			if e := tx.Table("ai_job_requests").Where("job_id=? AND owner_id=?", finish.JobID, finish.OwnerID).Updates(map[string]any{"capability_outcomes": string(body), "result_version_refs": string(refs), "finished_at": gorm.Expr("clock_timestamp()")}).Error; e != nil {
				return decision, e
			}
			err = tx.Table("async_tasks").Where("id=? AND owner_id=?", finish.JobID, finish.OwnerID).Updates(map[string]any{"status": finish.Status, "progress": 100, "version": gorm.Expr("version+1"), "updated_at": gorm.Expr("clock_timestamp()")}).Error
			decision.Outcome = asynccontrol.Succeeded

		case "object.authorize_delete", "object.delete":
			decision, err = c.Objects.CommitAsync(tx, t.Kind, in, out, c.GCDelay)
		case "thumbnail":
			decision, err = c.Media.CommitAsync(tx, in, out)
		case "staging.delete", "variant.delete", "output.delete":
			decision.Outcome = asynccontrol.Succeeded
			if in.OperationID != uuid.Nil {
				err = tx.Table("storage_operations").Where("id=?", in.OperationID).Updates(map[string]any{"state": "succeeded", "completed_at": gorm.Expr("clock_timestamp()")}).Error
			}
		default:
			if c.Auxiliary == nil {
				return decision, asynccontrol.ErrInvalid
			}
			decision, err = c.Auxiliary(tx, t, in, out)
		}
	}
	if err != nil {
		return decision, err
	}
	if decision.RescheduleAt != nil {
		return decision, nil
	}
	return decision, tx.Create(&receipt{SemanticKey: t.SemanticKey, InputDigest: r.InputDigest, ResultDigest: r.ResultDigest, ResultID: r.ID, Outcome: decision.Outcome}).Error
}
