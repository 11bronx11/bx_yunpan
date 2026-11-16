package aisystem

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/google/uuid"
)

// B03: every manifest identity/size field is verified before publication.
// This intentionally corrupts isolated defensive fixtures, never user data.
func TestPublishRejectsManifestTamperMatrix(t *testing.T) {
	db, store := databases(t)
	p := &Publisher{DB: db, Control: store}
	for _, tc := range []struct {
		name, column string
		value        any
	}{
		{"task", "task_id", uuid.New()}, {"generation", "generation", int64(42)},
		{"input", "input_digest", aiprotocol.Hash([]byte("wrong"))},
		{"artifact-hash", "artifact_hash", aiprotocol.Hash([]byte("wrong"))},
		{"kind", "kind", "ai.summary.generate"}, {"object", "object_id", uuid.New()},
		{"content-generation", "content_generation", int64(42)},
		{"output-ref", "typed_output_ref", uuid.New()}, {"row-count", "row_count", int64(2)},
		{"byte-count", "byte_count", int64(1)}, {"incomplete", "complete", false},
		{"payload", "data", jsonValue(map[string]string{"tampered": "yes"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := testInput()
			seedObject(t, db, in)
			task := claim(t, store, "ai.source.prepare", in)
			result, output := stage(t, db, store, task, in, sourceData{Parsed: ai.ParseResult{Sections: []rag.Section{{Text: "only current source", Kind: "text"}}}, Coverage: "complete"}, uuid.New(), 1)
			var original Artifact
			if err := db.First(&original, "artifact_id=?", output.ArtifactID).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&Artifact{}).Where("artifact_id=?", output.ArtifactID).Update(tc.column, tc.value).Error; err != nil {
				if tc.column == "complete" && strings.Contains(err.Error(), "23514") {
					// The database itself makes incomplete manifests unrepresentable.
					if outcome, err := p.Publish(context.Background(), task, result); err != nil || outcome != asynccontrol.Succeeded {
						t.Fatal("valid manifest blocked", err)
					}
					return
				}
				t.Fatal(err)
			}
			if _, err := p.Publish(context.Background(), task, result); err == nil {
				t.Fatal("corrupt manifest published")
			}
			var receipts, sources int64
			if err := db.Table("ai_commit_receipts").Where("result_id=?", result.ID).Count(&receipts).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Table("ai_sources").Where("object_id=?", in.Intent.ObjectID).Count(&sources).Error; err != nil {
				t.Fatal(err)
			}
			if receipts != 0 || sources != 0 {
				t.Fatal("partial effect after rejection")
			}
			if err := db.Model(&Artifact{}).Where("artifact_id=?", output.ArtifactID).Select("*").Updates(&original).Error; err != nil {
				t.Fatal(err)
			}
			if outcome, err := p.Publish(context.Background(), task, result); err != nil || outcome != asynccontrol.Succeeded {
				t.Fatalf("valid result cannot recover: %s %v", outcome, err)
			}
		})
	}
}

// B04: a receipt-write failure after the active-pointer update rolls back ALL
// local effects. The trigger is private to this test's isolated schema.
func TestReceiptFailureRollsBackActiveAndContent(t *testing.T) {
	db, store := databases(t)
	in := testInput()
	seedObject(t, db, in)
	task := claim(t, store, "ai.source.prepare", in)
	result, _ := stage(t, db, store, task, in, sourceData{Parsed: ai.ParseResult{Sections: []rag.Section{{Text: "rollback", Kind: "text"}}}, Coverage: "complete"}, uuid.New(), 1)
	if err := db.Exec(`CREATE FUNCTION reject_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture receipt barrier'; END $$; CREATE TRIGGER reject_receipt BEFORE INSERT ON ai_commit_receipts FOR EACH ROW EXECUTE FUNCTION reject_receipt()`).Error; err != nil {
		t.Fatal(err)
	}
	p := &Publisher{DB: db, Control: store}
	if _, err := p.Publish(context.Background(), task, result); err == nil {
		t.Fatal("barrier did not fail")
	}
	var object Object
	if err := db.First(&object, "object_id=?", in.Intent.ObjectID).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Table("ai_sources").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 || object.ActiveSourceID != nil {
		t.Fatal("transaction leaked content/active")
	}
	if err := db.Exec("DROP TRIGGER reject_receipt ON ai_commit_receipts").Error; err != nil {
		t.Fatal(err)
	}
	publish(t, p, task, result)
}

// B03/E03: an otherwise consistent, hash-valid text manifest cannot publish
// a child whose rune range does not describe its parent's actual text.
func TestPublishRejectsForgedChildRange(t *testing.T) {
	db, store, publisher, in, _ := readyText(t)
	in.Intent.RequestID = uuid.New()
	in.Intent.Seqs.Text++
	if err := db.Exec("UPDATE ai_objects SET desired_text_seq=? WHERE object_id=?", in.Intent.Seqs.Text, in.Intent.ObjectID).Error; err != nil {
		t.Fatal(err)
	}
	task := claim(t, store, "ai.text.build", in)
	id := uuid.New()
	chunks, _ := rag.BuildParentChild(id, []rag.Section{{Text: "中文🙂 exact ranges", Kind: "text"}})
	chunks.Children[0].Content = "forged excerpt"
	result, _ := stage(t, db, store, task, in, textData{chunks}, id, int64(len(chunks.Parents)+len(chunks.Children)))
	if _, err := publisher.Publish(context.Background(), task, result); err == nil {
		t.Fatal("forged child published")
	}
	var count int64
	if err := db.Table("ai_content_versions").Where("id=?", id).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed text version leaked")
	}
	var body aiprotocol.Output
	if json.Unmarshal(result.Body, &body) != nil {
		t.Fatal("bad fixture")
	}
}
