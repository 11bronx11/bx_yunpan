package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/aisystem"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dependencies"
)

func main() {
	if len(os.Args) != 2 {
		fatal(fmt.Errorf("usage: async-handover inspect|transfer|activate"))
	}
	cfg, err := config.LoadForRole("async-admin")
	fatal(err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	db, pool, err := dependencies.OpenRoleDB(ctx, cfg, dependencies.ControlDSN(cfg.Async.ControlDSN), false)
	fatal(err)
	defer pool.Close()
	s := asynccontrol.New(db)
	aisystem.Register(s.Registry)
	planner := &aisystem.Planner{ProjectionBackends: cfg.Search.WriteBackends, TextEpoch: cfg.AITextEpoch(), VectorEpoch: cfg.AIVectorEpoch(), TargetNamespace: cfg.AISplit.ProjectionNamespace, TargetEpoch: cfg.AIProjectionEpoch(), Profile: cfg.AIProfile()}
	s.PlanContinuations, s.PlanFailure = planner.Continuations, planner.Failure
	s.Observe, s.AfterTerminal = planner.Observe, planner.Finish
	if os.Args[1] == "inspect" {
		var rows []map[string]any
		fatal(db.Raw("SELECT protocol_version,state,phase,count(*),count(result_id) AS checkpoints,sum(attempts) AS attempts,sum(dispatches) AS dispatches FROM tasks GROUP BY protocol_version,state,phase").Scan(&rows).Error)
		fatal(json.NewEncoder(os.Stdout).Encode(rows))
		return
	}
	if os.Getenv("ASYNC_LEGACY_WRITERS_STOPPED") != "true" {
		fatal(fmt.Errorf("stop all legacy async writers and set ASYNC_LEGACY_WRITERS_STOPPED=true"))
	}
	if cfg.Async.BrokerPrefix == cfg.Async.LegacyBrokerPrefix {
		fatal(fmt.Errorf("new broker prefix must differ from ASYNC_LEGACY_BROKER_PREFIX"))
	}
	switch os.Args[1] {
	case "transfer":
		for {
			n, err := s.HandoverBatch(ctx, 100)
			fatal(err)
			if n == 0 {
				break
			}
		}
		fatal(s.DrainLegacy(ctx, cfg.TaskQueue.URL, cfg.Async.LegacyBrokerPrefix))
	case "activate":
		fatal(s.DrainLegacy(ctx, cfg.TaskQueue.URL, cfg.Async.LegacyBrokerPrefix))
		fatal(s.Activate(ctx))
	default:
		fatal(fmt.Errorf("unknown action"))
	}
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
