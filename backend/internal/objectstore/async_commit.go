package objectstore

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
)

func (s *Service) CommitAsync(tx *gorm.DB, kind string, in asyncprotocol.Input, out asyncprotocol.Output, gcDelay time.Duration) (asyncprotocol.Decision, error) {
	d := asyncprotocol.Decision{Outcome: "succeeded"}
	var o Object
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&o, "id=?", in.ObjectID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		d.Outcome = "superseded"
		return d, nil
	}
	if err != nil {
		return d, err
	}
	if o.ContentGeneration != in.ContentGeneration || o.ObjectKey != in.Key || o.Bucket != in.Bucket {
		d.Outcome = "superseded"
		return d, nil
	}
	switch kind {
	case "object.authorize_delete":
		if o.Status != "ready" || o.ReferenceCount != 0 || o.GCGeneration != in.DeletionGeneration {
			d.Outcome = "superseded"
			return d, nil
		}
		var now time.Time
		if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return d, err
		}
		if o.GCEligibleAt != nil && o.GCEligibleAt.Add(gcDelay).After(now) {
			due := o.GCEligibleAt.Add(gcDelay)
			d.RescheduleAt = &due
			return d, nil
		}
		var references int64
		if err := tx.Table("file_entries").Where("object_id=? AND deleted_at IS NULL", o.ID).Count(&references).Error; err != nil {
			return d, err
		}
		if references != 0 {
			return d, fmt.Errorf("object reference invariant violated: %s", o.ID)
		}
		if err := tx.Model(&o).Updates(map[string]any{"status": "deleting", "version": gorm.Expr("version+1"), "updated_at": now}).Error; err != nil {
			return d, err
		}
		if err := outbox.TombstoneAI(tx.Statement.Context, tx, o.ID, o.ContentGeneration, o.Version+1); err != nil {
			return d, err
		}
		_, err := storageops.Enqueue(tx, storageops.Input{OperationType: storageops.ObjectDelete, IdempotencyKey: fmt.Sprintf("object.delete:%s:%d:%d", o.ID, o.ContentGeneration, o.GCGeneration), ObjectID: &o.ID, SourceBucket: o.Bucket, SourceKey: o.ObjectKey})
		return d, err
	case storageops.ObjectDelete:
		if o.Status == "deleted" {
			return d, nil
		}
		if o.Status != "deleting" || o.GCGeneration != in.DeletionGeneration || o.ReferenceCount != 0 {
			return d, fmt.Errorf("delete result lacks active authorization")
		}
		if err := tx.Exec("DELETE FROM object_variants WHERE object_id=?", o.ID).Error; err != nil {
			return d, err
		}
		if err := tx.Model(&o).Updates(map[string]any{"status": "deleted", "deleted_at": gorm.Expr("clock_timestamp()"), "version": gorm.Expr("version+1"), "updated_at": gorm.Expr("clock_timestamp()")}).Error; err != nil {
			return d, err
		}
		if in.OperationID != uuid.Nil {
			if err := tx.Model(&storageops.Operation{}).Where("id=?", in.OperationID).Updates(map[string]any{"state": "succeeded", "completed_at": gorm.Expr("clock_timestamp()"), "updated_at": gorm.Expr("clock_timestamp()")}).Error; err != nil {
				return d, err
			}
		}
		err := outbox.AddTyped(tx.Statement.Context, tx, eventcontract.EnvelopeInput{EventType: eventcontract.EventObjectGCRequestedV1, AggregateID: o.ID, AggregateVersion: o.Version + 1, Producer: "committer-gc", Payload: eventcontract.ObjectGCRequestedV1{ObjectID: o.ID}})
		return d, err
	default:
		return d, fmt.Errorf("unknown object commit %s", kind)
	}
}
