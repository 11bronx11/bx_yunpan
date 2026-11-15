package asynccontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Event struct {
	AggregateVersion *int64
	ID, AggregateID  uuid.UUID
	EventType        string
	Payload          datatypes.JSON
	TraceContext     datatypes.JSON
	XID              string `gorm:"column:event_xid"`
	Seq              int64
	CreatedAt        time.Time
}
type Position struct {
	Source, LastXID  string
	LastSeq, Version int64
	UpdatedAt        time.Time
}
type (
	Planner func(context.Context, Event) ([]Spec, error)
	Relay   struct {
		Accept      func(*gorm.DB, Task, Event, Spec) error
		Control     *Store
		Business    *gorm.DB // explicitly read-only connection to the business primary
		Source      string
		Rows, Bytes int
		Plan        Planner
	}
)

const SubscriptionPlanVersion = 1

func (r *Relay) Read(ctx context.Context, p Position) ([]Event, error) {
	var events []Event
	err := r.Business.WithContext(ctx).Raw(`SELECT id,aggregate_id,event_type,payload,trace_context,event_xid::text,seq,created_at,aggregate_version
 FROM outbox_events
 WHERE event_xid < pg_snapshot_xmin(pg_current_snapshot())
 AND (event_xid,seq) > (?::xid8,?) ORDER BY event_xid,seq LIMIT ?`, p.LastXID, p.LastSeq, r.Rows).Scan(&events).Error
	if err != nil {
		return nil, err
	}
	n := 0
	total := 0
	for _, e := range events {
		size := len(e.Payload)
		if n > 0 && total+size > r.Bytes {
			break
		}
		total += size
		n++
	}
	return events[:n], nil
}

func (r *Relay) Step(ctx context.Context, token uuid.UUID) (int, error) {
	if r.Rows <= 0 || r.Bytes <= 0 || r.Plan == nil {
		return 0, ErrInvalid
	}
	if err := r.Control.DB.WithContext(ctx).Exec("INSERT INTO relay_positions(source) VALUES(?) ON CONFLICT DO NOTHING", r.Source).Error; err != nil {
		return 0, err
	}
	var p Position
	if err := r.Control.DB.WithContext(ctx).Raw("SELECT source,last_xid::text AS last_x_id,last_seq,version,updated_at FROM relay_positions WHERE source=?", r.Source).Scan(&p).Error; err != nil {
		return 0, err
	}
	events, err := r.Read(ctx, p)
	if err != nil || len(events) == 0 {
		return 0, err
	}
	// Bound snapshots and business reads. No business locks or remote calls while
	// holding control locks; a failed plan is durably isolated instead of skipped.
	type planned struct {
		event  Event
		specs  []Spec
		reason string
		digest string
	}
	plans := make([]planned, 0, len(events))
	for _, e := range events {
		identity, _ := json.Marshal(struct {
			Type      string
			Aggregate uuid.UUID
			Payload   json.RawMessage
		}{e.EventType, e.AggregateID, json.RawMessage(e.Payload)})
		digest, _, digestErr := Digest(identity)
		var specs []Spec
		var planErr error
		if digestErr != nil {
			planErr = digestErr
		} else if len(e.Payload) > r.Bytes {
			planErr = fmt.Errorf("event exceeds maximum payload: %d", len(e.Payload))
		} else {
			specs, planErr = r.Plan(ctx, e)
		}
		if planErr != nil && digestErr == nil && len(e.Payload) <= r.Bytes && !errors.Is(planErr, ErrInvalid) {
			return 0, planErr
		}
		reason := ""
		if planErr != nil {
			reason = planErr.Error()
		}
		plans = append(plans, planned{e, specs, reason, digest})
	}
	err = r.Control.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var live int64
		if err := tx.Raw("SELECT count(*) FROM module_leases WHERE name='relay' AND token=? AND lease_until>clock_timestamp()", token).Scan(&live).Error; err != nil {
			return err
		}
		if live != 1 {
			return ErrLost
		}
		var current Position
		if err := tx.Raw("SELECT source,last_xid::text AS last_x_id,last_seq,version FROM relay_positions WHERE source=? FOR UPDATE", r.Source).Scan(&current).Error; err != nil {
			return err
		}
		if current.Version != p.Version || current.LastXID != p.LastXID || current.LastSeq != p.LastSeq {
			return ErrLost
		}
		for _, item := range plans {
			e := item.event
			var existing struct{ InputDigest string }
			q := tx.Table("event_inbox").Select("input_digest").Where("id=?", e.ID).Scan(&existing)
			if q.Error != nil {
				return q.Error
			}
			if q.RowsAffected > 0 {
				if existing.InputDigest != item.digest {
					if err := r.Control.quarantine(tx, "event:"+e.ID.String(), nil, "event identity input conflict", e.Payload); err != nil {
						return err
					}
				}
				continue
			}
			plan, _ := json.Marshal(item.specs)
			status := "accepted"
			if item.reason != "" {
				status = Quarantined
			}
			if err := tx.Exec(`INSERT INTO event_inbox(id,event_type,aggregate_id,input_digest,payload,plan,status,reason,source_xid,source_seq,plan_version)
    VALUES(?,?,?,?,?::jsonb,?::jsonb,?,?,?::xid8,?,?)`, e.ID, e.EventType, e.AggregateID, item.digest, string(e.Payload), string(plan), status, item.reason, e.XID, e.Seq, SubscriptionPlanVersion).Error; err != nil {
				return err
			}
			if item.reason != "" {
				if err := r.Control.quarantine(tx, "event:"+e.ID.String(), nil, item.reason, e.Payload); err != nil {
					return err
				}
				continue
			}
			for _, spec := range item.specs {
				if len(spec.Trace) == 0 {
					spec.Trace = json.RawMessage(e.TraceContext)
				}
				task, err := r.Control.ensure(tx, spec, "event:"+e.ID.String())
				if err != nil {
					return err
				}
				digest, _, digestErr := Digest(spec.Input)
				if digestErr != nil {
					return digestErr
				}
				if task.InputDigest != digest || task.Kind != spec.Kind || task.ShardKey != spec.ShardKey {
					if err := tx.Table("event_inbox").Where("id=?", e.ID).Updates(map[string]any{"status": Quarantined, "reason": "semantic input conflict"}).Error; err != nil {
						return err
					}
					continue
				}
				if r.Accept != nil {
					if err := r.Accept(tx, task, e, spec); err != nil {
						return err
					}
				}
			}
		}
		last := events[len(events)-1]
		return owned(tx.Exec(`UPDATE relay_positions SET last_xid=?::xid8,last_seq=?,version=version+1,updated_at=clock_timestamp()
   WHERE source=? AND version=? AND EXISTS(SELECT 1 FROM module_leases WHERE name='relay' AND token=? AND lease_until>clock_timestamp())`, last.XID, last.Seq, r.Source, p.Version, token))
	})
	return len(events), err
}
