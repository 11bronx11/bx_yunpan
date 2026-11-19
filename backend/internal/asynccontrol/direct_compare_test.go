package asynccontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"gorm.io/gorm"
)

// This opt-in protocol microbenchmark requires a private PostgreSQL instance
// with pg_stat_statements. It never resets global statistics. Domain effects,
// network transport, Relay and provider costs are deliberately measured outside
// this test; these numbers describe the control protocol only.
type controlMeasurements struct {
	Selects, Inserts, Updates, Deletes, Rows, Transactions, WAL int64
}

func measureControl(t *testing.T, db *gorm.DB) controlMeasurements {
	t.Helper()
	var m controlMeasurements
	e := db.Raw(`SELECT
 COALESCE(sum(calls) FILTER(WHERE query ~* '^SELECT'),0) AS selects,
 COALESCE(sum(calls) FILTER(WHERE query ~* '^INSERT'),0) AS inserts,
 COALESCE(sum(calls) FILTER(WHERE query ~* '^UPDATE'),0) AS updates,
 COALESCE(sum(calls) FILTER(WHERE query ~* '^DELETE'),0) AS deletes,
 COALESCE(sum(rows) FILTER(WHERE query ~* '^(INSERT|UPDATE|DELETE)'),0) AS rows,
 COALESCE(sum(calls) FILTER(WHERE query ~* '^COMMIT'),0) AS transactions,
 COALESCE(sum(wal_bytes),0) AS wal
 FROM public.pg_stat_statements
 WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
 AND query NOT ILIKE '%pg_stat_statements%'`).Scan(&m).Error
	if e != nil {
		t.Fatal(e)
	}
	return m
}

func TestDirectControlReadWriteComparison(t *testing.T) {
	if os.Getenv("TEST_PG_DIRECT_COMPARE") != "true" {
		t.Skip("requires an exclusive PostgreSQL instance with pg_stat_statements")
	}
	ctx := context.Background()
	for _, protocol := range []int{2, 3} {
		t.Run(fmt.Sprint(protocol), func(t *testing.T) {
			s, _ := testStore(t)
			s.ProtocolVersion = protocol
			if e := s.DB.Exec(fmt.Sprintf("SET bx.coordination_protocol='v%d'", protocol)).Error; e != nil {
				t.Fatal(e)
			}
			for _, kind := range []string{"upload.expire", "multipart.complete", "upload.verify", "canonical.ensure", "thumbnail", "ai.text.build", "ai.project.text"} {
				if _, ok := s.Registry[kind]; !ok {
					s.Registry[kind] = Definition{Kind: kind, Queue: "ai-content", EffectTarget: EffectAI, Timeout: time.Minute, Lifetime: time.Hour}
				}
				const samples = 20
				start := measureControl(t, s.DB)
				began := time.Now()
				var workerTransactions int64
				for n := range samples {
					sp := spec(fmt.Sprintf("compare:%d:%s:%d", protocol, kind, n))
					sp.Kind = kind
					task, e := s.Ensure(ctx, sp, "measurement")
					if e != nil {
						t.Fatal(e)
					}
					r, ok, e := s.Reserve(ctx, task.Queue)
					if e != nil || !ok {
						t.Fatal(ok, e)
					}
					if e = s.Published(ctx, r, nil); e != nil {
						t.Fatal(e)
					}
					workerStart := measureControl(t, s.DB)
					current, ok, e := s.Claim(ctx, r.Notice())
					if e != nil || !ok {
						t.Fatal(ok, e)
					}
					if protocol == 3 {
						e = s.Complete(ctx, current, completion(current))
					} else {
						e = s.execute(ctx, &current, func(context.Context, Task) (json.RawMessage, error) { return json.RawMessage(`{"count":1}`), nil }, []Apply{func(c context.Context, task Task, result Result) (string, *time.Time, error) {
							return Succeeded, nil, s.AcceptedResult(c, task, result)
						}})
					}
					if e != nil {
						t.Fatal(e)
					}
					workerTransactions += measureControl(t, s.DB).Transactions - workerStart.Transactions
				}
				elapsed := time.Since(began)
				end := measureControl(t, s.DB)
				t.Logf("protocol=%d kind=%s samples=%d select=%.1f insert=%.1f update=%.1f delete=%.1f changed_rows=%.1f transactions=%.1f worker_transactions=%.1f statement_wal_bytes=%.1f mean_ms=%.3f", protocol, kind, samples,
					float64(end.Selects-start.Selects)/samples, float64(end.Inserts-start.Inserts)/samples, float64(end.Updates-start.Updates)/samples, float64(end.Deletes-start.Deletes)/samples, float64(end.Rows-start.Rows)/samples, float64(end.Transactions-start.Transactions)/samples, float64(workerTransactions)/samples, float64(end.WAL-start.WAL)/samples, float64(elapsed.Microseconds())/samples/1000)
				if protocol == 3 && workerTransactions != 2*samples {
					t.Fatal("direct worker must use one claim and one completion transaction", workerTransactions)
				}
			}
		})
	}
}
