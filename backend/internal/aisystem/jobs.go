package aisystem

import (
	"encoding/json"
	"strings"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dblock"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func (p *Planner) Observe(tx *gorm.DB, t asynccontrol.Task) error {
	if !strings.HasPrefix(t.Kind, "ai.") || t.Kind == "ai.job.finish" {
		return nil
	}
	in, err := decode[aiprotocol.Input](t.Input)
	if err != nil {
		return err
	}
	observers, err := requestObservers(tx, in.Intent)
	if err != nil {
		return err
	}
	for _, observer := range observers {
		if err := p.observeJob(tx, t, in, observer); err != nil {
			return err
		}
	}
	return nil
}

func jobObservers(i aiprotocol.Intent) []aiprotocol.JobObserver {
	observers := append([]aiprotocol.JobObserver{}, i.Observers...)
	if i.JobID != uuid.Nil {
		observers = append(observers, aiprotocol.JobObserver{JobID: i.JobID, OwnerID: i.OwnerID})
	}
	return observers
}

func requestObservers(tx *gorm.DB, i aiprotocol.Intent) ([]aiprotocol.JobObserver, error) {
	if err := dblock.Transaction(tx, "ai-job-observers:"+i.RequestID.String()); err != nil {
		return nil, err
	}
	observers := jobObservers(i)
	var attached []aiprotocol.JobObserver
	if err := tx.Table("ai_job_observers").Where("request_id=?", i.RequestID).Find(&attached).Error; err != nil {
		return nil, err
	}
	seen := map[uuid.UUID]bool{}
	out := []aiprotocol.JobObserver{}
	for _, o := range append(observers, attached...) {
		if !seen[o.JobID] {
			seen[o.JobID] = true
			out = append(out, o)
		}
	}
	return out, nil
}

func (p *Planner) observeJob(tx *gorm.DB, t asynccontrol.Task, in aiprotocol.Input, observer aiprotocol.JobObserver) error {
	targets := in.Intent.Targets
	b, _ := json.Marshal(targets)
	if err := tx.Exec(`INSERT INTO ai_job_aggregates(job_id,requested_targets) VALUES(?,?::jsonb) ON CONFLICT DO NOTHING`, observer.JobID, string(b)).Error; err != nil {
		return err
	}
	cap := map[string]string{"ai.intent.apply": "intent", "ai.source.prepare": "source", "ai.text.build": "text", "ai.embedding.generate": "vectors", "ai.summary.generate": "summary", "ai.project.text": "projection_text", "ai.project.vector": "projection_vector"}[t.Kind]
	if cap == "" {
		return nil
	}
	return tx.Exec(`INSERT INTO ai_job_task_links(job_id,task_id,target,required,requested_identity) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, observer.JobID, t.ID, cap, true, t.InputDigest).Error
}

func (p *Planner) Finish(tx *gorm.DB, s *asynccontrol.Store, t asynccontrol.Task, outcome string) error {
	if !strings.HasPrefix(t.Kind, "ai.") || t.Kind == "ai.job.finish" {
		return nil
	}
	in, err := decode[aiprotocol.Input](t.Input)
	if err != nil {
		return err
	}
	observers, err := requestObservers(tx, in.Intent)
	if err != nil {
		return err
	}
	for _, observer := range observers {
		if err := p.finishJob(tx, s, t, in, observer); err != nil {
			return err
		}
	}
	return nil
}

func (p *Planner) finishJob(tx *gorm.DB, s *asynccontrol.Store, t asynccontrol.Task, in aiprotocol.Input, observer aiprotocol.JobObserver) error {
	var agg struct {
		RequestedTargets json.RawMessage
		FinishTaskID     *uuid.UUID
	}
	if err := tx.Raw("SELECT requested_targets,finish_task_id FROM ai_job_aggregates WHERE job_id=? FOR UPDATE", observer.JobID).Scan(&agg).Error; err != nil {
		return err
	}
	if agg.FinishTaskID != nil {
		return nil
	}
	var targets []string
	if err := json.Unmarshal(agg.RequestedTargets, &targets); err != nil {
		return err
	}
	var rows []struct {
		Target, State string
		ResultBody    json.RawMessage
	}
	if err := tx.Raw(`SELECT l.target,CASE WHEN t.state IN ('done','stopped') THEN t.outcome ELSE t.state END AS state,r.body AS result_body FROM ai_job_task_links l JOIN tasks t ON t.id=l.task_id LEFT JOIN results r ON r.id=t.result_id WHERE l.job_id=?`, observer.JobID).Scan(&rows).Error; err != nil {
		return err
	}
	states := map[string]string{}
	versions := map[string]uuid.UUID{}
	for _, r := range rows {
		states[r.Target] = r.State
		if r.State == asynccontrol.Succeeded && (r.Target == "source" || r.Target == "text" || r.Target == "vectors" || r.Target == "summary") {
			var output aiprotocol.Output
			if json.Unmarshal(r.ResultBody, &output) == nil && output.Validate() == nil && output.ObjectID == in.Intent.ObjectID && output.ContentGeneration == in.Intent.ContentGeneration {
				versions[r.Target] = output.OutputID
			}
		}
	}
	outcomes := map[string]string{}
	successful := 0
	for _, target := range targets {
		state := states[target]
		if state == "" {
			if stopped(states["intent"]) || stopped(states["source"]) && target != "source" || stopped(states["text"]) && (target == "vectors" || target == "projection_text" || target == "projection_vector") || stopped(states["vectors"]) && target == "projection_vector" {
				state = "failed"
			} else {
				return nil
			}
		}
		if !asynccontrol.Terminal(state) {
			return nil
		}
		outcomes[target] = state
		if state == asynccontrol.Succeeded {
			successful++
		}
	}
	status := "failed"
	if successful == len(targets) {
		status = "succeeded"
	} else if successful > 0 {
		status = "partial_success"
	}
	finish := aiprotocol.JobFinish{JobID: observer.JobID, OwnerID: observer.OwnerID, ObjectID: in.Intent.ObjectID, Outcomes: outcomes, Status: status, VersionRefs: versions}
	b, _ := json.Marshal(finish)
	child, err := s.EnsureTx(tx, asynccontrol.Spec{Kind: "ai.job.finish", SemanticKey: "ai.v2.job.finish:" + finish.JobID.String(), OwnerKey: t.OwnerKey, ResourceKey: t.ResourceKey, ShardKey: t.ShardKey, Input: b}, "job:"+finish.JobID.String())
	if err != nil {
		return err
	}
	return tx.Exec("UPDATE ai_job_aggregates SET outcomes=?::jsonb,finish_task_id=?,revision=revision+1,updated_at=clock_timestamp() WHERE job_id=?", string(jsonValue(outcomes)), child.ID, finish.JobID).Error
}
func stopped(s string) bool { return asynccontrol.Terminal(s) && s != asynccontrol.Succeeded }
