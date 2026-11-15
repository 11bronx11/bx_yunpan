package asyncjobs

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
	"github.com/11bronx11/bx_yunpan/backend/internal/upload"
)

func CheckImport(ctx context.Context, business *gorm.DB, s *asynccontrol.Store) error {
	var pending string
	if err := s.DB.WithContext(ctx).Table("control_settings").Select("value").Where("key='legacy_cutover_pending'").Scan(&pending).Error; err != nil {
		return err
	}
	if pending == "true" {
		return errors.New("legacy broker cutover is incomplete; rerun async-import before starting control")
	}
	var marker string
	q := s.DB.WithContext(ctx).Table("control_settings").Select("value").Where("key='legacy_import_complete'").Scan(&marker)
	if q.Error != nil {
		return q.Error
	}
	if marker == "true" {
		return nil
	}
	var count int64
	if err := business.WithContext(ctx).Table("outbox_events").Count(&count).Error; err != nil {
		return err
	}
	if count != 0 {
		return errors.New("run async-import after stopping all old API/relay/worker consumers before starting async-control")
	}
	return s.DB.WithContext(ctx).Exec("INSERT INTO control_settings(key,value) VALUES('legacy_import_complete','true') ON CONFLICT DO NOTHING").Error
}

// ImportLegacy replays retained events with their semantic identities and
// conservative consumed budgets. It never writes business/legacy tables.
// The deployment cutover fences old writers before this command is run.
func ImportLegacy(ctx context.Context, business *gorm.DB, s *asynccontrol.Store, p *Planner, holder string) error {
	var marker string
	if err := s.DB.WithContext(ctx).Table("control_settings").Select("value").Where("key='legacy_import_complete'").Scan(&marker).Error; err != nil {
		return err
	}
	if marker == "true" {
		return nil
	}
	token, ok, err := s.AcquireModule(ctx, "relay", holder)
	if err != nil {
		return err
	}
	if !ok {
		return asynccontrol.ErrLost
	}
	defer s.DB.WithContext(context.WithoutCancel(ctx)).Exec("UPDATE module_leases SET lease_until=clock_timestamp() WHERE name='relay' AND token=?", token)
	relay := asynccontrol.Relay{Control: s, Business: business, Source: "business-primary", Rows: 200, Bytes: 1 << 20, Plan: func(ctx context.Context, e asynccontrol.Event) ([]asynccontrol.Spec, error) {
		specs, err := p.Plan(ctx, e)
		if err != nil {
			return nil, err
		}
		for i := range specs {
			definition := s.Registry[specs[i].Kind]
			specs[i].Deadline = e.CreatedAt.Add(definition.Lifetime)
			if specs[i].NotBefore.After(e.CreatedAt) {
				specs[i].Deadline = specs[i].NotBefore.Add(definition.Lifetime)
			}
			// MQ retry counters are cumulative for each old task identity. Multiple
			// recovery-event identities are conservatively combined before deduplication.
			var in asyncprotocol.Input
			_ = json.Unmarshal(specs[i].Input, &in)
			var budget struct{ Attempts, Dispatches int }
			ids, err := legacyTaskIDs(ctx, business, e, specs[i], in)
			if err != nil {
				return nil, err
			}
			if len(ids) > 0 {
				if err := business.WithContext(ctx).Raw(`SELECT COALESCE(sum(retry_count+1),0)::int AS attempts FROM task_executions WHERE task_id IN ?`, ids).Scan(&budget).Error; err != nil {
					return nil, err
				}
				var sends int
				if err := business.WithContext(ctx).Raw(`SELECT COALESCE(sum(attempt),0)::int FROM outbox_deliveries WHERE event_id::text||':'||destination IN ?`, ids).Scan(&sends).Error; err != nil {
					return nil, err
				}
				budget.Dispatches = sends
				var limit int
				if err := business.WithContext(ctx).Raw(`SELECT COALESCE(min(max_retry+1),0)::int FROM task_executions WHERE task_id IN ?`, ids).Scan(&limit).Error; err != nil {
					return nil, err
				}
				specs[i].MaxAttempts = limit
			}
			specs[i].Attempts = budget.Attempts
			specs[i].Dispatches = budget.Dispatches
			if in.OperationID != uuid.Nil {
				var op storageops.Operation
				if err := business.WithContext(ctx).First(&op, "id=?", in.OperationID).Error; err != nil {
					return nil, err
				}
				specs[i].Attempts = max(specs[i].Attempts, op.Attempt)
				if specs[i].MaxAttempts == 0 {
					specs[i].MaxAttempts = storageops.MaxAttempts
				} else {
					specs[i].MaxAttempts = min(specs[i].MaxAttempts, storageops.MaxAttempts)
				}
				start := op.CreatedAt
				if op.AvailableAt.After(start) {
					start = op.AvailableAt
				}
				specs[i].Deadline = start.Add(definition.Lifetime)
			}
		}
		return specs, nil
	}}
	relay.Accept = func(tx *gorm.DB, task asynccontrol.Task, e asynccontrol.Event, spec asynccontrol.Spec) error {
		if err := s.ImportBudget(tx, task, spec); err != nil {
			return err
		}
		var in asyncprotocol.Input
		if err := json.Unmarshal(spec.Input, &in); err != nil {
			return err
		}
		ids, err := legacyTaskIDs(ctx, business, e, spec, in)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := tx.Exec("INSERT INTO task_sources(task_id,source_key) VALUES(?,?) ON CONFLICT DO NOTHING", task.ID, "legacy-task:"+id).Error; err != nil {
				return err
			}
		}
		var session upload.Session
		if in.SessionID != uuid.Nil {
			if err := business.WithContext(ctx).First(&session, "id=?", in.SessionID).Error; err != nil {
				return err
			}
		}
		if in.OperationID != uuid.Nil {
			var op storageops.Operation
			if err := business.WithContext(ctx).First(&op, "id=?", in.OperationID).Error; err != nil {
				return err
			}
			if op.State == storageops.StateDead {
				return s.ImportState(tx, task, "exhausted", nil, "legacy storage budget exhausted")
			}
			if op.State == storageops.StateSucceeded {
				switch op.OperationType {
				case storageops.MultipartCreate:
					if session.Status == upload.StatusInitializing && op.StorageUploadID != "" {
						body, _ := json.Marshal(asyncprotocol.Output{UploadID: op.StorageUploadID})
						return s.ImportState(tx, task, "computed", body, "")
					}
				case storageops.MultipartComplete:
					if session.Status == upload.StatusCompleting {
						return s.ImportState(tx, task, "computed", []byte(`{}`), "")
					}
				case storageops.CanonicalEnsure:
					if session.Status == upload.StatusVerifying {
						body, _ := json.Marshal(asyncprotocol.Output{Bucket: op.TargetBucket, Key: op.TargetKey})
						return s.ImportState(tx, task, "needs_attention", body, "legacy canonical output retained; verify ownership before explicit recovery")
					}
				}
				return s.ImportState(tx, task, "succeeded", nil, "")
			}
		}
		if in.SessionID != uuid.Nil && (session.Status == upload.StatusCompleted || session.Status == upload.StatusFailed || session.Status == upload.StatusAborted || session.Status == upload.StatusExpired) {
			return s.ImportState(tx, task, "superseded", nil, "legacy business session already terminal")
		}
		if task.Kind == "ai.reprocess" {
			var job struct{ Status string }
			q := business.WithContext(ctx).Table("async_tasks").Select("status").Where("id=? AND owner_id=? AND resource_id=?", in.EventID, in.OwnerID, in.ObjectID).Scan(&job)
			if q.Error != nil {
				return q.Error
			}
			if q.RowsAffected != 1 {
				return s.ImportState(tx, task, "needs_attention", nil, "legacy user job missing")
			}
			if job.Status == "succeeded" || job.Status == "failed" {
				return s.ImportState(tx, task, job.Status, nil, "legacy user job already terminal")
			}
		}
		if len(ids) > 0 {
			var history struct{ Succeeded, Stopped int }
			if err := business.WithContext(ctx).Raw(`SELECT count(*) FILTER(WHERE status='succeeded')::int AS succeeded,count(*) FILTER(WHERE status<>'succeeded')::int AS stopped FROM task_results WHERE task_id IN ?`, ids).Scan(&history).Error; err != nil {
				return err
			}
			if history.Succeeded > 0 {
				// Derived completion and GC/thumbnail metadata are already authoritative.
				// Upload effects must be demonstrable from their business session/intent.
				if task.Kind == "ai.index" || task.Kind == "search.project" || task.Kind == "thumbnail" || task.Kind == "object.authorize_delete" {
					return s.ImportState(tx, task, "succeeded", nil, "")
				}
				return s.ImportState(tx, task, "needs_attention", nil, "legacy execution succeeded without a demonstrable business receipt")
			}
			if history.Stopped > 0 {
				return s.ImportState(tx, task, "exhausted", nil, "legacy terminal execution failure")
			}
		}
		return nil
	}
	for ctx.Err() == nil {
		if err := s.RenewModule(ctx, "relay", token); err != nil {
			return err
		}
		n, err := relay.Step(ctx, token)
		if err != nil {
			return err
		}
		if n == 0 {
			var unread bool
			var position asynccontrol.Position
			if err := s.DB.WithContext(ctx).Raw("SELECT last_xid::text AS last_x_id,last_seq FROM relay_positions WHERE source='business-primary'").Scan(&position).Error; err != nil {
				return err
			}
			if err := business.WithContext(ctx).Raw("SELECT EXISTS(SELECT 1 FROM outbox_events WHERE (event_xid,seq)>(?::xid8,?) LIMIT 1)", position.LastXID, position.LastSeq).Scan(&unread).Error; err != nil {
				return err
			}
			if unread {
				return errors.New("legacy import safe watermark is blocked by an open transaction; retry after it ends")
			}
			break
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// All storage intents must be represented, including legacy cleanup intents
	// that predate their outbox notification writer. Stable task keys merge them.
	var after uuid.UUID
	for {
		if err := s.RenewModule(ctx, "relay", token); err != nil {
			return err
		}
		var ops []storageops.Operation
		if err := business.WithContext(ctx).Where("id>?", after).Order("id").Limit(100).Find(&ops).Error; err != nil {
			return err
		}
		if len(ops) == 0 {
			break
		}
		for _, op := range ops {
			payload, _ := json.Marshal(map[string]any{"operation_id": op.ID})
			event := asynccontrol.Event{ID: uuid.NewSHA1(op.ID, []byte("legacy-intent")), AggregateID: op.ID, EventType: "storage.operation_recover.v1", Payload: payload, CreatedAt: op.CreatedAt}
			specs, err := relay.Plan(ctx, event)
			if err != nil {
				if errors.Is(err, asynccontrol.ErrInvalid) {
					if qerr := s.DB.WithContext(ctx).Exec(`INSERT INTO quarantines(identity,reason,details) VALUES(?,?,?::jsonb) ON CONFLICT DO NOTHING`, "legacy-intent:"+op.ID.String(), err.Error(), string(payload)).Error; qerr != nil {
						return qerr
					}
					after = op.ID
					continue
				}
				return err
			}
			for _, spec := range specs {
				task, err := s.Ensure(ctx, spec, "legacy-intent:"+op.ID.String())
				if err != nil {
					return err
				}
				if err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return relay.Accept(tx, task, event, spec) }); err != nil {
					return err
				}
			}
			after = op.ID
		}
	}
	return s.DB.WithContext(ctx).Exec("INSERT INTO control_settings(key,value) VALUES('legacy_import_complete','true') ON CONFLICT(key) DO UPDATE SET value='true'").Error
}

