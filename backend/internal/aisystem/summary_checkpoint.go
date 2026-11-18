package aisystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type summaryCheckpoint struct {
	SourceID  uuid.UUID
	InputHash string
	Insight   ai.Insight
}

// Node identity includes the frozen task input (source/model/prompt), algorithm,
// level, position and exact input text. Generations share completed nodes, while
// an explicit new task or changed input never borrows a previous summary.
func (c *Computer) summarizeTask(ctx context.Context, parts []string, t asynccontrol.Task, in aiprotocol.Input) (ai.Insight, []string, error) {
	return c.summarizeParts(ctx, parts, func(ctx context.Context, level, index int, text string) (ai.Insight, error) {
		inputHash := aiprotocol.Hash([]byte(text))
		kind := "ai.summary.checkpoint.v1:" + aiprotocol.Hash([]byte(fmt.Sprintf("%d:%d:%s", level, index, inputHash)))
		id := uuid.NewSHA1(t.ID, []byte(t.InputDigest+":"+kind))
		load := func() (ai.Insight, error) {
			var artifact Artifact
			if err := c.DB.WithContext(ctx).Table("ai_summary_checkpoints").Where("artifact_id=? AND task_id=? AND input_digest=? AND kind=?", id, t.ID, t.InputDigest, kind).Take(&artifact).Error; err != nil {
				return ai.Insight{}, err
			}
			var node summaryCheckpoint
			hash, canonical, err := asynccontrol.Digest(artifact.Data)
			if err != nil || hash != artifact.ArtifactHash || len(canonical) > 1<<20 || artifact.ByteCount != int64(len(canonical)) || artifact.Generation < 1 || artifact.Generation > t.Generation || !artifact.Complete || artifact.RowCount != 1 || artifact.TypedOutputRef != id || artifact.ObjectID != in.Intent.ObjectID || artifact.ContentGeneration != in.Intent.ContentGeneration || json.Unmarshal(artifact.Data, &node) != nil || node.SourceID != in.SourceID || node.InputHash != inputHash || strings.TrimSpace(node.Insight.Summary) == "" {
				return ai.Insight{}, asynccontrol.ErrInvalid
			}
			return node.Insight, nil
		}
		if cached, err := load(); err == nil {
			return cached, nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return ai.Insight{}, err
		}
		insight, err := c.Summary.Enrich(ctx, text)
		if err != nil {
			return insight, &modelBuildError{stage: "summary", cause: err}
		}
		if strings.TrimSpace(insight.Summary) == "" {
			return insight, &ai.ParseError{Code: "ai.summary.empty", Permanent: true}
		}
		raw, err := json.Marshal(summaryCheckpoint{SourceID: in.SourceID, InputHash: inputHash, Insight: insight})
		if err != nil {
			return ai.Insight{}, err
		}
		if len(raw) > 1<<20 {
			return ai.Insight{}, &ai.ParseError{Code: "ai.summary.resource_limit", ResourceLimit: true}
		}
		hash, canonical, err := asynccontrol.Digest(raw)
		if err != nil {
			return ai.Insight{}, err
		}
		artifact := Artifact{ArtifactID: id, TaskID: t.ID, Generation: t.Generation, Kind: kind, InputDigest: t.InputDigest, ArtifactHash: hash, TypedOutputRef: id, ObjectID: in.Intent.ObjectID, ContentGeneration: in.Intent.ContentGeneration, RowCount: 1, ByteCount: int64(len(canonical)), Complete: true, Data: canonical}
		if err := c.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&artifact).Error; err != nil {
			return ai.Insight{}, err
		}
		// Concurrent attempts must use the same stored winner at the next level.
		return load()
	})
}
