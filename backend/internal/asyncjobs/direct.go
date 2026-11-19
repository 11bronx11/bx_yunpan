package asyncjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/ownerepoch"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
	"github.com/11bronx11/bx_yunpan/backend/internal/upload"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type DirectWorker struct {
	Computer  *Computer
	Committer *Committer
}

func (w *DirectWorker) Lookup(ctx context.Context, t asynccontrol.Task) (asynccontrol.Completion, bool, error) {
	return w.Committer.Lookup(ctx, t)
}

func (w *DirectWorker) Execute(ctx context.Context, t asynccontrol.Task) (asynccontrol.Completion, *time.Time, error) {
	var out asyncprotocol.Output
	if !asynccontrol.Command(t.Kind) {
		var err error
		out, err = w.Computer.Compute(ctx, t)
		if err != nil {
			if !errors.Is(err, asynccontrol.ErrInvalid) || !w.Committer.Control.Registry[t.Kind].Converge {
				return asynccontrol.Completion{}, nil, err
			}
			out.Convergence = true
			out.Error = "upload.async_exhausted"
		}
	}
	return w.Committer.ApplyDirect(ctx, t, out)
}

func (c *Committer) Lookup(ctx context.Context, t asynccontrol.Task) (asynccontrol.Completion, bool, error) {
	var row receipt
	q := c.Business.WithContext(ctx).Where("semantic_key=?", t.SemanticKey).Find(&row)
	if q.Error != nil {
		return asynccontrol.Completion{}, false, q.Error
	}
	if q.RowsAffected == 0 {
		return asynccontrol.Completion{}, false, nil
	}
	if row.InputDigest != t.InputDigest || row.ProtocolVersion != 3 || row.TaskID == nil || *row.TaskID != t.ID || row.EffectKind != t.Kind {
		return asynccontrol.Completion{}, false, asynccontrol.ErrConflict
	}
	return coreCompletion(t, row.Outcome), true, nil
}

func coreCompletion(t asynccontrol.Task, outcome string) asynccontrol.Completion {
	return asynccontrol.Completion{TaskID: t.ID, InputDigest: t.InputDigest, ContractVersion: t.ContractVersion, Outcome: outcome, Refs: json.RawMessage(`{}`)}
}

func (c *Committer) ApplyDirect(ctx context.Context, t asynccontrol.Task, out asyncprotocol.Output) (asynccontrol.Completion, *time.Time, error) {
	started := time.Now()
	defer func() { c.Control.CommitCalls.Add(1); c.Control.CommitNanos.Add(uint64(time.Since(started))) }()
	if t.ProtocolVersion != 3 || t.Phase == "reconcile" {
		return asynccontrol.Completion{}, nil, asynccontrol.ErrInvalid
	}
	digest, _, err := asynccontrol.Digest(t.Input)
	if err != nil || digest != t.InputDigest {
		return asynccontrol.Completion{}, nil, asynccontrol.ErrConflict
	}
	var in asyncprotocol.Input
	if json.Unmarshal(t.Input, &in) != nil {
		return asynccontrol.Completion{}, nil, asynccontrol.ErrInvalid
	}
	if t.Kind == "failure.converge" {
		out.Convergence = true
		out.Error = "upload.async_exhausted"
	}
	identity := commitIdentity{ID: uuid.NewSHA1(t.ID, []byte("domain-receipt-v3:"+t.InputDigest)), InputDigest: t.InputDigest, Digest: t.InputDigest}
	bounded, cancel := context.WithTimeout(ownerepoch.Background(ctx), c.timeout())
	defer cancel()
	var decision asyncprotocol.Decision
	apply := func() error {
		return c.Business.WithContext(bounded).Transaction(func(tx *gorm.DB) error {
			if e := tx.Exec("SET LOCAL lock_timeout='2s'; SET LOCAL statement_timeout='10s'").Error; e != nil {
				return e
			}
			var e error
			decision, e = c.commitDomain(tx, t, identity, in, out)
			return e
		})
	}
	err = apply()
	if errors.Is(err, upload.ErrNameConflict) || errors.Is(err, upload.ErrNotFound) || errors.Is(err, drive.ErrConflict) || errors.Is(err, drive.ErrNotFound) || errors.Is(err, gorm.ErrDuplicatedKey) {
		out.Convergence, out.Error = true, "upload.commit_precondition_failed"
		err = apply()
	}
	if err == nil && decision.RescheduleAt == nil && c.AfterCommit != nil {
		err = c.AfterCommit()
	}
	return coreCompletion(t, decision.Outcome), decision.RescheduleAt, err
}

// AbandonedOutputs derives a bounded set of private keys from the frozen task.
// Scheduling precedes credential rotation. Late I/O is covered by the delay.
func AbandonedOutputs(safety time.Duration) func(asynccontrol.Task) ([]asynccontrol.Spec, error) {
	return func(t asynccontrol.Task) ([]asynccontrol.Spec, error) {
		var in asyncprotocol.Input
		if json.Unmarshal(t.Input, &in) != nil {
			return nil, asynccontrol.ErrInvalid
		}
		bucket, key := "", ""
		switch t.Kind {
		case "canonical.ensure":
			bucket = in.TargetBucket
			key = fmt.Sprintf("%s%s/g%d", in.TargetKey, t.ID, t.Generation)
		case "thumbnail":
			if strings.HasPrefix(in.Mime, "image/") {
				bucket = in.Bucket
				key = fmt.Sprintf("variants/compute/%s/%d/%s/g%d.jpg", in.ObjectID, in.ContentGeneration, t.ID, t.Generation)
			}
		}
		if key == "" {
			return nil, nil
		}
		if bucket == "" || (!strings.HasPrefix(key, "objects/compute/") && !strings.HasPrefix(key, "variants/compute/")) {
			return nil, asynccontrol.ErrInvalid
		}
		raw, _ := json.Marshal(asyncprotocol.Input{Bucket: bucket, Key: key})
		return []asynccontrol.Spec{{Kind: "output.delete", SemanticKey: fmt.Sprintf("output.delete:v3:%s:%d", t.ID, t.Generation), ResourceKey: t.ResourceKey, OwnerKey: t.OwnerKey, ShardKey: t.ShardKey, Input: raw, NotBefore: time.Now().Add(safety)}}, nil
	}
}

func cleanupUnadopted(tx *gorm.DB, t asynccontrol.Task, in asyncprotocol.Input, out asyncprotocol.Output) error {
	if out.Key == "" || t.Kind != "canonical.ensure" && t.Kind != "thumbnail" {
		return nil
	}
	var used int64
	if e := tx.Raw(`SELECT (SELECT count(*) FROM file_objects WHERE bucket=? AND object_key=? AND status IN ('ready','deleting'))+(SELECT count(*) FROM object_variants WHERE object_key=? AND status='ready')`, out.Bucket, out.Key, out.Key).Scan(&used).Error; e != nil {
		return e
	}
	if used > 0 {
		return nil
	}
	_, e := storageops.Enqueue(tx, storageops.Input{OperationType: storageops.StagingDelete, IdempotencyKey: "direct-loser:" + out.Bucket + ":" + out.Key, SourceBucket: out.Bucket, SourceKey: out.Key})
	return e
}
