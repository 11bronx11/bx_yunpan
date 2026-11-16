package asyncjobs_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/aisystem"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncjobs"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/serviceauth"
	"github.com/11bronx11/bx_yunpan/backend/internal/testdb"
	"github.com/google/uuid"
)

func TestAIRedoRenewsGrantAndRetainsStoppedBudget(t *testing.T) {
	business, control := testdb.Business(t), testdb.Control(t)
	s := asynccontrol.New(control)
	aisystem.Register(s.Registry)
	ctx := context.Background()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	profile := aiprotocol.Profile{ID: "default", Revision: "v1", Dimension: 1024, EnableVectors: true, EnableSummary: true}
	aiPlanner := &aisystem.Planner{Profile: profile, ProcessingKey: private}
	planner := &asyncjobs.Planner{Read: business, AIIntent: aiPlanner.Intent}
	object := objectstore.Object{ID: uuid.New(), SHA256: strings.Repeat("a", 64), SizeBytes: 10, MimeType: "text/plain", Bucket: "fixture", ObjectKey: uuid.NewString(), ContentGeneration: 1, ReferenceCount: 1, Status: "ready", Version: 1}
	if err := business.Create(&object).Error; err != nil {
		t.Fatal(err)
	}
	intent := aiprotocol.Intent{Version: 2, RequestedAt: time.Now().Add(-48 * time.Hour).Unix(), RequestID: uuid.New(), ObjectID: object.ID, ContentGeneration: 1, ContentHash: object.SHA256, Size: 10, MediaType: object.MimeType, ProfileID: profile.ID, ProfileRevision: profile.Revision, Targets: []string{"vectors"}, Seqs: aiprotocol.Seqs{Source: 1, Text: 1, Vector: 1, Summary: 1}, JobID: uuid.New()}
	if err := business.Exec(`INSERT INTO ai_resource_intents(object_id,content_generation,source_seq,text_seq,vector_seq,summary_seq,last_request_id,profile_id,profile_revision) VALUES(?,1,1,1,1,1,?,?,?)`, object.ID, intent.RequestID, profile.ID, profile.Revision).Error; err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(aiprotocol.Input{Intent: intent, Profile: profile, ProcessingGrant: "expired"})
	stopped := func() asynccontrol.Task {
		t.Helper()
		task, err := s.Ensure(ctx, asynccontrol.Spec{Kind: "ai.embedding.generate", SemanticKey: "fixture:" + uuid.NewString(), OwnerKey: "fixture", ResourceKey: "object:" + object.ID.String() + ":1", ShardKey: object.ID.String(), Input: input, Attempts: 3, Dispatches: 4, Recoveries: 2}, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		if err := control.Model(&asynccontrol.Task{}).Where("id=?", task.ID).Updates(map[string]any{"state": asynccontrol.NeedsAttention, "deadline": time.Now().Add(-time.Hour)}).Error; err != nil {
			t.Fatal(err)
		}
		return task
	}
	old := stopped()
	child, err := planner.Redo(ctx, s, old.ID, "operator verified provider recovery")
	if err != nil {
		t.Fatal(err)
	}
	again, err := planner.Redo(ctx, s, old.ID, "operator verified provider recovery")
	if err != nil || again.ID != child.ID {
		t.Fatal("redo duplicated the audited child", err)
	}
	var renewed aiprotocol.Input
	if json.Unmarshal(child.Input, &renewed) != nil {
		t.Fatal("invalid renewed input")
	}
	var grant aiprotocol.ProcessingGrant
	if serviceauth.Verify(public, renewed.ProcessingGrant, "core-processing-v1", &grant) != nil || !grant.Valid(renewed) {
		t.Fatal("redo reused expired authorization")
	}
	if child.Kind != "ai.intent.apply" || child.Queue != "ai-content" || child.ParentID == nil || *child.ParentID != old.ID || renewed.Intent.JobID != uuid.Nil || child.Attempts != 0 || !child.Deadline.After(time.Now()) {
		t.Fatal("redo did not resolve fresh prerequisites as a separate audited task")
	}
	var original asynccontrol.Task
	if err := control.First(&original, "id=?", old.ID).Error; err != nil || original.Attempts != 3 || original.Dispatches != 4 || original.Recoveries != 2 || !original.Deadline.Before(time.Now()) || !asynccontrol.Terminal(original.State) {
		t.Fatal("redo changed the original execution budget", err)
	}
	newer := stopped()
	if err := business.Exec("UPDATE ai_resource_intents SET vector_seq=2 WHERE object_id=?", object.ID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := planner.Redo(ctx, s, newer.ID, "operator verified provider recovery"); err == nil {
		t.Fatal("redo accepted a superseded business intent")
	}
	if err := business.Exec("UPDATE ai_resource_intents SET vector_seq=1 WHERE object_id=?", object.ID).Error; err != nil {
		t.Fatal(err)
	}
	deleted := stopped()
	if err := business.Exec("UPDATE file_objects SET reference_count=0 WHERE id=?", object.ID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := planner.Redo(ctx, s, deleted.ID, "operator verified provider recovery"); err == nil {
		t.Fatal("redo accepted an object without a live reference")
	}
}
