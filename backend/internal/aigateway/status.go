package aigateway

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pbv2"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/core"
	corepb "github.com/11bronx11/bx_yunpan/backend/internal/core/pb"
	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/google/uuid"
)

// indexStatus preserves the simple public status without making core metadata
// depend on the AI database. Disabled optional stages do not block completion.
func indexStatus(caps *pbv2.Capabilities) string {
	if caps == nil || !caps.GetText().GetSupported() {
		return "missing"
	}
	complete, started := true, false
	for _, cap := range []*pbv2.Capability{caps.Source, caps.Text, caps.Vector, caps.Summary} {
		if !cap.GetSupported() {
			continue
		}
		complete = complete && cap.GetAvailable()
		build := cap.GetLatestBuild()
		switch build.GetState() {
		case "failed", "exhausted", "quarantined", "needs_attention", "canceled":
			return "failed"
		}
		started = started || cap.GetAvailable() || build.GetObservedAt() != ""
		if build.GetObservedAt() != "" && (build.GetState() == "pending" || build.GetState() == "running" || build.GetState() == "processing") {
			complete = false
		}
	}
	if complete {
		return "indexed"
	}
	if started {
		return "processing"
	}
	return "missing"
}

// FileStatuses adds live ai_status values to one authorized directory page.
// Keep this outside metadata caches, and bound the optional AI read separately.
func (h *HTTP) FileStatuses(ctx context.Context, owner uuid.UUID, files []drive.FileView) {
	if len(files) == 0 {
		return
	}
	if !h.Enabled || h.Client == nil {
		for n := range files {
			value := "missing"
			files[n].AIStatus = &value
		}
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	grant, err := h.grant(ctx, owner, aiprotocol.Scope{Mode: "all"})
	if err != nil {
		return
	}
	svc := &core.Service{DB: h.DB, ScopeKey: h.ScopeKey.Public().(ed25519.PublicKey)}
	for start := 0; start < len(files); start += 100 {
		batch := files[start:min(start+100, len(files))]
		ids := make([]string, len(batch))
		for n := range batch {
			ids[n] = batch[n].ID.String()
		}
		refs, err := svc.BatchGetFileRefs(ctx, &corepb.FileRefsRequest{Grant: grant, FileIds: ids})
		if err != nil {
			return
		}
		objects := make([]*pbv2.ObjectRef, len(refs.Files))
		for n, f := range refs.Files {
			objects[n] = &pbv2.ObjectRef{ObjectId: f.ObjectId, ContentGeneration: f.ContentGeneration, MimeType: f.MimeType}
		}
		response, err := h.Client.BatchCapabilities(ctx, &pbv2.BatchCapabilitiesRequest{Objects: objects})
		if err != nil || len(response.GetItems()) != len(objects) {
			slog.WarnContext(ctx, "directory AI status unavailable")
			return
		}
		statuses := make(map[string]string, len(refs.Files))
		for n, item := range response.Items {
			if item.GetObject().GetObjectId() != objects[n].ObjectId || item.GetObject().GetContentGeneration() != objects[n].ContentGeneration {
				return
			}
			statuses[refs.Files[n].FileId] = indexStatus(item.Capabilities)
		}
		for n := range batch {
			if value, ok := statuses[batch[n].ID.String()]; ok {
				batch[n].AIStatus = &value
			}
		}
	}
}
