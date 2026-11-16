package aisystem

import (
	"context"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/google/uuid"
)

// H04: protected old candidates must not permanently hide a later eligible
// candidate. Only temporary schemas created by this test are involved.
func TestArtifactGCBatchMakesProgressPastProtectedCandidates(t *testing.T) {
	db, store := databases(t)
	in := testInput()
	seedObject(t, db, in)
	var eligible uuid.UUID
	for n := range 101 {
		i := in
		i.Intent.RequestID = uuid.New()
		task, err := store.Ensure(context.Background(), spec("ai.source.prepare", i), "gc-batch")
		if err != nil {
			t.Fatal(err)
		}
		if n == 100 {
			if err := store.DB.Model(&asynccontrol.Task{}).Where("id=?", task.ID).Update("state", asynccontrol.Succeeded).Error; err != nil {
				t.Fatal(err)
			}
		}
		a := Artifact{ArtifactID: uuid.New(), ObjectID: in.Intent.ObjectID, ContentGeneration: 1, TaskID: task.ID, Generation: 1, Kind: task.Kind, InputDigest: task.InputDigest, ArtifactHash: aiprotocol.Hash([]byte("{}")), TypedOutputRef: uuid.New(), Complete: true, Data: []byte("{}"), ByteCount: 2, CreatedAt: time.Now().Add(-10 * 24 * time.Hour).Add(time.Duration(n) * time.Second)}
		if err := db.Create(&a).Error; err != nil {
			t.Fatal(err)
		}
		if n == 100 {
			eligible = a.ArtifactID
		}
	}
	publisher := Publisher{DB: db, Control: store}
	found := false
	for attempt := range 3 {
		gcIn := aiprotocol.Input{Intent: aiprotocol.Intent{ObjectID: in.Intent.ObjectID, ContentGeneration: 1, ContentHash: in.Intent.ContentHash, Size: in.Intent.Size}, MaintenanceAt: int64(attempt + 1)}
		task := claim(t, store, "ai.artifact.gc", gcIn)
		computePublish(t, &publisher, &Computer{DB: db, Profile: in.Profile}, task)
		var remaining int64
		if err := db.Model(&Artifact{}).Where("artifact_id=?", eligible).Count(&remaining).Error; err != nil {
			t.Fatal(err)
		}
		found = remaining == 0
		ids, _, err := publisher.collectableArtifacts(context.Background(), in.Intent)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) > 100 {
			t.Fatal("unbounded collection", len(ids))
		}
		if found {
			break
		}
		t.Logf("batch=%d no eligible artifact", attempt+1)
	}
	if !found {
		t.Fatal("100 protected oldest artifacts permanently hide the eligible 101st artifact")
	}
	var protected int64
	if err := db.Model(&Artifact{}).Where("object_id=? AND kind='ai.source.prepare'", in.Intent.ObjectID).Count(&protected).Error; err != nil {
		t.Fatal(err)
	}
	if protected != 100 {
		t.Fatal("GC deleted protected artifacts", protected)
	}
}
