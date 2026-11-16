package asyncjobs

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/google/uuid"
)

// Redo re-enters the AI intent step so AI-local pointers are resolved afresh.
// It does not copy a stale processing grant, reopen a user job, or reset any
// original budget. A newer business intent requires an explicit rebuild.
func (p *Planner) redoAI(ctx context.Context, old asynccontrol.Task) (asynccontrol.Spec, error) {
	var in aiprotocol.Input
	if json.Unmarshal(old.Input, &in) != nil || in.Intent.Validate() != nil || p.AIIntent == nil {
		return asynccontrol.Spec{}, asynccontrol.ErrInvalid
	}
	target := map[string]string{
		"ai.source.prepare": "source", "ai.text.build": "text",
		"ai.embedding.generate": "vectors", "ai.summary.generate": "summary",
		"ai.project.text": "projection_text", "ai.project.vector": "projection_vector",
	}[old.Kind]
	if target == "" && old.Kind != "ai.intent.apply" {
		return asynccontrol.Spec{}, errors.New("AI maintenance and terminal snapshots require their dedicated command")
	}
	i := in.Intent
	var object objectstore.Object
	if err := p.Read.WithContext(ctx).First(&object, "id=?", i.ObjectID).Error; err != nil {
		return asynccontrol.Spec{}, err
	}
	if object.Status != "ready" || object.ReferenceCount < 1 || object.ContentGeneration != i.ContentGeneration || object.SHA256 != i.ContentHash || object.SizeBytes != i.Size {
		return asynccontrol.Spec{}, errors.New("AI resource is no longer current; rebuild through the current file command")
	}
	var current struct {
		SourceSeq, TextSeq, VectorSeq, SummarySeq int64
		ProfileID, ProfileRevision                string
	}
	if err := p.Read.WithContext(ctx).Table("ai_resource_intents").Where("object_id=? AND content_generation=?", i.ObjectID, i.ContentGeneration).Take(&current).Error; err != nil {
		return asynccontrol.Spec{}, err
	}
	if current.ProfileID != i.ProfileID || current.ProfileRevision != i.ProfileRevision || current.SourceSeq != i.Seqs.Source || current.TextSeq != i.Seqs.Text || current.VectorSeq != i.Seqs.Vector || current.SummarySeq != i.Seqs.Summary {
		return asynccontrol.Spec{}, errors.New("a newer AI intent exists; rebuild through the current file command")
	}
	if target != "" {
		i.Targets = []string{target}
	}
	i.RequestID = uuid.NewSHA1(old.ID, []byte("ai-manual-redo-v1"))
	i.RequestedAt = time.Now().Unix()
	i.JobID, i.Observers, i.Legacy = uuid.Nil, nil, nil
	planned, err := p.AIIntent(i)
	if err != nil {
		return planned, err
	}
	var renewed aiprotocol.Input
	if json.Unmarshal(planned.Input, &renewed) != nil || renewed.Profile.Digest() != in.Profile.Digest() {
		return asynccontrol.Spec{}, errors.New("AI profile changed; rebuild through the current file command")
	}
	return planned, nil
}
