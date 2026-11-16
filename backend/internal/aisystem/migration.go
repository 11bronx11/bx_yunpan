package aisystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dblock"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Migrator is an offline tool with explicit connections. Online AI processes
// never receive these business/read/write handles.
type Migrator struct {
	Read, AI, Control, IntentWriter *gorm.DB
	Profile                         aiprotocol.Profile
	Revision                        string
	Batch                           int
}
type MigrationReport struct {
	RunID       uuid.UUID `json:"run_id"`
	Stage       string    `json:"stage"`
	Items       int64     `json:"items"`
	Queued      int64     `json:"queued"`
	Quarantined int64     `json:"quarantined"`
	Verified    int64     `json:"verified"`
	HasMore     bool      `json:"has_more"`
}

func (m *Migrator) legacyStopped(ctx context.Context) error {
	if os.Getenv("AI_LEGACY_WRITERS_STOPPED") != "true" {
		return errors.New("AI_LEGACY_WRITERS_STOPPED=true required after stopping old AI/search consumers")
	}
	if err := m.Control.WithContext(ctx).Exec("UPDATE queue_budgets SET paused_until='infinity' WHERE queue IN ('ai','search')").Error; err != nil {
		return err
	}
	var active int64
	if err := m.Control.WithContext(ctx).Table("tasks").Where("kind IN ('ai.index','ai.reprocess','search.project') AND state IN ('running','dispatched') AND lease_until>clock_timestamp()").Count(&active).Error; err != nil {
		return err
	}
	if active > 0 {
		return errors.New("legacy AI/search execution leases remain live")
	}
	return nil
}
func (m *Migrator) Inspect(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	for _, table := range []string{"ai_documents", "ai_document_versions", "ai_chunks"} {
		var count int64
		var exists bool
		if err := m.Read.WithContext(ctx).Raw("SELECT to_regclass(?) IS NOT NULL", table).Scan(&exists).Error; err != nil {
			return nil, err
		}
		if exists {
			if err := m.Read.WithContext(ctx).Table(table).Count(&count).Error; err != nil {
				return nil, err
			}
		}
		out[table] = count
	}
	for _, state := range []string{"ready", "retry_wait", "running", "computed", "succeeded", "needs_attention", "quarantined"} {
		var n int64
		if err := m.Control.WithContext(ctx).Table("tasks").Where("kind IN ('ai.index','ai.reprocess','search.project') AND state=?", state).Count(&n).Error; err != nil {
			return nil, err
		}
		out["legacy_tasks_"+state] = n
	}
	return out, nil
}

