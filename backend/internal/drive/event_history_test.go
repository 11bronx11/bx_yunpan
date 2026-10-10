package drive

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
)

func TestMutationEventsPreservePreviousMetadata(t *testing.T) {
	f := newMoveFixture(t)
	ctx := outbox.WithV2EventsEnabled(context.Background(), true)
	check := func(id uuid.UUID, kind string, oldKey, oldValue, newKey, newValue string) {
		t.Helper()
		var event outbox.Event
		if err := f.db.Where("aggregate_id = ? AND event_type = ?", id, kind).Order("created_at DESC").First(&event).Error; err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload[oldKey] != oldValue || payload[newKey] != newValue {
			t.Fatalf("incorrect mutation history: %s", event.Payload)
		}
	}
	if _, err := f.service.RenameWithContext(ctx, f.ownerID, f.source.ID, 1, "renamed"); err != nil {
		t.Fatal(err)
	}
	check(f.source.ID, eventcontract.EventFolderRenamedV1, "old_name", "source", "new_name", "renamed")
	if _, err := f.service.MoveWithContext(ctx, f.ownerID, f.source.ID, f.target.ID, 2); err != nil {
		t.Fatal(err)
	}
	check(f.source.ID, eventcontract.EventFolderMovedV1, "old_parent_id", f.source.ParentID.String(), "new_parent_id", f.target.ID.String())
	if _, err := f.service.RenameFileWithContext(ctx, f.ownerID, f.file.ID, 1, "renamed.pdf"); err != nil {
		t.Fatal(err)
	}
	check(f.file.ID, eventcontract.EventFileUpdatedV1, "old_name", "report.pdf", "new_name", "renamed.pdf")
	if _, err := f.service.MoveFileWithContext(ctx, f.ownerID, f.file.ID, f.target.ID, 2); err != nil {
		t.Fatal(err)
	}
	check(f.file.ID, eventcontract.EventFileUpdatedV1, "old_folder_id", f.source.ID.String(), "new_folder_id", f.target.ID.String())
}
