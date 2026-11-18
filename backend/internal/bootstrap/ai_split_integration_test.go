package bootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncjobs"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/testdb"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"gorm.io/gorm"
)

// Real business/control/AI PG connections, RabbitMQ, MinIO, parser subprocess,
// etcd discovery and independent ES/Qdrant projections. Providers are fake.
func TestAISplitRealDependencies(t *testing.T) {
	if os.Getenv("TEST_AI_SPLIT") != "true" {
		t.Skip("TEST_AI_SPLIT enables isolated full-chain resources")
	}
	for _, key := range []string{"TEST_POSTGRES_DSN", "TEST_CONTROL_POSTGRES_DSN", "TEST_AI_POSTGRES_DSN", "TEST_POSTGRES_READONLY_DSN", "TEST_RABBITMQ_URL", "TEST_RABBITMQ_MANAGEMENT_URL", "TEST_MINIO_ENDPOINT", "TEST_SEARCH_ES_ENDPOINT", "TEST_SEARCH_QDRANT_ENDPOINT", "TEST_ETCD_ENDPOINT"} {
		if os.Getenv(key) == "" {
			t.Fatalf("missing %s", key)
		}
	}
	biz, ctrl, aidb := testdb.Business(t), testdb.Control(t), testdb.AI(t)
	scoped := func(db *gorm.DB, key string) string {
		var schema string
		if err := db.Raw("SELECT current_schema()").Scan(&schema).Error; err != nil {
			t.Fatal(err)
		}
		u, e := url.Parse(os.Getenv(key))
		if e != nil {
			t.Fatal(e)
		}
		q := u.Query()
		q.Set("search_path", schema+",public")
		u.RawQuery = q.Encode()
		return u.String()
	}
	bizDSN, ctrlDSN, aiDSN := scoped(biz, "TEST_POSTGRES_DSN"), scoped(ctrl, "TEST_CONTROL_POSTGRES_DSN"), scoped(aidb, "TEST_AI_POSTGRES_DSN")
	var bizSchema, aiSchema string
	_ = biz.Raw("SELECT current_schema()").Scan(&bizSchema)
	_ = aidb.Raw("SELECT current_schema()").Scan(&aiSchema)
	reader, _ := url.Parse(os.Getenv("TEST_POSTGRES_READONLY_DSN"))
	readerRole := reader.User.Username()
	if err := biz.Exec(fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s;GRANT SELECT ON ALL TABLES IN SCHEMA %s TO %s", bizSchema, readerRole, bizSchema, readerRole)).Error; err != nil {
		t.Fatal(err)
	}
	readerDSN, _ := url.Parse(bizDSN)
	readerDSN.User = reader.User
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	// Each run owns a vhost and projection namespace. Failure proxies affect
	// only this fixture, while successful requests reach real ES and Qdrant.
	rabbit, _ := url.Parse(os.Getenv("TEST_RABBITMQ_URL"))
	rabbitPassword, _ := rabbit.User.Password()
	vhost := "ai-split-" + suffix
	management := func(method, path string, body string) {
		t.Helper()
		req, _ := http.NewRequest(method, os.Getenv("TEST_RABBITMQ_MANAGEMENT_URL")+path, strings.NewReader(body))
		req.SetBasicAuth(rabbit.User.Username(), rabbitPassword)
		req.Header.Set("Content-Type", "application/json")
		res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatal("test vhost unavailable")
		}
		defer res.Body.Close()
		if res.StatusCode >= 300 {
			t.Fatalf("test vhost status %d", res.StatusCode)
		}
	}
	management("PUT", "/api/vhosts/"+vhost, `{}`)
	management("PUT", "/api/permissions/"+vhost+"/"+url.PathEscape(rabbit.User.Username()), `{"configure":".*","write":".*","read":".*"}`)
	t.Cleanup(func() { management("DELETE", "/api/vhosts/"+vhost, "") })
	rabbit.Path = "/" + vhost
	t.Setenv("TEST_RABBITMQ_URL", rabbit.String())
	var esFail, qdFail atomic.Bool
	proxy := func(key string, fail *atomic.Bool) string {
		u, err := url.Parse(os.Getenv(key))
		if err != nil {
			t.Fatal(err)
		}
		forward := httputil.NewSingleHostReverseProxy(u)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fail.Load() {
				http.Error(w, "fixture unavailable", 503)
				return
			}
			forward.ServeHTTP(w, r)
		}))
		t.Cleanup(server.Close)
		return server.URL
	}
	esEndpoint, qdEndpoint := proxy("TEST_SEARCH_ES_ENDPOINT", &esFail), proxy("TEST_SEARCH_QDRANT_ENDPOINT", &qdFail)
	roleDSNs := map[string]string{}
	for _, kind := range []string{"query", "compute", "commit"} {
		role := "ai_" + kind + "_" + suffix
		password := uuid.NewString()
		if err := aidb.Exec(fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s';GRANT USAGE ON SCHEMA %s TO %s", role, password, aiSchema, role)).Error; err != nil {
			t.Fatal(err)
		}
		// Cleanup runs after online pools close, before the fixture schema drops.
		t.Cleanup(func() { _ = aidb.Exec("DROP OWNED BY " + role).Error; _ = aidb.Exec("DROP ROLE " + role).Error })
		if kind == "commit" {
			if err := aidb.Exec(fmt.Sprintf("GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA %s TO %s", aiSchema, role)).Error; err != nil {
				t.Fatal(err)
			}
		} else {
			var views []string
			_ = aidb.Raw("SELECT viewname FROM pg_views WHERE schemaname=? AND viewname LIKE 'ai_published_%'", aiSchema).Scan(&views)
			for _, v := range views {
				if err := aidb.Exec(fmt.Sprintf("GRANT SELECT ON %s.%s TO %s", aiSchema, v, role)).Error; err != nil {
					t.Fatal(err)
				}
			}
			if kind == "compute" {
				if err := aidb.Exec(fmt.Sprintf("GRANT SELECT ON %s.ai_projection_catalog TO %s", aiSchema, role)).Error; err != nil {
					t.Fatal(err)
				}
				if err := aidb.Exec(fmt.Sprintf("GRANT INSERT ON %s.ai_artifact_manifests TO %s", aiSchema, role)).Error; err != nil {
					t.Fatal(err)
				}
			}
		}
		u, _ := url.Parse(aiDSN)
		u.User = url.UserPassword(role, password)
		roleDSNs[kind] = u.String()
	}
	for key, value := range map[string]string{"POSTGRES_PRIMARY_DSN": bizDSN, "POSTGRES_READONLY_DSN": readerDSN.String(), "ASYNC_CONTROL_DSN": ctrlDSN, "RABBITMQ_URL": os.Getenv("TEST_RABBITMQ_URL"), "S3_ENDPOINT": os.Getenv("TEST_MINIO_ENDPOINT"), "S3_PUBLIC_ENDPOINT": os.Getenv("TEST_MINIO_ENDPOINT"), "S3_ACCESS_KEY": os.Getenv("TEST_MINIO_ACCESS_KEY"), "S3_SECRET_KEY": os.Getenv("TEST_MINIO_SECRET_KEY"), "S3_BUCKET": "ai-split-" + suffix, "S3_PUBLIC_PATH_PREFIX": "", "AI_QUERY_DSN": roleDSNs["query"], "AI_COMPUTE_DSN": roleDSNs["compute"], "AI_COMMIT_DSN": roleDSNs["commit"], "AI_ENABLED": "true", "AI_PROVIDER": "fake", "AI_PROFILE_ID": "smoke-" + suffix, "AI_PROJECTION_NAMESPACE": "bx-ai-test-" + suffix, "AI_PROFILE_REVISION": "v1", "ETCD_ENABLED": "true", "ETCD_ENDPOINTS": os.Getenv("TEST_ETCD_ENDPOINT"), "PARSER_WORKER_DIR": "../../parser_worker", "PARSER_REMOTE_ENABLED": "true", "PARSER_SERVICE_TOKEN": uuid.NewString(), "CORE_INTERNAL_SERVICE_TOKEN": uuid.NewString(), "AI_QUERY_SERVICE_TOKEN": uuid.NewString(), "CACHE_ENABLED": "false", "AI_RATE_LIMIT_ENABLED": "false", "POSTGRES_MAX_OPEN_CONNS": "2", "POSTGRES_MAX_IDLE_CONNS": "1", "ASYNC_CONTROL_MAX_OPEN_CONNS": "2", "ASYNC_CONTROL_CRITICAL_CONNS": "1", "ASYNC_WORKER_CONCURRENCY": "1", "ASYNC_BROKER_PREFIX": "bx.ai.test." + suffix, "ASYNC_RETRY_BASE": "100ms", "ASYNC_RETRY_CAP": "1s", "SEARCH_READ_BACKEND": "es_qdrant", "SEARCH_WRITE_BACKENDS": "postgres,es_qdrant", "SEARCH_ES_ENDPOINT": esEndpoint, "SEARCH_QDRANT_ENDPOINT": qdEndpoint, "AI_EMBEDDING_REVISION": "config-v1", "AI_SUMMARY_REVISION": "config-v1", "AI_SUMMARY_PROMPT_VERSION": "overview-v1"} {
		t.Setenv(key, value)
	}
	for _, prefix := range []string{"AI_SCOPE", "AI_PROCESSING"} {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		t.Setenv(prefix+"_SIGNING_SEED", base64.RawURLEncoding.EncodeToString(priv.Seed()))
		t.Setenv(prefix+"_PUBLIC_KEY", base64.RawURLEncoding.EncodeToString(pub))
	}
	addr := func() string {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		a := l.Addr().String()
		_ = l.Close()
		return a
	}
	t.Setenv("HTTP_ADDR", addr())
	t.Setenv("CORE_INTERNAL_GRPC_ADDR", addr())
	t.Setenv("CORE_INTERNAL_ADVERTISE_ADDR", os.Getenv("CORE_INTERNAL_GRPC_ADDR"))
	t.Setenv("CORE_INTERNAL_TARGET", os.Getenv("CORE_INTERNAL_GRPC_ADDR"))
	t.Setenv("AI_QUERY_GRPC_ADDR", addr())
	t.Setenv("AI_QUERY_ADVERTISE_ADDR", os.Getenv("AI_QUERY_GRPC_ADDR"))
	t.Setenv("AI_QUERY_TARGET", os.Getenv("AI_QUERY_GRPC_ADDR"))
	t.Setenv("PARSER_GRPC_ADDR", addr())
	t.Setenv("PARSER_ADVERTISE_ADDR", os.Getenv("PARSER_GRPC_ADDR"))
	t.Setenv("PARSER_ENGINE_ADDR", addr())
	t.Setenv("PARSER_ETCD_SERVICE_PREFIX", "/services/parser-"+suffix)
	browserBase := ""
	if build := os.Getenv("TEST_AI_BROWSER_WEB_ROOT"); build != "" {
		apiTarget, _ := url.Parse("http://" + os.Getenv("HTTP_ADDR"))
		storageTarget, _ := url.Parse("http://" + os.Getenv("TEST_MINIO_ENDPOINT"))
		apiProxy, storageProxy := httputil.NewSingleHostReverseProxy(apiTarget), httputil.NewSingleHostReverseProxy(storageTarget)
		mux := http.NewServeMux()
		mux.Handle("/api/", apiProxy)
		mux.Handle("/storage/", http.StripPrefix("/storage", storageProxy))
		files := http.FileServer(http.Dir(build))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if _, err := os.Stat(filepath.Join(build, filepath.Clean("/"+r.URL.Path))); err != nil {
				http.ServeFile(w, r, filepath.Join(build, "index.html"))
				return
			}
			files.ServeHTTP(w, r)
		})
		web := httptest.NewServer(mux)
		t.Cleanup(web.Close)
		browserBase = web.URL
		t.Setenv("S3_PUBLIC_ENDPOINT", strings.TrimPrefix(web.URL, "http://"))
		t.Setenv("S3_PUBLIC_PATH_PREFIX", "/storage")
	}
	cfg, err := config.LoadForRole("api")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Worker.MetricsAddr = ""
	oldProvider, oldPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	exporter := tracetest.NewInMemoryExporter()
	traceProvider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(traceProvider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	defer func() {
		_ = traceProvider.Shutdown(context.Background())
		otel.SetTracerProvider(oldProvider)
		otel.SetTextMapPropagator(oldPropagator)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	ctx, rootSpan := otel.Tracer("parity").Start(ctx, "fullchain.http")
	defer rootSpan.End()
	defer cancel()
	storage, err := minio.New(cfg.Storage.Endpoint, &minio.Options{Creds: credentials.NewStaticV4(cfg.Storage.AccessKey, cfg.Storage.SecretKey, ""), Region: cfg.Storage.Region})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.MakeBucket(ctx, cfg.Storage.Bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for o := range storage.ListObjects(context.Background(), cfg.Storage.Bucket, minio.ListObjectsOptions{Recursive: true}) {
			_ = storage.RemoveObject(context.Background(), cfg.Storage.Bucket, o.Key, minio.RemoveObjectOptions{})
		}
		_ = storage.RemoveBucket(context.Background(), cfg.Storage.Bucket)
	}()
	// Delete only immutable targets created in this run's unique namespace.
	defer func() {
		var targets []struct{ Backend, TargetVersion string }
		_ = aidb.Table("ai_projection_targets").Find(&targets).Error
		for _, target := range targets {
			if !strings.HasPrefix(target.TargetVersion, "bx-ai-test-"+suffix+"-") {
				continue
			}
			endpoint := esEndpoint + "/" + target.TargetVersion
			if target.Backend == "qd" {
				endpoint = qdEndpoint + "/collections/" + target.TargetVersion
			}
			req, _ := http.NewRequest("DELETE", endpoint, nil)
			if target.Backend == "es" {
				req.SetBasicAuth(os.Getenv("TEST_SEARCH_ES_USERNAME"), os.Getenv("TEST_SEARCH_ES_PASSWORD"))
			} else {
				req.Header.Set("api-key", os.Getenv("TEST_SEARCH_QDRANT_API_KEY"))
			}
			res, e := (&http.Client{Timeout: 5 * time.Second}).Do(req)
			if e == nil {
				res.Body.Close()
			}
		}
	}()
	s := asynccontrol.New(ctrl)
	if err := asyncjobs.CheckImport(ctx, biz, s); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	logs := slog.New(slog.NewTextHandler(os.Stderr, nil))
	start := func(role string, fn func(context.Context, config.Config, *slog.Logger) error) {
		local := cfg
		local.Role = role
		local.App.InstanceID = role + "-" + suffix
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := fn(ctx, local, logs); e != nil && ctx.Err() == nil {
				t.Errorf("%s: %v", role, e)
			}
		}()
	}
	defer func() { cancel(); wg.Wait() }()
	start("parser-svc", RunParserSvc)
	for _, r := range []string{"core-internal", "ai-query", "ai-committer", "ai-content-worker", "ai-embedding-worker", "ai-summary-worker", "ai-text-projector", "ai-vector-projector"} {
		role := r
		start(role, func(c context.Context, v config.Config, l *slog.Logger) error { return RunAISplit(c, v, l, role) })
	}
	for _, r := range []string{"async-control", "committer", "upload-worker", "object-worker", "media-worker", "gc-worker"} {
		role := r
		start(role, func(c context.Context, v config.Config, l *slog.Logger) error { return RunAsync(c, v, l, role) })
	}
	start("api", RunAPI)
	client := &http.Client{Timeout: 15 * time.Second}
	base := "http://" + cfg.HTTP.Addr
	token := ""
	request := func(method, path string, body any, want int) map[string]any {
		t.Helper()
		var reader io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			reader = bytes.NewReader(b)
		}
		req, _ := http.NewRequestWithContext(ctx, method, base+path, reader)
		otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", uuid.NewString())
		res, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != want {
			t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, b)
		}
		out := map[string]any{}
		_ = json.Unmarshal(b, &out)
		return out
	}
	await := func(label string, fn func() bool) {
		t.Helper()
		until := time.Now().Add(80 * time.Second)
		for time.Now().Before(until) {
			if fn() {
				return
			}
			if ctx.Err() != nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("timed out: %s", label)
	}
	await("api", func() bool {
		res, e := client.Get(base + "/health/live")
		if e != nil {
			return false
		}
		defer res.Body.Close()
		return res.StatusCode == 200
	})
	username, password := "ai_"+suffix, "ai-split-smoke-password"
	reg := request("POST", "/api/v1/auth/register", map[string]any{"username": username, "password": password}, 201)
	token = reg["access_token"].(string)
	root := request("GET", "/api/v1/folders/root", nil, 200)["id"].(string)
	upload := func(name, content string, mediaTypes ...string) string {
		t.Helper()
		b := []byte(content)
		hash := sha256.Sum256(b)
		mediaType := "text/plain"
		if len(mediaTypes) > 0 {
			mediaType = mediaTypes[0]
		}
		created := request("POST", "/api/v1/uploads", map[string]any{"folder_id": root, "filename": name, "sha256": hex.EncodeToString(hash[:]), "size_bytes": len(b), "mime_type": mediaType}, 201)["upload"].(map[string]any)
		id := created["id"].(string)
		await("multipart", func() bool { return request("GET", "/api/v1/uploads/"+id, nil, 200)["status"] == "created" })
		signed := request("POST", "/api/v1/uploads/"+id+"/parts/presign", map[string]any{"part_numbers": []int{1}}, 200)["parts"].([]any)[0].(map[string]any)["url"].(string)
		put, _ := http.NewRequestWithContext(ctx, "PUT", signed, bytes.NewReader(b))
		res, e := client.Do(put)
		if e != nil {
			t.Fatal(e)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatal("MinIO upload", res.StatusCode)
		}
		etag := strings.Trim(res.Header.Get("ETag"), "\"")
		request("POST", "/api/v1/uploads/"+id+"/parts/confirm", map[string]any{"parts": []map[string]any{{"part_number": 1, "etag": etag, "size_bytes": len(b)}}}, 200)
		request("POST", "/api/v1/uploads/"+id+"/complete", nil, 202)
		var done map[string]any
		await("upload complete", func() bool {
			done = request("GET", "/api/v1/uploads/"+id, nil, 200)
			return done["status"] == "completed"
		})
		return done["completed_entry_id"].(string)
	}
	a := upload("alpha.txt", "alpha rollback 项目预算已经批准。保留回滚旧版本，恢复只读配置，检查数据完整性。\n"+strings.Repeat("上线前校验 source text vector summary。\n", 80))
	b := upload("beta.txt", "beta rollback 项目已启动，预算尚未批准。回滚使用快照，不删除历史数据。")
	await("all capabilities and projections", func() bool {
		var count int64
		_ = aidb.Table("ai_objects").Where("active_content_version_id IS NOT NULL AND active_embedding_set_id IS NOT NULL AND active_summary_id IS NOT NULL").Count(&count)
		var projected int64
		_ = aidb.Table("ai_projection_states").Where("state='complete'").Count(&projected)
		return count == 2 && projected >= 4
	})
	list := request("GET", "/api/v1/ai/files?limit=1", nil, 200)
	if len(list["items"].([]any)) != 1 || !list["page"].(map[string]any)["has_more"].(bool) {
		t.Fatal("eligibility pagination", list)
	}
	full := request("POST", "/api/v1/search", map[string]any{"query": "rollback", "mode": "fulltext"}, 200)
	if full["retrieval"].(map[string]any)["text_backend"] != "elasticsearch" {
		t.Fatal("ES projection unused", full)
	}
	for _, forcePG := range []bool{false, true} {
		esFail.Store(forcePG)
		for _, query := range []string{"项目预算", "目", "项目 天气"} {
			result := request("POST", "/api/v1/search", map[string]any{"query": query, "mode": "fulltext"}, 200)
			hits := result["hits"].([]any)
			want := 1
			if query == "目" {
				want = 2
			}
			if query == "项目 天气" {
				want = 0
			}
			if len(hits) != want {
				t.Fatalf("Chinese query %q fallback=%t got %d want %d", query, forcePG, len(hits), want)
			}
			if query == "项目预算" && hits[0].(map[string]any)["file"].(map[string]any)["id"] != a {
				t.Fatal("nonadjacent Chinese false positive")
			}
		}
	}
	esFail.Store(false)
	fuzzy := request("POST", "/api/v1/search", map[string]any{"query": "alhpa.txt", "mode": "name"}, 200)
	if len(fuzzy["hits"].([]any)) == 0 || fuzzy["hits"].([]any)[0].(map[string]any)["file"].(map[string]any)["id"] != a {
		t.Fatal("name typo ranking lost", fuzzy)
	}
	semantic := request("POST", "/api/v1/search", map[string]any{"query": "rollback", "mode": "semantic"}, 200)
	if semantic["retrieval"].(map[string]any)["vector_backend"] != "qdrant" {
		t.Fatal("QD projection unused", semantic)
	}
	esFail.Store(true)
	fallback := request("POST", "/api/v1/search", map[string]any{"query": "rollback", "mode": "fulltext"}, 200)
	if fallback["retrieval"].(map[string]any)["text_backend"] != "postgres" || len(fallback["hits"].([]any)) == 0 {
		t.Fatal("ES fallback failed", fallback)
	}
	independent := request("POST", "/api/v1/search", map[string]any{"query": "rollback", "mode": "semantic"}, 200)
	if independent["retrieval"].(map[string]any)["vector_backend"] != "qdrant" {
		t.Fatal("ES failure affected QD", independent)
	}
	esFail.Store(false)
	qdFail.Store(true)
	fallback = request("POST", "/api/v1/search", map[string]any{"query": "rollback", "mode": "semantic"}, 200)
	if fallback["retrieval"].(map[string]any)["vector_backend"] != "postgres" || len(fallback["hits"].([]any)) == 0 {
		t.Fatal("QD fallback failed", fallback)
	}
	independent = request("POST", "/api/v1/search", map[string]any{"query": "rollback", "mode": "fulltext"}, 200)
	if independent["retrieval"].(map[string]any)["text_backend"] != "elasticsearch" {
		t.Fatal("QD failure affected ES", independent)
	}
	qdFail.Store(false)
	for _, ids := range [][]string{{a}, {a, b}} {
		ans := request("POST", "/api/v1/ai/ask", map[string]any{"question": "rollback 回滚策略是什么", "scope": map[string]any{"mode": "selected", "file_ids": ids}}, 200)
		if len(ans["citations"].([]any)) == 0 {
			t.Fatal("missing citations", ans)
		}
		for _, raw := range ans["citations"].([]any) {
			c := raw.(map[string]any)
			found := false
			for _, id := range ids {
				if c["file_id"] == id {
					found = true
				}
			}
			if !found {
				t.Fatal("scope escaped", c)
			}
		}
	}
	overview := request("GET", "/api/v1/files/"+a+"/ai", nil, 200)
	if !overview["capabilities"].(map[string]any)["askable"].(bool) {
		t.Fatal("not askable", overview)
	}
	job := request("POST", "/api/v1/files/"+a+"/ai/reprocess", map[string]any{"targets": []string{"summary"}}, 202)
	await("summary user job", func() bool {
		return request("GET", "/api/v1/ai/jobs/"+job["job_id"].(string), nil, 200)["status"] == "succeeded"
	})
	request("POST", "/api/v1/ai/ask", map[string]any{"question": "rollback", "scope": map[string]any{"mode": "selected", "file_ids": []string{uuid.NewString()}}}, 409)
	request("POST", "/api/v1/ai/ask", map[string]any{"question": "rollback", "scope": map[string]any{"mode": "selected", "file_ids": []string{}}}, 400)
	sourceFile := upload("safe-code.go", "package main\n// rollback 保留历史版本\nfunc main() {}\n", "application/octet-stream")
	await("source MIME fallback", func() bool {
		caps := request("GET", "/api/v1/files/"+sourceFile+"/ai", nil, 200)["capabilities"].(map[string]any)
		return caps["askable"].(bool) && caps["summary"].(map[string]any)["available"].(bool)
	})
	codeJob := request("POST", "/api/v1/files/"+sourceFile+"/ai/reprocess", map[string]any{"targets": []string{"summary"}}, 202)
	await("source MIME summary rebuild", func() bool {
		return request("GET", "/api/v1/ai/jobs/"+codeJob["job_id"].(string), nil, 200)["status"] == "succeeded"
	})
	tracedStages := map[string]bool{}
	for _, span := range exporter.GetSpans() {
		if span.SpanContext.TraceID() == rootSpan.SpanContext().TraceID() {
			tracedStages[span.Name] = true
		}
	}
	for _, name := range []string{"ai.source.prepare", "ai.text.build", "ai.embedding.generate", "ai.summary.generate", "ai.publish", "ai.search", "ai.ask"} {
		if !tracedStages[name] {
			t.Fatalf("distributed trace lost stage %s", name)
		}
	}
	t.Log("real upload -> source -> parent/child text -> vectors/summary -> independent ES/QD -> Search/Ask -> citations and summary-only user job: PASS")
	if ready := os.Getenv("TEST_AI_BROWSER_READY_FILE"); ready != "" {
		raw, _ := json.Marshal(map[string]any{"base_url": browserBase, "api": base, "storage": "http://" + cfg.Storage.Endpoint, "username": username, "password": password, "file_ids": []string{a, b}, "root": root})
		if err := os.WriteFile(ready, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(ready)
		t.Log("browser fixture ready", filepath.Base(ready))
		for {
			if _, e := os.Stat(ready + ".done"); e == nil {
				break
			}
			if ctx.Err() != nil {
				t.Fatal("browser fixture timed out")
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}
