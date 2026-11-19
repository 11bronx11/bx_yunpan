package aisystem

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type aiPGMeasurements struct {
	Calls, Rows, Transactions, WAL int64
}

func aiPGMeasure(t *testing.T, db *gorm.DB) aiPGMeasurements {
	t.Helper()
	var m aiPGMeasurements
	if e := db.Raw(`SELECT COALESCE(sum(calls),0) AS calls,
 COALESCE(sum(rows) FILTER(WHERE query ~* '^(INSERT|UPDATE|DELETE)'),0) AS rows,
 COALESCE(sum(calls) FILTER(WHERE query ~* '^COMMIT'),0) AS transactions,
 COALESCE(sum(wal_bytes),0) AS wal FROM public.pg_stat_statements
 WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database()) AND query NOT ILIKE '%pg_stat_statements%'`).Scan(&m).Error; e != nil {
		t.Fatal(e)
	}
	return m
}

// Measures real normalized writes and fake-model work with the same source
// text. Parsing, external projections, background loops and contention are
// excluded; this opt-in test requires exclusive PG resources.
func TestDirectAIReadWriteComparison(t *testing.T) {
	if os.Getenv("TEST_PG_DIRECT_COMPARE") != "true" {
		t.Skip("requires exclusive PostgreSQL with pg_stat_statements")
	}
	ctx := context.Background()
	for _, protocol := range []int{2, 3} {
		t.Run(fmt.Sprint(protocol), func(t *testing.T) {
			db, s := databases(t)
			s.ProtocolVersion = protocol
			s.PlanContinuations, s.PlanCompleted = nil, nil
			if e := s.DB.Exec(fmt.Sprintf("SET bx.coordination_protocol='v%d'", protocol)).Error; e != nil {
				t.Fatal(e)
			}
			if e := db.Exec(fmt.Sprintf("SET bx.ai_protocol='v%d'", protocol)).Error; e != nil {
				t.Fatal(e)
			}
			provider := ai.NewProvider(config.AI{Provider: "fake", Dimension: 1024})
			p := &Publisher{DB: db, Control: s}
			computer := &Computer{DB: db, Embedding: provider, Summary: provider, SummaryCalls: 128}
			const samples = 3
			totals := map[string][2]aiPGMeasurements{}
			for sample := range samples {
				in := testInput()
				seedObject(t, db, in)
				if protocol == 3 {
					if e := db.Model(&Object{}).Where("object_id=?", in.Intent.ObjectID).Update("writer_protocol", 3).Error; e != nil {
						t.Fatal(e)
					}
				}
				computer.Profile = in.Profile
				for _, kind := range []string{"ai.source.prepare", "ai.text.build", "ai.embedding.generate", "ai.summary.generate"} {
					beforeAI, beforeControl := aiPGMeasure(t, db), aiPGMeasure(t, s.DB)
					task, e := s.Ensure(ctx, spec(kind, in), "comparison")
					if e != nil {
						t.Fatal(e)
					}
					reserved, ok, e := s.Reserve(ctx, task.Queue)
					if e != nil || !ok {
						t.Fatal(ok, e)
					}
					if e = s.Published(ctx, reserved, nil); e != nil {
						t.Fatal(e)
					}
					task, ok, e = s.Claim(ctx, reserved.Notice())
					if e != nil || !ok {
						t.Fatal(ok, e)
					}
					id := uuid.NewSHA1(task.ID, []byte("output:"+task.InputDigest))
					source := sourceData{Parsed: ai.ParseResult{ParserID: "fixture", Sections: []ai.Section{{Text: strings.Repeat("固定文本 alpha 交接与回执。", 64), Kind: "paragraph"}}}, Coverage: "complete"}
					if protocol == 3 {
						value := &ComputedValue{OutputID: id, Value: source, RowCount: 1, Coverage: "complete"}
						if kind != "ai.source.prepare" {
							value, e = computer.ComputeValue(ctx, task)
						}
						if e != nil {
							t.Fatal(e)
						}
						var done asynccontrol.Completion
						done, e = p.ApplyDirect(ctx, task, value)
						if e == nil {
							e = s.Complete(ctx, task, done)
						}
					} else {
						var result asynccontrol.Result
						if kind == "ai.source.prepare" {
							result, _ = stage(t, db, s, task, in, source, id, 1)
						} else {
							var raw json.RawMessage
							raw, e = computer.Compute(ctx, task)
							if e == nil {
								result, e = s.SaveResult(ctx, task, raw)
							}
						}
						if e == nil {
							e = s.BeginCommit(ctx, &task, result)
						}
						if e == nil {
							var outcome string
							outcome, e = p.Publish(ctx, task, result)
							if e == nil {
								e = s.Report(ctx, task, result, outcome)
							}
						}
					}
					if e != nil {
						t.Fatal(kind, sample, e)
					}
					afterAI, afterControl := aiPGMeasure(t, db), aiPGMeasure(t, s.DB)
					current := totals[kind]
					for n, pair := range [][2]aiPGMeasurements{{beforeAI, afterAI}, {beforeControl, afterControl}} {
						current[n].Calls += pair[1].Calls - pair[0].Calls
						current[n].Rows += pair[1].Rows - pair[0].Rows
						current[n].Transactions += pair[1].Transactions - pair[0].Transactions
						current[n].WAL += pair[1].WAL - pair[0].WAL
					}
					totals[kind] = current
					switch kind {
					case "ai.source.prepare":
						in.SourceID = id
					case "ai.text.build":
						in.ContentVersionID = id
					case "ai.embedding.generate":
						in.EmbeddingSetID = id
					}
				}
			}
			for kind, stores := range totals {
				for n, name := range []string{"ai", "control"} {
					m := stores[n]
					t.Logf("protocol=%d kind=%s store=%s samples=%d sql_calls=%.1f changed_rows=%.1f explicit_transactions=%.1f statement_wal_bytes=%.1f", protocol, kind, name, samples, float64(m.Calls)/samples, float64(m.Rows)/samples, float64(m.Transactions)/samples, float64(m.WAL)/samples)
				}
			}
		})
	}
}
