package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pbv2"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/testfault"
	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"
	"gorm.io/gorm"
)

func TestAISplitDirectScaleRealDependencies(t *testing.T) {
	t.Setenv("TEST_AI_PROTOCOL_VERSION", "3")
	t.Setenv("TEST_AI_SCALE", "true")
	t.Setenv("TEST_AI_ONLINE", "true")
	TestAISplitRealDependencies(t)
}

// Production role bootstraps share real queues and distinct DB pools; each
// query replica registers a different endpoint with the real etcd resolver.
func runAIScaleChecks(t *testing.T, ctx context.Context, cfg config.Config, control, aidb *gorm.DB, base, a, b string, stops map[string]func(), start func(string, func(context.Context, config.Config, *slog.Logger) error) func(), request func(string, string, any, int) map[string]any, upload func(string, string, ...string) string) {
	t.Helper()
	await := func(label string, fn func() bool) {
		t.Helper()
		until := time.Now().Add(60 * time.Second)
		for ctx.Err() == nil && time.Now().Before(until) {
			if fn() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("scale check timed out:", label)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	fault := testfault.NewTCP(t, address)
	replicas := map[string]func(){}
	for _, role := range []string{"ai-content-worker", "ai-embedding-worker", "ai-summary-worker", "ai-text-projector", "ai-vector-projector", "ai-query"} {
		stop := start(role, func(c context.Context, v config.Config, logger *slog.Logger) error {
			if role == "ai-query" {
				v.AISplit.QueryAddr, v.AISplit.QueryAdvertise = address, fault.Address
			}
			return RunAISplit(c, v, logger, role)
		})
		replicas[role] = stop
	}
	conn, cleanup, err := internalClient(cfg, "ai-query-v2", cfg.AISplit.QueryTarget, cfg.AISplit.QueryToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	defer conn.Close()
	client := pbv2.NewAIQueryServiceClient(conn)
	seen := map[string]bool{}
	await("round robin reaches both actual query replicas", func() bool {
		callCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		var remote peer.Peer
		if _, err := client.BatchCapabilities(callCtx, &pbv2.BatchCapabilitiesRequest{}, grpc.Peer(&remote)); err != nil {
			return false
		}
		if remote.Addr != nil {
			seen[remote.Addr.String()] = true
		}
		return seen[fault.Address] && seen[cfg.AISplit.QueryAdvertise]
	})
	t.Logf("query replicas reached through authenticated etcd round_robin: %v", seen)
	fault.Cut()
	await("healthy query replica serves while peer sockets are cut", func() bool {
		for range 8 {
			callCtx, cancel := context.WithTimeout(ctx, time.Second)
			var remote peer.Peer
			_, err := client.BatchCapabilities(callCtx, &pbv2.BatchCapabilitiesRequest{}, grpc.Peer(&remote))
			cancel()
			if err != nil || remote.Addr == nil || remote.Addr.String() != cfg.AISplit.QueryAdvertise {
				return false
			}
		}
		return true
	})
	request("POST", "/api/v1/search", map[string]any{"query": "rollback", "mode": "fulltext"}, 200)
	request("POST", "/api/v1/ai/ask", map[string]any{"question": "rollback", "scope": map[string]any{"mode": "selected", "file_ids": []string{a}}}, 200)
	fault.Restore()
	seen = map[string]bool{}
	await("recovered query replica rejoins round robin", func() bool {
		callCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		var remote peer.Peer
		if _, err := client.BatchCapabilities(callCtx, &pbv2.BatchCapabilitiesRequest{}, grpc.Peer(&remote)); err == nil && remote.Addr != nil {
			seen[remote.Addr.String()] = true
		}
		return seen[fault.Address] && seen[cfg.AISplit.QueryAdvertise]
	})
	t.Log("query replica TCP sockets cut/restored: surviving replica served HTTP Search/Ask and recovered peer rejoined round_robin")

	files := []string{a, b}
	for n := range 4 {
		files = append(files, upload(fmt.Sprintf("scale-%d.txt", n), fmt.Sprintf("rollback scale marker %d 保留版本、权限和回滚证据。", n)))
	}
	await("initial six files published", func() bool {
		var ready int64
		return aidb.Table("ai_objects").Where("active_content_version_id IS NOT NULL AND active_embedding_set_id IS NOT NULL AND active_summary_id IS NOT NULL").Count(&ready).Error == nil && ready == 7 // includes the source-code MIME fixture
	})
	var jobs []string
	for _, file := range files {
		job := request("POST", "/api/v1/files/"+file+"/ai/reprocess", map[string]any{"targets": []string{"text", "vectors", "summary"}}, 202)
		jobs = append(jobs, job["job_id"].(string))
	}
	// Withdraw one complete set while durable work is outstanding. Surviving
	// consumers must finish it without resetting task identity or retry budgets.
	for _, role := range []string{"ai-content-worker", "ai-embedding-worker", "ai-summary-worker", "ai-text-projector", "ai-vector-projector", "ai-query"} {
		stops[role]()
		stops[role] = replicas[role]
	}
	await("withdrawn query endpoint removed from resolver", func() bool {
		for range 4 {
			callCtx, cancel := context.WithTimeout(ctx, time.Second)
			var remote peer.Peer
			_, err := client.BatchCapabilities(callCtx, &pbv2.BatchCapabilitiesRequest{}, grpc.Peer(&remote))
			cancel()
			if err != nil || remote.Addr == nil || remote.Addr.String() != fault.Address {
				return false
			}
		}
		return true
	})
	for _, job := range jobs {
		await("rebuild job on surviving consumers", func() bool {
			return request("GET", "/api/v1/ai/jobs/"+job, nil, 200)["status"] == "succeeded"
		})
	}
	var duplicateReceipts, mismatchedVectors, genericArtifacts, oldResults int64
	checks := []struct {
		db    *gorm.DB
		sql   string
		count *int64
	}{
		{aidb, "SELECT count(*) FROM (SELECT task_id FROM ai_commit_receipts GROUP BY task_id HAVING count(*)<>1) r", &duplicateReceipts},
		{aidb, "SELECT count(*) FROM ai_objects o JOIN ai_embedding_sets s ON s.id=o.active_embedding_set_id WHERE s.content_version_id<>o.active_content_version_id", &mismatchedVectors},
		{aidb, "SELECT count(*) FROM ai_artifact_manifests", &genericArtifacts},
		{control, "SELECT count(*) FROM results", &oldResults},
	}
	for _, check := range checks {
		if err := check.db.Raw(check.sql).Scan(check.count).Error; err != nil || *check.count != 0 {
			t.Fatal("scale invariant failed", check.sql, *check.count, err)
		}
	}
	t.Logf("1->2->1 replicas: six rebuild jobs succeeded; one receipt/task, compatible active vectors, zero generic artifacts/control results; HTTP endpoint %s", base)
}
