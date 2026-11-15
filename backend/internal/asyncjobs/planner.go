// Package asyncjobs adapts domain contracts to the independent control plane.
package asyncjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
	"github.com/11bronx11/bx_yunpan/backend/internal/upload"
)

type Planner struct {
	Read         *gorm.DB
	AI, Search   bool
	GCDelay      time.Duration
	OutputSafety time.Duration
}

func pack(kind, key, owner, resource string, in asyncprotocol.Input) asynccontrol.Spec {
	b, _ := json.Marshal(in)
	if owner == "" {
		owner = resource
	}
	shard := owner
	if shard == "" {
		shard = resource
	}
	return asynccontrol.Spec{Kind: kind, SemanticKey: key, OwnerKey: owner, ResourceKey: resource, ShardKey: shard, Input: b}
}

func (p *Planner) Plan(ctx context.Context, e asynccontrol.Event) ([]asynccontrol.Spec, error) {
	var tasks []asynccontrol.Spec
	db := p.Read.WithContext(ctx)
	switch e.EventType {
	case eventcontract.EventStorageOperationRecoverV1, eventcontract.EventUploadOperationRecoverV1:
		var payload eventcontract.StorageOperationRecoverV1
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			return nil, fmt.Errorf("%w: storage payload", asynccontrol.ErrInvalid)
		}
		var op storageops.Operation
		if err := db.First(&op, "id=?", payload.OperationID).Error; err != nil {
			return nil, missingResource(err)
		}
		in := asyncprotocol.Input{OperationID: op.ID, Bucket: op.SourceBucket, Key: op.SourceKey, TargetBucket: op.TargetBucket, TargetKey: op.TargetKey, UploadID: op.StorageUploadID}
		if op.OperationType == "multipart.create" {
			in.UploadID = ""
		}
		owner, resource := "", "operation:"+op.ID.String()
		if op.UploadSessionID != nil {
			var s upload.Session
			if err := db.First(&s, "id=?", *op.UploadSessionID).Error; err != nil {
				return nil, missingResource(err)
			}
			in.SessionID = s.ID
			in.OwnerID = s.UserID
			in.SHA256 = s.DeclaredSHA256
			in.Size = s.SizeBytes
			in.Mime = s.MimeType
			owner = "owner:" + s.UserID.String()
			resource = "session:" + s.ID.String()
			if op.OperationType == storageops.MultipartComplete {
				var parts []upload.Part
				if err := db.Where("session_id=?", s.ID).Order("part_number").Find(&parts).Error; err != nil {
					return nil, missingResource(err)
				}
				for _, part := range parts {
					in.Parts = append(in.Parts, asyncprotocol.Part{Number: part.PartNumber, ETag: part.ETag})
				}
			}
		}
		if op.ObjectID != nil {
			var o objectstore.Object
			if err := db.First(&o, "id=?", *op.ObjectID).Error; err != nil {
				return nil, missingResource(err)
			}
			in.ObjectID = o.ID
			in.ContentGeneration = o.ContentGeneration
			in.DeletionGeneration = o.GCGeneration
			resource = "object:" + o.ID.String()
			if op.OperationType == storageops.ObjectDelete {
				if o.Status != "deleting" || o.ObjectKey != op.SourceKey {
					return nil, nil
				}
				if err := db.Table("object_variants").Where("object_id=?", o.ID).Pluck("object_key", &in.VariantKeys).Error; err != nil {
					return nil, missingResource(err)
				}
			}
		}
		task := pack(op.OperationType, op.OperationType+":"+op.ID.String(), owner, resource, in)
		task.NotBefore = op.AvailableAt
		if op.OperationType == "multipart.sweep" {
			safety := p.OutputSafety
			if safety == 0 {
				safety = time.Hour
			}
			task.NotBefore = op.CreatedAt.Add(safety)
		}
		tasks = append(tasks, task)
	case eventcontract.EventUploadExpiryRequestedV1:
		var in eventcontract.UploadExpiryRequestedV1
		if err := json.Unmarshal(e.Payload, &in); err != nil {
			return nil, fmt.Errorf("%w: expiry", asynccontrol.ErrInvalid)
		}
		t := pack("upload.expire", "upload.expire:"+in.UploadID.String(), "owner:"+in.OwnerID.String(), "session:"+in.UploadID.String(), asyncprotocol.Input{SessionID: in.UploadID, OwnerID: in.OwnerID})
		t.NotBefore = in.ExpiresAt
		tasks = append(tasks, t)
	case eventcontract.EventObjectVerifyRequestedV1:
		var s upload.Session
		if err := db.First(&s, "id=?", e.AggregateID).Error; err != nil {
			return nil, missingResource(err)
		}
		tasks = append(tasks, pack("upload.verify", "upload.verify:"+s.ID.String()+":v1", "owner:"+s.UserID.String(), "session:"+s.ID.String(), asyncprotocol.Input{SessionID: s.ID, OwnerID: s.UserID, Bucket: s.Bucket, Key: s.ObjectKey, SHA256: s.DeclaredSHA256, Size: s.SizeBytes, Mime: s.MimeType}))
	case eventcontract.EventObjectReadyV1, eventcontract.EventMediaThumbnailRequestedV1, eventcontract.EventObjectGCRequestedV1, eventcontract.EventAIIndexRequestedV1:
		var o objectstore.Object
		if err := db.First(&o, "id=?", e.AggregateID).Error; err != nil {
			return nil, missingResource(err)
		}
		resource := "object:" + o.ID.String()
		in := asyncprotocol.Input{ObjectID: o.ID, ContentGeneration: o.ContentGeneration, Bucket: o.Bucket, Key: o.ObjectKey, SHA256: o.SHA256, Size: o.SizeBytes, Mime: o.MimeType}
		if e.EventType == eventcontract.EventObjectGCRequestedV1 {
			in.DeletionGeneration = o.GCGeneration
			if o.Status == "ready" && o.ReferenceCount == 0 {
				t := pack("object.authorize_delete", fmt.Sprintf("object.gc:%s:%d:%d", o.ID, o.ContentGeneration, o.GCGeneration), "", resource, in)
				t.NotBefore = e.CreatedAt.Add(p.GCDelay)
				if o.GCEligibleAt != nil {
					t.NotBefore = o.GCEligibleAt.Add(p.GCDelay)
				}
				tasks = append(tasks, t)
			}
		} else if o.Status == "ready" {
			key := fmt.Sprintf("%s:%d", o.ID, o.ContentGeneration)
			if e.EventType != eventcontract.EventAIIndexRequestedV1 {
				tasks = append(tasks, pack("thumbnail", "thumbnail:"+key+":v1", "", resource, in))
			}
			if p.AI && e.EventType != eventcontract.EventMediaThumbnailRequestedV1 {
				in.OriginalPayload, _ = json.Marshal(eventcontract.AIIndexRequestedV1{ObjectID: o.ID})
				tasks = append(tasks, pack("ai.index", "ai.index:"+key+":v1", "", resource, in))
			}
		}
	case eventcontract.EventAIReprocessRequestedV1:
		if p.AI {
			var in eventcontract.AIReprocessRequestedV1
			if err := json.Unmarshal(e.Payload, &in); err != nil {
				return nil, fmt.Errorf("%w: ai reprocess", asynccontrol.ErrInvalid)
			}
			var owner struct{ OwnerID uuid.UUID }
			if err := db.Table("async_tasks").Select("owner_id").Where("id=?", in.TaskID).Scan(&owner).Error; err != nil {
				return nil, missingResource(err)
			}
			tasks = append(tasks, pack("ai.reprocess", "ai.reprocess:"+in.TaskID.String(), "owner:"+owner.OwnerID.String(), "object:"+in.ObjectID.String(), asyncprotocol.Input{ObjectID: in.ObjectID, OwnerID: owner.OwnerID, EventID: in.TaskID, OriginalPayload: e.Payload}))
		}
	default:
		if _, ok := eventcontract.Lookup(e.EventType); !ok {
			return nil, fmt.Errorf("%w: unknown event %s", asynccontrol.ErrInvalid, e.EventType)
		}
	}
	if len(outbox.SearchRoutes(e.EventType, p.Search)) > 0 {
		tasks = append(tasks, pack("search.project", "search.project:"+e.ID.String(), "", "event:"+e.AggregateID.String(), asyncprotocol.Input{EventID: e.ID, EventType: e.EventType, OriginalPayload: searchPayload(e), ObjectID: e.AggregateID}))
	}
	return tasks, nil
}

func searchPayload(e asynccontrol.Event) []byte {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(e.Payload, &fields)
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	fields["event_id"], _ = json.Marshal(e.ID)
	fields["event_type"], _ = json.Marshal(e.EventType)
	if e.AggregateVersion != nil {
		fields["aggregate_version"], _ = json.Marshal(e.AggregateVersion)
	}
	b, _ := json.Marshal(fields)
	return b
}

func missingResource(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("%w: referenced business resource no longer exists", asynccontrol.ErrInvalid)
	}
	return err
}
