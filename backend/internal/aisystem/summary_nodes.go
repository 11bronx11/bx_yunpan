package aisystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type summaryNode struct {
	NodeID, TaskID, ObjectID, SourceID                         uuid.UUID
	InputDigest, ProfileDigest, Algorithm, InputHash, DataHash string
	ContentGeneration                                          int64
	Level, Position                                            int
	Data                                                       datatypes.JSON
	CreatedAt                                                  time.Time
}

func (summaryNode) TableName() string { return "ai_summary_nodes" }

func (c *Computer) summarizeDirect(ctx context.Context, parts []string, t asynccontrol.Task, in aiprotocol.Input) (ai.Insight, []string, error) {
	return c.summarizeParts(ctx, parts, func(ctx context.Context, level, index int, text string) (ai.Insight, error) {
		hash := aiprotocol.Hash([]byte(text))
		kind := "ai.summary.checkpoint.v1:" + aiprotocol.Hash([]byte(fmt.Sprintf("%d:%d:%s", level, index, hash)))
		id := uuid.NewSHA1(t.ID, []byte(t.InputDigest+":"+kind))
		load := func() (ai.Insight, error) {
			var row summaryNode
			if e := c.DB.WithContext(ctx).Where("node_id=?", id).Take(&row).Error; e != nil {
				return ai.Insight{}, e
			}
			h, canonical, e := asynccontrol.Digest(row.Data)
			var node summaryCheckpoint
			if e != nil || h != row.DataHash || len(canonical) > 1<<20 || row.TaskID != t.ID || row.InputDigest != t.InputDigest || row.SourceID != in.SourceID || row.ObjectID != in.Intent.ObjectID || row.ContentGeneration != in.Intent.ContentGeneration || row.ProfileDigest != in.Profile.Digest() || row.InputHash != hash || json.Unmarshal(row.Data, &node) != nil || node.SourceID != in.SourceID || node.InputHash != hash || strings.TrimSpace(node.Insight.Summary) == "" {
				return ai.Insight{}, asynccontrol.ErrConflict
			}
			return node.Insight, nil
		}
		if cached, e := load(); e == nil {
			return cached, nil
		} else if !errors.Is(e, gorm.ErrRecordNotFound) {
			return ai.Insight{}, e
		}
		insight, e := c.Summary.Enrich(ctx, text)
		if e != nil {
			return insight, &modelBuildError{stage: "summary", cause: e}
		}
		if strings.TrimSpace(insight.Summary) == "" {
			return insight, &ai.ParseError{Code: "ai.summary.empty", Permanent: true}
		}
		raw, e := json.Marshal(summaryCheckpoint{SourceID: in.SourceID, InputHash: hash, Insight: insight})
		if e != nil {
			return ai.Insight{}, e
		}
		h, canonical, e := asynccontrol.Digest(raw)
		if e != nil {
			return ai.Insight{}, e
		}
		if len(canonical) > 1<<20 {
			return ai.Insight{}, &ai.ParseError{Code: "ai.summary.resource_limit", ResourceLimit: true}
		}
		row := summaryNode{NodeID: id, TaskID: t.ID, ObjectID: in.Intent.ObjectID, SourceID: in.SourceID, InputDigest: t.InputDigest, ProfileDigest: in.Profile.Digest(), Algorithm: "source-map-reduce-v1", InputHash: hash, DataHash: h, ContentGeneration: in.Intent.ContentGeneration, Level: level, Position: index, Data: canonical}
		if e := c.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; e != nil {
			return ai.Insight{}, e
		}
		return load()
	})
}
