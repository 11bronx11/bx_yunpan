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
		fail(fmt.Errorf("usage: ai-direct-admin inspect|transfer|activate|archive"))
	}
	cfg, e := config.LoadForRole("async-admin")
	fail(e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	db, pool, e := dependencies.OpenRoleDB(ctx, cfg, dependencies.ProtocolDSN(cfg.AISplit.MigrationDSN, 3), false)
	fail(e)
	defer pool.Close()
	control, cp, e := dependencies.OpenRoleDB(ctx, cfg, dependencies.ProtocolDSN(cfg.Async.ControlDSN, 3), false)
	fail(e)
	defer cp.Close()
	store := asynccontrol.New(control)
	store.ProtocolVersion = 3
	m := &aisystem.DirectMigrator{DB: db, Control: store}
	if os.Args[1] == "inspect" {
		var rows []map[string]any
		fail(db.Raw("SELECT writer_protocol,count(*) FROM ai_objects GROUP BY writer_protocol").Scan(&rows).Error)
		fail(json.NewEncoder(os.Stdout).Encode(rows))
		return
	}
	if os.Getenv("AI_LEGACY_WRITERS_STOPPED") != "true" {
		fail(fmt.Errorf("stop old AI writers and set AI_LEGACY_WRITERS_STOPPED=true"))
	}
	switch os.Args[1] {
	case "transfer":
		copied, retained, e := m.TransferNodes(ctx)
		fail(e)
		fmt.Printf("summary_nodes_verified=%d historical_nodes_retained=%d\n", copied, retained)
	case "activate":
		for {
			n, e := m.ActivateObjects(ctx, 100)
			fail(e)
			if n == 0 {
				break
			}
		}
	case "archive":
		sql, e := os.ReadFile("ai-contract-migrations/000001_archive_artifacts.up.sql")
		fail(e)
		fail(m.Archive(ctx, string(sql)))
	default:
		fail(fmt.Errorf("unknown action"))
	}
}

func fail(e error) {
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
