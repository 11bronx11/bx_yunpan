package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/aisystem"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncjobs"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dependencies"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/serviceauth"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: async-admin inspect <session/operation/object/task UUID> | redo <task UUID> <reason>")
		os.Exit(2)
	}
	cfg, err := config.LoadForRole("async-admin")
	must(err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	deps, err := dependencies.OpenAsync(ctx, cfg, false)
	must(err)
	defer deps.Close()
	s := asynccontrol.New(deps.Control)
	aisystem.Register(s.Registry)
	id, err := uuid.Parse(os.Args[2])
	must(err)
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	switch os.Args[1] {
	case "inspect":
		var rows []map[string]any
		must(s.DB.WithContext(ctx).Raw(`SELECT t.id,t.semantic_key,t.kind,t.effect_target,t.state,t.resource_key,t.parent_id,t.attempts,t.max_attempts,t.dispatches,t.recoveries,t.result_sends,t.generation,t.result_id,t.last_error,t.trace,t.created_at,t.updated_at,
  (SELECT jsonb_agg(source_key) FROM task_sources WHERE task_id=t.id) AS sources,
  (SELECT jsonb_agg(jsonb_build_object('generation',generation,'outcome',outcome,'error',error,'outputs',outputs)) FROM executions WHERE task_id=t.id) AS executions,
  (SELECT jsonb_agg(jsonb_build_object('id',id,'outcome',outcome,'effect_target',effect_target,'output',body,'committed_at',committed_at)) FROM results WHERE task_id=t.id) AS results,
  (SELECT jsonb_agg(jsonb_build_object('ordinal',ordinal,'state',state,'child_task_id',child_task_id)) FROM task_continuations WHERE parent_task_id=t.id) AS continuations,
  (SELECT jsonb_agg(jsonb_build_object('job_id',job_id,'target',target)) FROM ai_job_task_links WHERE task_id=t.id) AS ai_jobs
  FROM tasks t WHERE t.id=? OR t.resource_key IN ? OR t.resource_key LIKE ? ORDER BY t.created_at LIMIT 100`, id, []string{"session:" + id.String(), "operation:" + id.String(), "object:" + id.String()}, "object:"+id.String()+":%").Scan(&rows).Error)
		var keys []string
		for _, row := range rows {
			if key, ok := row["semantic_key"].(string); ok {
				keys = append(keys, key)
			}
		}
		var receipts, events, quarantines, intents []map[string]any
		if len(keys) > 0 {
			must(deps.Read.WithContext(ctx).Table("async_commit_receipts").Where("semantic_key IN ?", keys).Find(&receipts).Error)
			must(s.DB.WithContext(ctx).Raw(`SELECT id,event_type,aggregate_id,status,reason,source_xid::text,source_seq FROM event_inbox WHERE 'event:'||id::text IN (SELECT source_key FROM task_sources WHERE task_id IN (SELECT id FROM tasks WHERE semantic_key IN ?)) LIMIT 100`, keys).Scan(&events).Error)
		}
		must(s.DB.WithContext(ctx).Table("quarantines").Where("task_id=? OR task_id IN (SELECT id FROM tasks WHERE resource_key IN ? OR resource_key LIKE ?)", id, []string{"session:" + id.String(), "operation:" + id.String(), "object:" + id.String()}, "object:"+id.String()+":%").Limit(100).Find(&quarantines).Error)
		must(deps.Read.WithContext(ctx).Table("storage_operations").Select("id,operation_type,upload_session_id,object_id,state,source_bucket,source_key,target_bucket,target_key").Where("id=? OR upload_session_id=? OR object_id=?", id, id, id).Limit(100).Find(&intents).Error)
		must(encoder.Encode(map[string]any{"tasks": rows, "events": events, "business_receipts": receipts, "quarantines": quarantines, "storage_intents": intents}))
	case "redo":
		if len(os.Args) != 4 {
			must(fmt.Errorf("redo requires one quoted reason"))
		}
		planner := &asyncjobs.Planner{Read: deps.Read, AIIntent: func(in aiprotocol.Intent) (asynccontrol.Spec, error) {
			if !cfg.AI.Enabled {
				return asynccontrol.Spec{}, fmt.Errorf("AI is disabled")
			}
			key, err := serviceauth.Private(cfg.AISplit.ProcessingSigningSeed)
			if err != nil {
				return asynccontrol.Spec{}, err
			}
			return (&aisystem.Planner{Profile: cfg.AIProfile(), ProcessingKey: key, TextEpoch: cfg.AITextEpoch(), VectorEpoch: cfg.AIVectorEpoch(), TargetEpoch: cfg.AIProjectionEpoch(), TargetNamespace: cfg.AISplit.ProjectionNamespace}).Intent(in)
		}}
		task, err := planner.Redo(ctx, s, id, os.Args[3])
		must(err)
		must(encoder.Encode(map[string]any{"id": task.ID, "semantic_key": task.SemanticKey, "state": task.State}))
	default:
		must(fmt.Errorf("unknown action"))
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