// Match exact legacy destinations within a semantic resource, never merge
// sibling fanout budgets (e.g. thumbnail versus AI) or different upload stages.
func legacyTaskIDs(ctx context.Context, db *gorm.DB, e asynccontrol.Event, spec asynccontrol.Spec, in asyncprotocol.Input) ([]string, error) {
	destinations := []string{}
	switch spec.Kind {
	case "upload.verify":
		destinations = []string{"object.verify.v1@object", "object.verify_requested.v1@object"}
	case "thumbnail":
		destinations = []string{"media.thumbnail.v1@media", "media.thumbnail_requested.v1@media"}
	case "ai.index":
		destinations = []string{"ai.index.v1@ai", "ai.index_requested.v1@ai"}
	case "ai.reprocess":
		destinations = []string{"ai.reprocess.v1@ai", "ai.reprocess_requested.v1@ai"}
	case "search.project":
		destinations = []string{"search.project.v1@search"}
	case "object.authorize_delete":
		destinations = []string{"object.gc.v1@maintenance", "object.gc_requested.v1@maintenance"}
	case "upload.expire":
		return nil, nil
	default:
		destinations = []string{"upload.operation_recover.v1@upload", "storage.operation_recover.v1@maintenance"}
	}
	var ids []string
	err := db.WithContext(ctx).Raw(`SELECT DISTINCT e.id::text||':'||d.destination FROM outbox_events e JOIN outbox_deliveries d ON d.event_id=e.id WHERE d.destination IN ? AND
 (e.id=? OR (?::uuid<>?::uuid AND e.payload->>'operation_id'=?::text) OR (?::uuid<>?::uuid AND e.payload->>'upload_id'=?::text AND e.event_type=?) OR (?::uuid<>?::uuid AND e.aggregate_id=?))`, destinations, e.ID, in.OperationID, uuid.Nil, in.OperationID, in.SessionID, uuid.Nil, in.SessionID, e.EventType, in.ObjectID, uuid.Nil, in.ObjectID).Scan(&ids).Error
	return ids, err
}
