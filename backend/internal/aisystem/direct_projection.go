package aisystem

import (
	"context"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func publishTypedProjection(tx *gorm.DB, in aiprotocol.Input, value any) error {
	if in.Operation == "delete" || in.Operation == "retire" {
		d, ok := value.(cleanupData)
		if !ok {
			return asynccontrol.ErrInvalid
		}
		return publishCleanupData(tx, in, d)
	}
	d, ok := value.(projectionData)
	if !ok {
		return asynccontrol.ErrInvalid
	}
	kind := "ai.project.text"
	if d.Backend == "qd" {
		kind = "ai.project.vector"
	}
	return publishProjectionData(tx, in, d, kind)
}

// ProjectDirect records recoverable physical ownership before the first remote
// write. Failed/unknown writes retain the record through the full writer window.
func (p *Projector) ProjectDirect(ctx context.Context, t asynccontrol.Task, in aiprotocol.Input) (any, int64, error) {
	if in.Operation != "delete" && in.Operation != "retire" {
		backend := "es"
		version := in.ContentVersionID
		if t.Kind == "ai.project.vector" {
			backend = "qd"
			version = in.EmbeddingSetID
		}
		id, name, _, err := projectionIdentity(backend, in)
		if err != nil {
			return nil, 0, err
		}
		if version == uuid.Nil {
			return nil, 0, asynccontrol.ErrInvalid
		}
		// A deadline plus the unchanged eight-day retirement grace also covers
		// processing grants and bounded HTTP calls which return after cancellation.
		err = p.DB.WithContext(ctx).Exec(`INSERT INTO ai_orphan_projections(backend,target_id,target_version,object_id,content_generation,artifact_version_id,task_id,input_digest,writer_deadline)
 VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(backend,target_id,artifact_version_id) DO UPDATE SET writer_deadline=GREATEST(ai_orphan_projections.writer_deadline,EXCLUDED.writer_deadline)`, backend, id, name, in.Intent.ObjectID, in.Intent.ContentGeneration, version, t.ID, t.InputDigest, t.Deadline.Add(time.Hour)).Error
		if err != nil {
			return nil, 0, err
		}
	}
	return p.Project(ctx, t.Kind, in)
}
