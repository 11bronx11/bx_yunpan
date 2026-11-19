package aigateway

import (
	"context"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pbv2"
	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/google/uuid"
)

func TestIndexStatus(t *testing.T) {
	ready := func() *pbv2.Capability {
		return &pbv2.Capability{Supported: true, Available: true, LatestBuild: &pbv2.LatestBuild{State: "succeeded", ObservedAt: "2026-11-17T00:00:00Z"}}
	}
	pending := func(observed bool) *pbv2.Capability {
		cap := &pbv2.Capability{Supported: true, LatestBuild: &pbv2.LatestBuild{State: "pending"}}
		if observed {
			cap.LatestBuild.ObservedAt = "2026-11-17T00:00:00Z"
		}
		return cap
	}
	failed := ready()
	failed.LatestBuild.State = "failed"
	refresh := ready()
	refresh.LatestBuild.State = "pending"
	disabled := ready()
	disabled.Supported = false
	disabled.LatestBuild.State = "failed"
	for _, test := range []struct {
		name string
		caps *pbv2.Capabilities
		want string
	}{
		{"missing response", nil, "missing"},
		{"unsupported file", &pbv2.Capabilities{}, "failed"},
		{"no recorded build", &pbv2.Capabilities{Source: pending(false), Text: pending(false)}, "missing"},
		{"queued build", &pbv2.Capabilities{Source: pending(true), Text: pending(true)}, "processing"},
		{"text ready while vectors are queued", &pbv2.Capabilities{Source: ready(), Text: ready(), Vector: pending(true)}, "processing"},
		{"failed source with pending downstream", &pbv2.Capabilities{Source: failed, Text: pending(true)}, "failed"},
		{"failed summary with usable text", &pbv2.Capabilities{Source: ready(), Text: ready(), Summary: failed}, "failed"},
		{"all stages ready", &pbv2.Capabilities{Source: ready(), Text: ready(), Vector: ready(), Summary: ready()}, "indexed"},
		{"text-only profile", &pbv2.Capabilities{Source: ready(), Text: ready()}, "indexed"},
		{"disabled stage cannot block", &pbv2.Capabilities{Source: ready(), Text: ready(), Vector: disabled}, "indexed"},
		{"refresh preserves old result but shows building", &pbv2.Capabilities{Source: ready(), Text: refresh}, "processing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := indexStatus(test.caps); got != test.want {
				t.Fatalf("status = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFileStatusesWithAIDisabledNeedsNoDependencies(t *testing.T) {
	files := []drive.FileView{{FileEntry: drive.FileEntry{ID: uuid.New()}}}
	new(HTTP).FileStatuses(context.Background(), uuid.New(), files)
	if files[0].AIStatus == nil || *files[0].AIStatus != "missing" {
		t.Fatalf("disabled AI status = %v", files[0].AIStatus)
	}
}