func (m *Migrator) Manifest(ctx context.Context, run uuid.UUID) (map[string]any, error) {
	var record struct {
		ID       uuid.UUID       `json:"id"`
		Stage    string          `json:"stage"`
		Cursor   string          `json:"cursor"`
		Manifest json.RawMessage `json:"manifest"`
	}
	if err := m.AI.WithContext(ctx).Table("ai_migration_runs").Select("id,stage,cursor,manifest").Where("id=?", run).Take(&record).Error; err != nil {
		return nil, err
	}
	var items []struct {
		ObjectID          uuid.UUID       `json:"object_id"`
		ContentGeneration int64           `json:"content_generation"`
		Conclusion        string          `json:"conclusion"`
		Details           json.RawMessage `json:"details"`
	}
	limit := min(max(m.Batch, 1), 1000)
	if err := m.AI.WithContext(ctx).Table("ai_migration_items").Where("run_id=?", run).Order("object_id").Limit(limit).Find(&items).Error; err != nil {
		return nil, err
	}
	var tasks []legacyTask
	if err := m.AI.WithContext(ctx).Where("run_id=?", run).Order("legacy_task_id").Limit(limit).Find(&tasks).Error; err != nil {
		return nil, err
	}
	return map[string]any{"run": record, "items": items, "legacy_tasks": tasks, "limit": limit}, nil
}
func (m *Migrator) Plan(ctx context.Context, run uuid.UUID) (MigrationReport, error) {
	report := MigrationReport{RunID: run, Stage: "planning"}
	if err := m.legacyStopped(ctx); err != nil {
		return report, err
	}
	if run == uuid.Nil {
		return report, errors.New("explicit run ID required")
	}
	batch := m.Batch
	if batch < 1 || batch > 1000 {
		batch = 100
	}
	// Keep a durable bounded manifest. The relay uses xmin/(xid,seq); this tool
	// does not substitute MAX(seq) as a committed-event completeness proof.
	var identity string
	if err := m.Read.WithContext(ctx).Raw("SELECT current_database()||':'||COALESCE(inet_server_addr()::text,'local')").Scan(&identity).Error; err != nil {
		return report, err
	}
	boundary := map[string]any{"relay": "business-primary", "profile": m.Profile.Digest(), "policy": "reparse-canonical-source", "created_at": time.Now().UTC()}
	b, _ := json.Marshal(boundary)
	if err := m.AI.WithContext(ctx).Exec(`INSERT INTO ai_migration_runs(id,source_identity,source_revision,boundary,scope_hash,stage) VALUES(?,?,?,?::jsonb,?,'planning') ON CONFLICT DO NOTHING`, run, identity, m.Revision, string(b), aiprotocol.HashJSON([]any{identity, m.Profile.Digest()})).Error; err != nil {
		return report, err
	}
	var record struct{ Cursor, SourceIdentity, ScopeHash string }
	if err := m.AI.WithContext(ctx).Table("ai_migration_runs").Where("id=?", run).Take(&record).Error; err != nil {
		return report, err
	}
	if record.SourceIdentity != identity || record.ScopeHash != aiprotocol.HashJSON([]any{identity, m.Profile.Digest()}) {
		return report, errors.New("migration manifest identity mismatch")
	}
	cursor := record.Cursor
	if cursor == "" {
		cursor = uuid.Nil.String()
	}
	var rows []struct {
		ID                       uuid.UUID
		ContentGeneration        int64
		SHA256, Status, MimeType string
	}
	if err := m.Read.WithContext(ctx).Table("file_objects").Select("id,content_generation,sha256,status,mime_type").Where("id>?::uuid", cursor).Order("id").Limit(batch).Find(&rows).Error; err != nil {
		return report, err
	}
	for n := range rows {
		mediaType, err := outbox.AIResourceMediaType(m.Read.WithContext(ctx), rows[n].ID, rows[n].MimeType)
		if err != nil {
			return report, err
		}
		rows[n].MimeType = mediaType
	}
	err := m.AI.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, row := range rows {
			conclusion := "rebuild"
			if row.Status != "ready" {
				conclusion = "skip_deleted"
			} else if !supportedText(row.MimeType) {
				conclusion = "skip_unsupported"
			}
			details := map[string]any{"reason": "legacy chunks have no trustworthy parent/source ranges; vectors require independently proven model identity", "status": row.Status}
			d, _ := json.Marshal(details)
			if err := tx.Exec(`INSERT INTO ai_migration_items(run_id,object_id,content_generation,source_hash,conclusion,details) VALUES(?,?,?,?,?,?::jsonb) ON CONFLICT DO NOTHING`, run, row.ID, row.ContentGeneration, row.SHA256, conclusion, string(d)).Error; err != nil {
				return err
			}
		}
		more := len(rows) == batch
		stage := "planned"
		if more {
			stage = "planning"
		}
		last := cursor
		if len(rows) > 0 {
			last = rows[len(rows)-1].ID.String()
		}
		report.HasMore = more
		report.Stage = stage
		return tx.Exec("UPDATE ai_migration_runs SET cursor=?,stage=? WHERE id=?", last, stage, run).Error
	})
	if err != nil {
		return report, err
	}
	_ = m.AI.WithContext(ctx).Table("ai_migration_items").Where("run_id=?", run).Count(&report.Items)
	return report, nil
}
func (m *Migrator) Enqueue(ctx context.Context, run uuid.UUID) (MigrationReport, error) {
	report := MigrationReport{RunID: run, Stage: "enqueuing"}
	if err := m.legacyStopped(ctx); err != nil {
		return report, err
	}
	var handed bool
	if err := m.AI.WithContext(ctx).Raw("SELECT COALESCE((manifest->>'handoff_complete')::boolean,false) FROM ai_migration_runs WHERE id=?", run).Scan(&handed).Error; err != nil {
		return report, err
	}
	if !handed {
		return report, errors.New("complete bounded handoff before enqueue-intents")
	}
	if m.IntentWriter == nil {
		return report, errors.New("explicit narrow business intent migration writer DSN required")
	}
	var items []struct {
		ObjectID          uuid.UUID
		ContentGeneration int64
		SourceHash        string
	}
	if err := m.AI.WithContext(ctx).Table("ai_migration_items").Where("run_id=? AND conclusion='rebuild'", run).Order("object_id").Limit(max(1, m.Batch)).Find(&items).Error; err != nil {
		return report, err
	}
	for _, item := range items {
		var old []legacyTask
		if err := m.AI.WithContext(ctx).Table("ai_migration_legacy_tasks").Where("run_id=? AND object_id=? AND content_generation=?", run, item.ObjectID, item.ContentGeneration).Find(&old).Error; err != nil {
			return report, err
		}
		observers := []aiprotocol.JobObserver{}
		for _, v := range old {
			if v.JobID != nil && v.OwnerID != nil {
				observers = append(observers, aiprotocol.JobObserver{JobID: *v.JobID, OwnerID: *v.OwnerID})
			}
		}
		var inherited *aiprotocol.LegacyBudget
		stopped := false
		for _, v := range old {
			if (asynccontrol.Terminal(v.State) && v.State != asynccontrol.Succeeded) || v.ResultID != nil && v.State != asynccontrol.Succeeded || v.Attempts >= v.MaxAttempts || v.Dispatches >= v.MaxDispatches || !v.Deadline.After(time.Now()) {
				stopped = true
			}
			if inherited == nil {
				inherited = &aiprotocol.LegacyBudget{TaskID: v.LegacyTaskID, MaxAttempts: v.MaxAttempts, MaxDispatches: v.MaxDispatches, MaxRecoveries: v.MaxRecoveries, Deadline: v.Deadline}
			}
			inherited.Attempts += v.Attempts
			inherited.Dispatches += v.Dispatches
			inherited.Recoveries += v.Recoveries
			inherited.MaxAttempts = min(inherited.MaxAttempts, v.MaxAttempts)
			inherited.MaxDispatches = min(inherited.MaxDispatches, v.MaxDispatches)
			inherited.MaxRecoveries = min(inherited.MaxRecoveries, v.MaxRecoveries)
			if v.Deadline.Before(inherited.Deadline) {
				inherited.Deadline = v.Deadline
			}
		}
		if inherited != nil && (inherited.Attempts >= inherited.MaxAttempts || inherited.Dispatches >= inherited.MaxDispatches || inherited.Recoveries >= inherited.MaxRecoveries) {
			stopped = true
		}
		if stopped {
			store := asynccontrol.New(m.Control)
			Register(store.Registry)
			for _, observer := range observers {
				finish := aiprotocol.JobFinish{JobID: observer.JobID, OwnerID: observer.OwnerID, ObjectID: item.ObjectID, Status: "needs_attention", Outcomes: map[string]string{"legacy": "stopped_preserved"}}
				if _, err := store.Ensure(ctx, asynccontrol.Spec{Kind: "ai.job.finish", SemanticKey: "ai.v2.legacy.job.finish:" + observer.JobID.String(), OwnerKey: "owner:" + observer.OwnerID.String(), ResourceKey: "object:" + item.ObjectID.String(), ShardKey: "object:" + item.ObjectID.String(), Input: json.RawMessage(jsonValue(finish))}, "ai-migrate:"+run.String()); err != nil {
					return report, err
				}
			}
			if err := m.AI.WithContext(ctx).Exec("UPDATE ai_migration_items SET conclusion='stopped_preserved',details=details||?::jsonb,updated_at=clock_timestamp() WHERE run_id=? AND object_id=? AND content_generation=?", string(jsonValue(map[string]any{"reason": "legacy stop/result requires explicit review or admin redo; no automatic budget reset"})), run, item.ObjectID, item.ContentGeneration).Error; err != nil {
				return report, err
			}
			report.Quarantined++
			continue
		}

		err := m.IntentWriter.WithContext(outbox.WithAIProfile(ctx, m.Profile.ID, m.Profile.Revision)).Transaction(func(tx *gorm.DB) error {
			var o struct {
				Status, SHA256, MimeType              string
				ContentGeneration, Version, SizeBytes int64
			}
			if err := tx.Table("file_objects").Where("id=?", item.ObjectID).Take(&o).Error; err != nil {
				return err
			}
			if o.ContentGeneration != item.ContentGeneration || o.SHA256 != item.SourceHash || o.Status != "ready" {
				return errors.New("migration resource changed")
			}
			mediaType, err := outbox.AIResourceMediaType(tx, item.ObjectID, o.MimeType)
			if err != nil {
				return err
			}
			o.MimeType = mediaType
			// A deterministic request outbox identity makes crash/retry safe. Existing
			// automatic intents win; no previously stopped semantic task is reopened.
			var exists int64
			if err := tx.Table("ai_resource_intents").Where("object_id=? AND content_generation=?", item.ObjectID, item.ContentGeneration).Count(&exists).Error; err != nil {
				return err
			}
			if exists > 0 {
				if len(observers) > 0 {
					var live struct{ LastRequestID uuid.UUID }
					if err := tx.Table("ai_resource_intents").Where("object_id=? AND content_generation=?", item.ObjectID, item.ContentGeneration).Take(&live).Error; err != nil {
						return err
					}
					return m.attachJobs(ctx, live.LastRequestID, observers)
				}
				return nil
			}
			if inherited == nil {
				return outbox.ReadyAIIntent(tx.Statement.Context, tx, item.ObjectID)
			}
			requestID := uuid.NewSHA1(run, []byte(item.ObjectID.String()+":"+stringInt(item.ContentGeneration)))
			if err := tx.Exec("INSERT INTO ai_resource_intents(object_id,content_generation,last_request_id,profile_id,profile_revision) VALUES(?,?,?,?,?)", item.ObjectID, item.ContentGeneration, requestID, m.Profile.ID, m.Profile.Revision).Error; err != nil {
				return err
			}
			targets, err := aiprotocol.Targets(nil, m.Profile)
			if err != nil {
				return err
			}
			intent := aiprotocol.Intent{Version: 2, RequestedAt: time.Now().Unix(), RequestID: requestID, ObjectID: item.ObjectID, ContentGeneration: item.ContentGeneration, ContentHash: item.SourceHash, Size: o.SizeBytes, MediaType: o.MimeType, LifecycleRevision: o.Version, Seqs: aiprotocol.Seqs{Source: 1, Text: 1, Vector: 1, Summary: 1}, ProfileID: m.Profile.ID, ProfileRevision: m.Profile.Revision, Targets: targets, Legacy: inherited, Observers: observers}
			return outbox.AddTyped(tx.Statement.Context, tx, eventcontract.EnvelopeInput{EventType: aiprotocol.IntentEvent, AggregateID: item.ObjectID, AggregateVersion: o.Version, Producer: "ai-migrate", Payload: intent})
		})
		if err != nil {
			return report, err
		}
		if err := m.AI.WithContext(ctx).Exec("UPDATE ai_migration_items SET conclusion='queued',updated_at=clock_timestamp() WHERE run_id=? AND object_id=? AND content_generation=? AND conclusion='rebuild'", run, item.ObjectID, item.ContentGeneration).Error; err != nil {
			return report, err
		}
		report.Queued++
	}
	var pending int64
	_ = m.AI.WithContext(ctx).Table("ai_migration_items").Where("run_id=? AND conclusion='rebuild'", run).Count(&pending)
	report.HasMore = pending > 0
	return report, nil
}

