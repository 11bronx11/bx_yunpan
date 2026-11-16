package asynccontrol

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const (
	EffectNone = "none"
	EffectCore = "core"
	EffectAI   = "ai"
)

// Target is the only compatibility mapping for the former business-effect bit.
func (d Definition) Target() string {
	if d.EffectTarget != "" {
		return d.EffectTarget
	}
	if d.BusinessEffect {
		return EffectCore
	}
	return EffectNone
}

func WorkQueues() []string {
	return []string{"upload", "object", "media", "gc", "ai", "search", "ai-content", "ai-embedding", "ai-summary", "ai-project-text", "ai-project-vector", "ai-maintenance"}
}

type Continuation struct {
	ParentTaskID, AcceptedResultID uuid.UUID
	Ordinal                        int
	Spec                           datatypes.JSON `gorm:"type:jsonb"`
	SpecDigest, State              string
	ChildTaskID                    *uuid.UUID
	CreatedAt                      time.Time
	ReleasedAt                     *time.Time
}

func (Continuation) TableName() string { return "task_continuations" }

func (s *Store) freezeContinuations(tx *gorm.DB, t Task, r Result) error {
	if s.PlanContinuations == nil {
		return nil
	}
	specs, err := s.PlanContinuations(t, r)
	if err != nil {
		return err
	}
	if len(specs) > 128 {
		return ErrInvalid
	}
	for i, spec := range specs {
		if _, err := s.Registry.Validate(spec); err != nil {
			return err
		}
		// Bound descendants by their root authorization and execution lifetime.
		if spec.Kind != "ai.artifact.gc" && (spec.Deadline.IsZero() || spec.Deadline.After(t.Deadline)) {
			spec.Deadline = t.Deadline
		}
		spec.ParentID = &t.ID
		raw, err := json.Marshal(spec)
		if err != nil {
			return err
		}
		digest, raw, err := Digest(raw)
		if err != nil {
			return err
		}
		row := Continuation{ParentTaskID: t.ID, AcceptedResultID: r.ID, Ordinal: i, Spec: raw, SpecDigest: digest, State: "blocked"}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}

// Called inside the parent completion transaction. A failed child ensure rolls
// back the parent receipt acknowledgement as well as all sibling releases.
func (s *Store) releaseContinuations(tx *gorm.DB, t Task, r Result, outcome string) error {
	var rows []Continuation
	if err := tx.Where("accepted_result_id=? AND state='blocked'", r.ID).Order("ordinal").Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		if outcome != Succeeded {
			if err := tx.Model(&Continuation{}).Where("accepted_result_id=? AND ordinal=?", r.ID, row.Ordinal).Update("state", "suppressed").Error; err != nil {
				return err
			}
			continue
		}
		digest, _, err := Digest(row.Spec)
		if err != nil || digest != row.SpecDigest {
			return ErrConflict
		}
		var spec Spec
		if err := json.Unmarshal(row.Spec, &spec); err != nil {
			return err
		}
		child, err := s.ensure(tx, spec, fmt.Sprintf("continuation:%s:%d", r.ID, row.Ordinal))
		if err != nil {
			return err
		}
		inputDigest, _, err := Digest(spec.Input)
		if err != nil || child.InputDigest != inputDigest || child.Kind != spec.Kind || child.ShardKey != spec.ShardKey {
			return ErrConflict
		}
		if err := tx.Model(&Continuation{}).Where("accepted_result_id=? AND ordinal=? AND state='blocked'", r.ID, row.Ordinal).Updates(map[string]any{"state": "released", "child_task_id": child.ID, "released_at": gorm.Expr("clock_timestamp()")}).Error; err != nil {
			return err
		}
	}
	return nil
}
