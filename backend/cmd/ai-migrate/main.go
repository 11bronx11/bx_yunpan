package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/aisystem"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncjobs"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dependencies"
	"github.com/google/uuid"
)

func main() {
	if err := run(); err != nil {
		slog.Error("AI migration stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: ai-migrate inspect|rebuild-plan|copy|enqueue-intents|resume|verify|handoff|cutover-check --run UUID [--batch 100]")
	}
	cmd := os.Args[1]
	flags := flag.NewFlagSet(cmd, flag.ContinueOnError)
	runID := flags.String("run", "", "durable migration run UUID")
	batch := flags.Int("batch", 100, "bounded rows per invocation")
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	if *batch < 1 || *batch > 1000 {
		return fmt.Errorf("batch must be 1..1000")
	}
	// Load defaults without granting this one-shot tool an online process role.
	cfg, err := config.LoadForRole("api")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	read, readSQL, err := dependencies.OpenRoleDB(ctx, cfg, cfg.Async.BusinessReadDSN, true)
	if err != nil {
		return err
	}
	defer readSQL.Close()
	ai, aiSQL, err := dependencies.OpenRoleDB(ctx, cfg, cfg.AISplit.MigrationDSN, false)
	if err != nil {
		return err
	}
	defer aiSQL.Close()
	ctrl, ctrlSQL, err := dependencies.OpenRoleDB(ctx, cfg, os.Getenv("AI_CONTROL_MIGRATION_DSN"), false)
	if err != nil {
		return err
	}
	defer ctrlSQL.Close()
	m := &aisystem.Migrator{Read: read, AI: ai, Control: ctrl, Profile: cfg.AIProfile(), Revision: "ai-split-v2-20261116", Batch: *batch}
	if dsn := os.Getenv("AI_INTENT_MIGRATION_DSN"); dsn != "" {
		db, sqlDB, err := dependencies.OpenRoleDB(ctx, cfg, dsn, false)
		if err != nil {
			return err
		}
		defer sqlDB.Close()
		m.IntentWriter = db
	}
	id := uuid.Nil
	if cmd != "inspect" || *runID != "" {
		id, err = uuid.Parse(*runID)
		if err != nil {
			return fmt.Errorf("--run UUID required")
		}
	}
	var result any
	switch cmd {
	case "inspect":
		if id == uuid.Nil {
			result, err = m.Inspect(ctx)
		} else {
			result, err = m.Manifest(ctx, id)
		}
	case "rebuild-plan", "copy", "resume":
		result, err = m.Plan(ctx, id)
	case "enqueue-intents":
		result, err = m.Enqueue(ctx, id)
	case "handoff":
		var report aisystem.MigrationReport
		report, err = m.Handoff(ctx, id)
		if err == nil && !report.HasMore {
			err = asyncjobs.TransferLegacyAIMessages(ctx, cfg.TaskQueue.URL, asynccontrol.New(ctrl), cfg.Async.BrokerPrefix)
			if err == nil {
				err = m.MarkMessagesTransferred(ctx, id)
			}
		}
		result = report
	case "verify":
		result, err = m.Verify(ctx, id)
	case "cutover-check":
		result, err = m.CutoverCheck(ctx, id)
	default:
		return fmt.Errorf("unknown migration command")
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