// A durable request observer also covers descendants created after this tool
// exits. The same request lock is taken by online observe/finish callbacks.
func (m *Migrator) attachJobs(ctx context.Context, request uuid.UUID, observers []aiprotocol.JobObserver) error {
	targets, err := aiprotocol.Targets(nil, m.Profile)
	if err != nil {
		return err
	}
	store := asynccontrol.New(m.Control)
	Register(store.Registry)
	planner := &Planner{}
	return m.Control.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := dblock.Transaction(tx, "ai-job-observers:"+request.String()); err != nil {
			return err
		}
		for _, o := range observers {
			if err := tx.Exec("INSERT INTO ai_job_observers(request_id,job_id,owner_id) VALUES(?,?,?) ON CONFLICT DO NOTHING", request, o.JobID, o.OwnerID).Error; err != nil {
				return err
			}
			if err := tx.Exec("INSERT INTO ai_job_aggregates(job_id,requested_targets) VALUES(?,?::jsonb) ON CONFLICT DO NOTHING", o.JobID, string(jsonValue(targets))).Error; err != nil {
				return err
			}
		}
		var tasks []asynccontrol.Task
		if err := tx.Where("input->'intent'->>'request_id'=?", request.String()).Find(&tasks).Error; err != nil {
			return err
		}
		for _, task := range tasks {
			if err := planner.Observe(tx, task); err != nil {
				return err
			}
		}
		for _, task := range tasks {
			if asynccontrol.Terminal(task.State) {
				if err := planner.Finish(tx, store, task, task.State); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (m *Migrator) MarkMessagesTransferred(ctx context.Context, run uuid.UUID) error {
	return m.AI.WithContext(ctx).Exec("UPDATE ai_migration_runs SET manifest=manifest||'{\"messages_transferred\":true}'::jsonb WHERE id=?", run).Error
}
func (m *Migrator) Verify(ctx context.Context, run uuid.UUID) (MigrationReport, error) {
	r := MigrationReport{RunID: run, Stage: "verifying"}
	var items []struct {
		ObjectID          uuid.UUID
		ContentGeneration int64
	}
	if err := m.AI.WithContext(ctx).Table("ai_migration_items").Where("run_id=? AND conclusion='queued'", run).Order("object_id").Limit(max(1, m.Batch)).Find(&items).Error; err != nil {
		return r, err
	}
	for _, item := range items {
		var o Object
		if err := m.AI.WithContext(ctx).Where("object_id=? AND content_generation=?", item.ObjectID, item.ContentGeneration).Find(&o).Error; err != nil {
			return r, err
		}
		if o.ActiveContentVersionID == nil {
			r.HasMore = true
			continue
		}
		var n int64
		if err := m.AI.WithContext(ctx).Table("ai_child_chunks").Where("content_version_id=?", o.ActiveContentVersionID).Count(&n).Error; err != nil {
			return r, err
		}
		if n == 0 {
			return r, errors.New("published text has no children")
		}
		if err := m.AI.WithContext(ctx).Exec("UPDATE ai_migration_items SET conclusion='verified',target_id=?,updated_at=clock_timestamp() WHERE run_id=? AND object_id=? AND content_generation=?", *o.ActiveContentVersionID, run, item.ObjectID, item.ContentGeneration).Error; err != nil {
			return r, err
		}
		r.Verified++
	}
	var remaining int64
	if err := m.AI.WithContext(ctx).Table("ai_migration_items").Where("run_id=? AND conclusion='queued'", run).Count(&remaining).Error; err != nil {
		return r, err
	}
	r.HasMore = remaining > 0
	return r, nil
}

// Handoff freezes stopped/completed legacy tasks as historical barriers. Pending
// work retains all consumed counters in its audited successor. Computed results
// are quarantined with their body intact for receipt review, never recomputed.
type legacyTask struct {
	JobID, OwnerID                                                              *uuid.UUID
	RunID, LegacyTaskID, ObjectID                                               uuid.UUID
	ContentGeneration                                                           int64
	State, InputDigest                                                          string
	Attempts, Dispatches, Recoveries, MaxAttempts, MaxDispatches, MaxRecoveries int
	Deadline                                                                    time.Time
	ResultID                                                                    *uuid.UUID
}

func (legacyTask) TableName() string { return "ai_migration_legacy_tasks" }
func (m *Migrator) Handoff(ctx context.Context, run uuid.UUID) (MigrationReport, error) {
	r := MigrationReport{RunID: run, Stage: "handoff"}
	if err := m.legacyStopped(ctx); err != nil {
		return r, err
	}
	var cursor string
	var count int64
	if err := m.AI.WithContext(ctx).Table("ai_migration_runs").Where("id=?", run).Count(&count).Error; err != nil {
		return r, err
	}
	if count != 1 {
		return r, errors.New("rebuild-plan must create the run first")
	}
	if err := m.AI.WithContext(ctx).Raw("SELECT COALESCE(manifest->>'handoff_cursor','') FROM ai_migration_runs WHERE id=?", run).Scan(&cursor).Error; err != nil {
		return r, err
	}
	if cursor == "" {
		cursor = uuid.Nil.String()
	}
	var rows []asynccontrol.Task
	if err := m.Control.WithContext(ctx).Where("kind IN ('ai.index','ai.reprocess','search.project') AND id>?::uuid", cursor).Order("id").Limit(max(1, m.Batch)).Find(&rows).Error; err != nil {
		return r, err
	}
	for _, old := range rows {
		var in struct {
			EventID           uuid.UUID `json:"event_id"`
			OwnerID           uuid.UUID `json:"owner_id"`
			ObjectID          uuid.UUID `json:"object_id"`
			ContentGeneration int64     `json:"content_generation"`
		}
		if json.Unmarshal(old.Input, &in) != nil {
			return r, errors.New("legacy input requires manual review")
		}
		if in.ObjectID == uuid.Nil {
			parts := strings.Split(old.ResourceKey, ":")
			if len(parts) > 1 {
				in.ObjectID, _ = uuid.Parse(parts[1])
			}
		}
		if in.ObjectID == uuid.Nil {
			return r, errors.New("legacy resource cannot be mapped; task retained for review")
		}
		if in.ContentGeneration < 1 {
			if err := m.Read.WithContext(ctx).Table("file_objects").Select("content_generation").Where("id=?", in.ObjectID).Scan(&in.ContentGeneration).Error; err != nil {
				return r, err
			}
		}
		row := legacyTask{RunID: run, LegacyTaskID: old.ID, ObjectID: in.ObjectID, ContentGeneration: in.ContentGeneration, State: old.State, InputDigest: old.InputDigest, Attempts: old.Attempts, Dispatches: old.Dispatches, Recoveries: old.Recoveries, MaxAttempts: old.MaxAttempts, MaxDispatches: old.MaxDispatches, MaxRecoveries: old.MaxRecoveries, Deadline: old.Deadline, ResultID: old.ResultID}
		if old.Kind == "ai.reprocess" && in.EventID != uuid.Nil {
			row.JobID = &in.EventID
			row.OwnerID = &in.OwnerID
		}
		if err := m.AI.WithContext(ctx).Exec(`INSERT INTO ai_migration_legacy_tasks(run_id,legacy_task_id,object_id,content_generation,state,input_digest,attempts,dispatches,recoveries,max_attempts,max_dispatches,max_recoveries,deadline,result_id,job_id,owner_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, row.RunID, row.LegacyTaskID, row.ObjectID, row.ContentGeneration, row.State, row.InputDigest, row.Attempts, row.Dispatches, row.Recoveries, row.MaxAttempts, row.MaxDispatches, row.MaxRecoveries, row.Deadline, row.ResultID, row.JobID, row.OwnerID).Error; err != nil {
			return r, err
		}
		if !asynccontrol.Terminal(old.State) {
			reason := "AI split handoff retained original budget and durable result"
			if err := m.Control.WithContext(ctx).Exec("INSERT INTO quarantines(identity,task_id,reason,details) VALUES(?,?,?,?::jsonb) ON CONFLICT DO NOTHING", "ai-migration:"+old.ID.String(), old.ID, reason, string(jsonValue(row))).Error; err != nil {
				return r, err
			}
			if err := m.Control.WithContext(ctx).Model(&asynccontrol.Task{}).Where("id=? AND state=?", old.ID, old.State).Updates(map[string]any{"state": asynccontrol.Quarantined, "token": nil, "lease_until": nil, "last_error": reason, "stopped_at": gorm.Expr("clock_timestamp()")}).Error; err != nil {
				return r, err
			}
			r.Quarantined++
		}
	}
	r.HasMore = len(rows) == max(1, m.Batch)
	if len(rows) > 0 {
		cursor = rows[len(rows)-1].ID.String()
	}
	if !r.HasMore {
		more, err := m.handoffUnmappedJobs(ctx, run)
		if err != nil {
			return r, err
		}
		r.HasMore = more
	}
	return r, m.AI.WithContext(ctx).Exec("UPDATE ai_migration_runs SET manifest=manifest||?::jsonb WHERE id=?", string(jsonValue(map[string]any{"handoff_cursor": cursor, "handoff_complete": !r.HasMore})), run).Error
}

func (m *Migrator) handoffUnmappedJobs(ctx context.Context, run uuid.UUID) (bool, error) {
	var cursor string
	if err := m.AI.WithContext(ctx).Raw("SELECT COALESCE(manifest->>'job_cursor','') FROM ai_migration_runs WHERE id=?", run).Scan(&cursor).Error; err != nil {
		return false, err
	}
	if cursor == "" {
		cursor = uuid.Nil.String()
	}
	var jobs []struct{ ID, OwnerID, ResourceID uuid.UUID }
	if err := m.Read.WithContext(ctx).Table("async_tasks").Select("id,owner_id,resource_id").Where("task_type='ai.reprocess' AND status IN ('pending','running','processing') AND id>?::uuid", cursor).Order("id").Limit(max(1, m.Batch)).Find(&jobs).Error; err != nil {
		return false, err
	}
	store := asynccontrol.New(m.Control)
	Register(store.Registry)
	for _, job := range jobs {
		var mapped int64
		if err := m.AI.WithContext(ctx).Table("ai_migration_legacy_tasks").Where("run_id=? AND job_id=?", run, job.ID).Count(&mapped).Error; err != nil {
			return false, err
		}
		if mapped == 0 {
			// No execution identity/budget can be proven. Record a stopped user
			// conclusion instead of silently granting a fresh model-call budget.
			finish := aiprotocol.JobFinish{JobID: job.ID, OwnerID: job.OwnerID, Status: "needs_attention", Outcomes: map[string]string{"legacy": "execution_identity_unproven"}}
			if _, err := store.Ensure(ctx, asynccontrol.Spec{Kind: "ai.job.finish", SemanticKey: "ai.v2.legacy.job.finish:" + job.ID.String(), OwnerKey: "owner:" + job.OwnerID.String(), ResourceKey: "job:" + job.ID.String(), ShardKey: "owner:" + job.OwnerID.String(), Input: json.RawMessage(jsonValue(finish))}, "ai-migrate:"+run.String()); err != nil {
				return false, err
			}
		}
		cursor = job.ID.String()
	}
	if err := m.AI.WithContext(ctx).Exec("UPDATE ai_migration_runs SET manifest=manifest||?::jsonb WHERE id=?", string(jsonValue(map[string]any{"job_cursor": cursor})), run).Error; err != nil {
		return false, err
	}
	return len(jobs) == max(1, m.Batch), nil
}
func (m *Migrator) CutoverCheck(ctx context.Context, run uuid.UUID) (MigrationReport, error) {
	r := MigrationReport{RunID: run, Stage: "cutover_check"}
	if err := m.legacyStopped(ctx); err != nil {
		return r, err
	}
	var runs int64
	if err := m.AI.WithContext(ctx).Table("ai_migration_runs").Where("id=?", run).Count(&runs).Error; err != nil {
		return r, err
	}
	if runs != 1 {
		return r, errors.New("migration run not found")
	}
	var complete bool
	if err := m.AI.WithContext(ctx).Raw("SELECT stage IN ('planned','cutover_ready') AND COALESCE((manifest->>'handoff_complete')::boolean,false) AND COALESCE((manifest->>'messages_transferred')::boolean,false) FROM ai_migration_runs WHERE id=?", run).Scan(&complete).Error; err != nil {
		return r, err
	}
	if !complete {
		return r, errors.New("planning, task handoff and message transfer must complete before cutover")
	}
	var missing int64
	if err := m.AI.WithContext(ctx).Table("ai_migration_items").Where("run_id=? AND conclusion NOT IN ('verified','skip_deleted','skip_unsupported','stopped_preserved')", run).Count(&missing).Error; err != nil {
		return r, err
	}
	if missing > 0 {
		return r, fmt.Errorf("%d migration items are unfinished", missing)
	}
	var results int64
	if err := m.Control.WithContext(ctx).Table("tasks").Where("kind IN ('ai.index','ai.reprocess','search.project') AND result_id IS NOT NULL AND state NOT IN ('succeeded','superseded','rejected','failed')").Count(&results).Error; err != nil {
		return r, err
	}
	if results > 0 {
		return r, errors.New("legacy durable results require receipt review before archive")
	}
	var live int64
	if err := m.Control.WithContext(ctx).Table("tasks").Where("kind IN ('ai.index','ai.reprocess','search.project') AND state NOT IN ('succeeded','superseded','rejected','failed','quarantined','needs_attention')").Count(&live).Error; err != nil {
		return r, err
	}
	if live > 0 {
		return r, errors.New("legacy tasks remain executable; repeat handoff")
	}
	var jobs int64
	if err := m.Read.WithContext(ctx).Table("async_tasks").Where("task_type='ai.reprocess' AND status IN ('pending','running','processing')").Count(&jobs).Error; err != nil {
		return r, err
	}
	if jobs > 0 {
		return r, fmt.Errorf("%d historical user jobs await their core conclusion", jobs)
	}
	if err := m.AI.WithContext(ctx).Exec("UPDATE ai_migration_runs SET stage='cutover_ready',completed_at=clock_timestamp() WHERE id=?", run).Error; err != nil {
		return r, err
	}
	if m.IntentWriter == nil {
		return r, errors.New("explicit intent/cutover migration writer required")
	}
	if err := m.IntentWriter.WithContext(ctx).Exec("INSERT INTO ai_split_cutovers(run_id,status,manifest_hash) VALUES(?,'cutover_ready',?) ON CONFLICT(run_id) DO NOTHING", run, aiprotocol.HashJSON([]any{run, m.Profile.Digest()})).Error; err != nil {
		return r, err
	}
	r.Stage = "cutover_ready"
	return r, nil
}
