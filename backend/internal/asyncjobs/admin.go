package asyncjobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/upload"
)

// Redo is deliberately operator-only. The old stop/budget is retained, a
// distinct operation generation is audited, and current business prerequisites
// must still permit that work. It is never called by a recovery scan.
func (p *Planner) Redo(ctx context.Context, s *asynccontrol.Store, id uuid.UUID, reason string) (asynccontrol.Task, error) {
	if len(strings.TrimSpace(reason)) < 8 || len(reason) > 1024 {
		return asynccontrol.Task{}, errors.New("provide a specific redo reason (8..1024 bytes)")
	}
	return s.Redo(ctx, id, reason, func(old asynccontrol.Task) error {
		var in asyncprotocol.Input
		if json.Unmarshal(old.Input, &in) != nil {
			return asynccontrol.ErrInvalid
		}
		if in.SessionID != uuid.Nil {
			var session upload.Session
			if err := p.Read.WithContext(ctx).First(&session, "id=? AND user_id=?", in.SessionID, in.OwnerID).Error; err != nil {
				return err
			}
			allowed := false
			switch old.Kind {
			case "multipart.create":
				allowed = session.Status == upload.StatusInitializing
			case "multipart.complete":
				allowed = session.Status == upload.StatusCompleting
			case "upload.verify", "canonical.ensure":
				allowed = session.Status == upload.StatusVerifying
			case "upload.expire":
				allowed = session.Status == upload.StatusInitializing || session.Status == upload.StatusCreated || session.Status == upload.StatusUploading
			case "multipart.abort", "multipart.sweep", "staging.delete", "variant.delete", "output.delete":
				allowed = session.Status == upload.StatusCompleted || session.Status == upload.StatusFailed || session.Status == upload.StatusAborted || session.Status == upload.StatusExpired
			}
			if !allowed {
				return errors.New("current upload state does not permit this redo")
			}
		} else if in.ObjectID != uuid.Nil {
			var o objectstore.Object
			if err := p.Read.WithContext(ctx).First(&o, "id=?", in.ObjectID).Error; err != nil {
				return err
			}
			if o.ContentGeneration != in.ContentGeneration || o.ObjectKey != in.Key {
				return errors.New("object generation changed; rebuild through current business command")
			}
			if old.Kind == "object.delete" {
				if o.Status != "deleting" || o.GCGeneration != in.DeletionGeneration {
					return asynccontrol.ErrConflict
				}
			} else if o.Status != "ready" {
				return asynccontrol.ErrConflict
			}
		} else if old.Kind != "output.delete" {
			return errors.New("redo requires a current business resource")
		}
		return nil
	})
}
