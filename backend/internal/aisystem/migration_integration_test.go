package aisystem

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncjobs"
	"github.com/11bronx11/bx_yunpan/backend/internal/core"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/testdb"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func TestRetainedMigrationResumeBudgetsAndArchive(t *testing.T) {
	biz := testdb.Business(t)
	db, s := databases(t)
	ctx := context.Background()
	t.Setenv("AI_LEGACY_WRITERS_STOPPED", "true")
	in := testInput()
	objects := []objectstore.Object{}
	for n, mime := range []string{"text/plain", "text/plain", "application/octet-stream"} {
		o := objectstore.Object{ID: uuid.New(), SHA256: aiprotocol.Hash([]byte{byte(n)}), SizeBytes: 10, MimeType: mime, Bucket: "fixture", ObjectKey: uuid.NewString(), Status: "ready", ReferenceCount: 1, Version: 1}
		if err := biz.Create(&o).Error; err != nil {
			t.Fatal(err)
		}
		objects = append(objects, o)
	}
	summary := "retained old summary"
	doc := ai.Document{ID: uuid.New(), ObjectID: objects[0].ID, Status: "indexed", Tags: []string{}, Summary: &summary}
	if err := biz.Create(&doc).Error; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Millisecond)
	for n, o := range objects[:2] {
		input := json.RawMessage(jsonValue(map[string]any{"object_id": o.ID, "content_generation": 1}))
		task, err := s.Ensure(ctx, asynccontrol.Spec{Kind: "ai.index", SemanticKey: "legacy:" + o.ID.String(), OwnerKey: "fixture", ResourceKey: "object:" + o.ID.String(), ShardKey: "object:" + o.ID.String(), Input: input, Deadline: deadline, Attempts: 3, Dispatches: 4, Recoveries: 2}, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			if err := s.DB.Model(&asynccontrol.Task{}).Where("id=?", task.ID).Update("state", asynccontrol.NeedsAttention).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	run := uuid.New()
	m := Migrator{Read: biz, AI: db, Control: s.DB, IntentWriter: biz, Profile: in.Profile, Revision: "fixture-v1", Batch: 1}
	for {
		r, err := m.Plan(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		if !r.HasMore {
			break
		}
	}
	for {
		r, err := m.Handoff(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		if !r.HasMore {
			break
		}
	}
	// Repeat after a simulated tool restart, preserving the durable cursor.
	if _, err := m.Handoff(ctx, run); err != nil {
		t.Fatal(err)
	}
	// This fixture has no legacy broker messages. The full-chain test exercises
	// a real vhost; cutover still requires its explicit transfer checkpoint.
	if err := m.MarkMessagesTransferred(ctx, run); err != nil {
		t.Fatal(err)
	}
	for {
		r, err := m.Enqueue(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		if !r.HasMore {
			break
		}
	}
	if _, err := m.Enqueue(ctx, run); err != nil {
		t.Fatal(err)
	}
	var events []struct{ Payload json.RawMessage }
	if err := biz.Table("outbox_events").Where("event_type=?", aiprotocol.IntentEvent).Find(&events).Error; err != nil || len(events) != 1 {
		t.Fatal("migration duplicated/reset intents", len(events), err)
	}
	var intent aiprotocol.Intent
	if json.Unmarshal(events[0].Payload, &intent) != nil || intent.Legacy == nil || intent.Legacy.Attempts != 3 || intent.Legacy.Dispatches != 4 || intent.Legacy.Recoveries != 2 || !intent.Legacy.Deadline.Equal(deadline) {
		t.Fatal("legacy budget lost")
	}
	var stopped int64
	_ = db.Table("ai_migration_items").Where("run_id=? AND conclusion='stopped_preserved'", run).Count(&stopped)
	if stopped != 1 {
		t.Fatal("old stop was reopened")
	}
	if _, err := m.CutoverCheck(ctx, run); err == nil {
		t.Fatal("cutover accepted unfinished rebuild")
	}
	// Publish a real parent/child version for the historical object's intent.
	in.Intent = intent
	seedObject(t, db, in)
	p := &Publisher{DB: db, Control: s}
	sourceTask := claim(t, s, "ai.source.prepare", in)
	in.SourceID = uuid.New()
	r, _ := stage(t, db, s, sourceTask, in, sourceData{Parsed: ai.ParseResult{ParserID: "text", ParserVersion: "v1", Sections: []rag.Section{{Text: "retained rollback", Kind: "text"}}}, Coverage: "complete"}, in.SourceID, 1)
	publish(t, p, sourceTask, r)
	textTask := claim(t, s, "ai.text.build", in)
	computePublish(t, p, &Computer{DB: db, Profile: in.Profile}, textTask)
	if _, err := m.Verify(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CutoverCheck(ctx, run); err != nil {
		t.Fatal(err)
	}
	var schema string
	_ = biz.Raw("SELECT current_schema()").Scan(&schema)
	archiveSchema := schema + "_ai_legacy_20261116"
	t.Cleanup(func() { _ = biz.Exec(`DROP SCHEMA IF EXISTS "` + archiveSchema + `" CASCADE`).Error })
	for _, direction := range []string{"up", "down"} {
		raw, err := os.ReadFile("../../archive-migrations/000001_archive_legacy_ai_search." + direction + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		if err := biz.Exec(string(raw)).Error; err != nil {
			t.Fatal("archive "+direction, err)
		}
		table := schema + ".ai_documents"
		if direction == "up" {
			table = archiveSchema + ".ai_documents"
		}
		var stored string
		if err := biz.Table(table).Select("summary").Where("id=?", doc.ID).Scan(&stored).Error; err != nil || stored != summary {
			t.Fatal("archive lost original content", err)
		}
	}
	var total int64
	_ = biz.Table("file_objects").Count(&total)
	if total != 3 {
		t.Fatal("migration altered business objects")
	}
	var hash string
	_ = db.Table("ai_migration_items").Select("source_hash").Where("run_id=? AND object_id=?", run, objects[0].ID).Scan(&hash)
	if len(hash) != 64 || strings.Trim(hash, "0") == "" {
		t.Fatal("manifest lost source identity")
	}
}

func TestMigrationAttachesJobsWithoutChangingFrozenWork(t *testing.T) {
	db, s := databases(t)
	ctx := context.Background()
	in := testInput()
	root, err := s.Ensure(ctx, spec("ai.intent.apply", in), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	m := Migrator{AI: db, Control: s.DB, Profile: in.Profile}
	observer := aiprotocol.JobObserver{JobID: uuid.New(), OwnerID: uuid.New()}
	for range 2 {
		if err := m.attachJobs(ctx, in.Intent.RequestID, []aiprotocol.JobObserver{observer}); err != nil {
			t.Fatal(err)
		}
	}
	// Descendants frozen before attachment carry no observer in their input.
	// Online callbacks must still link and finish the historical user job.
	child, err := s.Ensure(ctx, spec("ai.source.prepare", in), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&asynccontrol.Task{}).Where("id=?", root.ID).Update("state", asynccontrol.Succeeded).Error; err != nil {
			return err
		}
		if err := tx.Model(&asynccontrol.Task{}).Where("id=?", child.ID).Update("state", asynccontrol.NeedsAttention).Error; err != nil {
			return err
		}
		return (&Planner{}).Finish(tx, s, child, asynccontrol.NeedsAttention)
	})
	if err != nil {
		t.Fatal(err)
	}
	var frozen asynccontrol.Task
	if err := s.DB.Where("id=?", root.ID).Take(&frozen).Error; err != nil || frozen.InputDigest != root.InputDigest || frozen.Attempts != root.Attempts || frozen.Deadline != root.Deadline {
		t.Fatal("attachment changed frozen identity/budget", err)
	}
	var links, finishes int64
	_ = s.DB.Table("ai_job_task_links").Where("job_id=?", observer.JobID).Count(&links)
	_ = s.DB.Table("tasks").Where("kind='ai.job.finish' AND input->>'job_id'=?", observer.JobID.String()).Count(&finishes)
	if links != 2 || finishes != 1 {
		t.Fatal("attached historical job lost descendants or completion", links, finishes)
	}
}

func TestUnmappedLegacyJobGetsDurableCoreConclusion(t *testing.T) {
	biz := testdb.Business(t)
	db, store := databases(t)
	ctx := context.Background()
	t.Setenv("AI_LEGACY_WRITERS_STOPPED", "true")
	owner := identity.User{ID: uuid.New(), Username: "legacy-" + uuid.NewString(), PasswordHash: "fixture", Status: 1}
	owner.UsernameNormalized = owner.Username
	if err := biz.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: uuid.New(), OwnerID: owner.ID, ResourceID: uuid.New(), ResourceType: "file", TaskType: "ai.reprocess", DedupeKey: uuid.NewString(), Status: "processing", Version: 1}
	if err := biz.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	m := Migrator{Read: biz, AI: db, Control: store.DB, IntentWriter: biz, Profile: testInput().Profile, Revision: "fixture", Batch: 1}
	run := uuid.New()
	if _, err := m.Plan(ctx, run); err != nil {
		t.Fatal(err)
	}
	for {
		r, err := m.Handoff(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		if !r.HasMore {
			break
		}
	}
	if err := m.MarkMessagesTransferred(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CutoverCheck(ctx, run); err == nil {
		t.Fatal("archive accepted a processing historical job")
	}
	if err := store.PrepareCommands(ctx, 10); err != nil {
		t.Fatal(err)
	}
	var task asynccontrol.Task
	if err := store.DB.Where("kind='ai.job.finish'").Take(&task).Error; err != nil {
		t.Fatal(err)
	}
	committer := &asyncjobs.Committer{Business: biz, Control: store}
	if err := committer.Commit(ctx, asynccontrol.Notice{Version: 1, TaskID: task.ID, Shard: task.Shard, ResultID: task.ResultID}); err != nil {
		t.Fatal(err)
	}
	if err := biz.Where("id=?", job.ID).Take(&job).Error; err != nil || job.Status != "needs_attention" {
		t.Fatal("historical job stayed processing", err, job.Status)
	}
	if _, err := m.CutoverCheck(ctx, run); err != nil {
		t.Fatal(err)
	}
}
