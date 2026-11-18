package aisystem

import (
	"encoding/json"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/google/uuid"
)

func TestJobCompletionRetainsPublishedVersions(t *testing.T) {
	db, store := databases(t)
	store.PlanContinuations = nil
	publisher, in, _ := appendRetrievalBlocks(t, db, store, []rag.Section{{Text: "rollback summary"}})
	in.Intent.JobID, in.Intent.OwnerID = uuid.New(), uuid.New()
	in.Intent.Targets = []string{"summary"}
	task := claim(t, store, "ai.summary.generate", in)
	computePublish(t, publisher, &Computer{DB: db, Profile: in.Profile, Summary: ai.NewProvider(config.AI{Dimension: 1024}), SummaryCalls: 128}, task)
	var object Object
	if err := db.First(&object, "object_id=?", in.Intent.ObjectID).Error; err != nil {
		t.Fatal(err)
	}
	var finish asynccontrol.Task
	if err := store.DB.Where("kind='ai.job.finish' AND input->>'job_id'=?", in.Intent.JobID.String()).Take(&finish).Error; err != nil {
		t.Fatal(err)
	}
	var body struct {
		aiprotocol.JobFinish
		VersionRefs map[string]uuid.UUID `json:"result_version_refs"`
	}
	if err := json.Unmarshal(finish.Input, &body); err != nil || body.Status != "succeeded" || object.ActiveSummaryID == nil || body.VersionRefs["summary"] != *object.ActiveSummaryID {
		t.Fatal("job completion lost the published summary identity", body, err)
	}
}
