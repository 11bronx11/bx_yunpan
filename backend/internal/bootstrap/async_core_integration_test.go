package bootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncjobs"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

// This test uses actual HTTP, both PostgreSQL databases, quorum RabbitMQ
// queues and MinIO bytes. It must run only against dedicated test resources.
func TestAsyncCoreRealDependencies(t *testing.T) {
	if os.Getenv("TEST_ASYNC_CORE") != "true" {
		t.Skip("TEST_ASYNC_CORE enables dedicated end-to-end resources")
	}
	protocol := "2"
	if os.Getenv("TEST_ASYNC_PROTOCOL_VERSION") == "3" {
		protocol = "3"
	}
	t.Setenv("ASYNC_PROTOCOL_VERSION", protocol)
	for _, key := range []string{"TEST_POSTGRES_DSN", "TEST_CONTROL_POSTGRES_DSN", "TEST_POSTGRES_READONLY_DSN", "TEST_RABBITMQ_URL", "TEST_MINIO_ENDPOINT", "TEST_MINIO_ACCESS_KEY", "TEST_MINIO_SECRET_KEY"} {
		if os.Getenv(key) == "" {
			t.Fatalf("missing %s", key)
		}
	}
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	schema := "async_smoke_" + suffix
	scope := func(key string, control bool) {
		original := os.Getenv(key)
		admin, err := gorm.Open(postgres.Open(original), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		pool, _ := admin.DB()
		defer pool.Close()
		if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(original)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("search_path", schema+",public")
		q.Set("options", "-c bx.coordination_protocol=v"+protocol)
		u.RawQuery = q.Encode()
		t.Setenv(key, u.String())
		scoped, err := gorm.Open(postgres.Open(u.String()), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		sp, _ := scoped.DB()
		defer sp.Close()
		if control {
			paths, _ := filepath.Glob("../../control-migrations/*.up.sql")
			sort.Strings(paths)
			for _, p := range paths {
				b, _ := os.ReadFile(p)
				if err := scoped.Exec(string(b)).Error; err != nil {
					t.Fatal(err)
				}
			}
			if protocol == "3" {
				if e := scoped.Exec("UPDATE control_settings SET value='v3' WHERE key='coordination_protocol'").Error; e != nil {
					t.Fatal(e)
				}
			}
		} else {
			if err := scoped.Exec(`DO $$ DECLARE r record; BEGIN FOR r IN SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename<>'schema_migrations' LOOP EXECUTE format('CREATE TABLE %I.%I (LIKE public.%I INCLUDING ALL)',current_schema(),r.tablename,r.tablename); END LOOP; END $$`).Error; err != nil {
				t.Fatal(err)
			}
			ru, err := url.Parse(os.Getenv("TEST_POSTGRES_READONLY_DSN"))
			if err != nil {
				t.Fatal(err)
			}
			rq := ru.Query()
			rq.Set("search_path", schema+",public")
			q.Set("options", "-c bx.coordination_protocol=v"+protocol)
			ru.RawQuery = rq.Encode()
			t.Setenv("TEST_POSTGRES_READONLY_DSN", ru.String())
			reader := ru.User.Username()
			if strings.ContainsAny(reader, `";`) {
				t.Fatal("invalid reader fixture role")
			}
			if err := admin.Exec("GRANT USAGE ON SCHEMA " + schema + " TO " + reader + ";GRANT SELECT ON ALL TABLES IN SCHEMA " + schema + " TO " + reader).Error; err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() {
			db, err := gorm.Open(postgres.Open(original), &gorm.Config{})
			if err != nil {
				t.Error(err)
				return
			}
			p, _ := db.DB()
			defer p.Close()
			if err := db.Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
				t.Error(err)
			}
		})
	}
	scope("TEST_POSTGRES_DSN", false)
	scope("TEST_CONTROL_POSTGRES_DSN", true)
	t.Setenv("ASYNC_BROKER_PREFIX", "bx.async.test."+suffix)
	for key, value := range map[string]string{"POSTGRES_PRIMARY_DSN": os.Getenv("TEST_POSTGRES_DSN"), "POSTGRES_READONLY_DSN": os.Getenv("TEST_POSTGRES_READONLY_DSN"), "ASYNC_CONTROL_DSN": os.Getenv("TEST_CONTROL_POSTGRES_DSN"), "RABBITMQ_URL": os.Getenv("TEST_RABBITMQ_URL"), "S3_ENDPOINT": os.Getenv("TEST_MINIO_ENDPOINT"), "S3_PUBLIC_ENDPOINT": os.Getenv("TEST_MINIO_ENDPOINT"), "S3_ACCESS_KEY": os.Getenv("TEST_MINIO_ACCESS_KEY"), "S3_SECRET_KEY": os.Getenv("TEST_MINIO_SECRET_KEY"), "S3_BUCKET": "async-core-test", "AI_ENABLED": "false", "CACHE_ENABLED": "false", "ETCD_ENABLED": "false", "PARSER_REMOTE_ENABLED": "false", "S3_PUBLIC_PATH_PREFIX": "", "POSTGRES_MAX_OPEN_CONNS": "4", "POSTGRES_MAX_IDLE_CONNS": "2", "ASYNC_CONTROL_MAX_OPEN_CONNS": "4", "ASYNC_CONTROL_MAX_IDLE_CONNS": "1", "ASYNC_CONTROL_CRITICAL_CONNS": "2", "ASYNC_RETRY_BASE": "100ms", "ASYNC_RETRY_CAP": "1s", "OBJECT_GC_DELAY": "100ms", "ASYNC_GC_SCAN_INTERVAL": "100ms"} {
		t.Setenv(key, value)
	}
	cfg, err := config.LoadForRole("api")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Worker.MetricsAddr = ""
	cfg.HTTP.Addr = listener.Addr().String()
	_ = listener.Close()
	storage, err := minio.New(cfg.Storage.Endpoint, &minio.Options{Creds: credentials.NewStaticV4(cfg.Storage.AccessKey, cfg.Storage.SecretKey, ""), Region: cfg.Storage.Region})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	exists, err := storage.BucketExists(ctx, cfg.Storage.Bucket)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		if err := storage.MakeBucket(ctx, cfg.Storage.Bucket, minio.MakeBucketOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := asyncjobs.EnsureLifecycle(ctx, storage, cfg.Storage.Bucket, 14, 4*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	open := func(dsn string) *gorm.DB {
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
		if err != nil {
			t.Fatal(err)
		}
		sql, _ := db.DB()
		sql.SetMaxOpenConns(4)
		t.Cleanup(func() { _ = sql.Close() })
		return db
	}
	business, control := open(os.Getenv("TEST_POSTGRES_DSN")), open(os.Getenv("TEST_CONTROL_POSTGRES_DSN"))
	s := asynccontrol.New(control)
	if protocol == "3" {
		s.ProtocolVersion = 3
	}
	s.RetryBase = 100 * time.Millisecond
	s.RetryCap = time.Second
	if err := asyncjobs.CheckImport(ctx, business, s); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	logs := slog.New(slog.NewTextHandler(os.Stderr, nil))
	start := func(role string, fn func(context.Context, config.Config, *slog.Logger) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := cfg
			local.Role = role
			local.App.InstanceID = role + "-" + uuid.NewString()
			if err := fn(ctx, local, logs); err != nil && ctx.Err() == nil {
				t.Errorf("%s: %v", role, err)
			}
		}()
	}
	defer func() { cancel(); wg.Wait() }()
	// Prove API admission works with both unavailable/empty background dependencies.
	apiCfg := cfg
	apiCfg.Async.ControlDSN = ""
	apiCfg.TaskQueue.URL = "amqp://127.0.0.1:1/"
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := RunAPI(ctx, apiCfg, logs); err != nil && ctx.Err() == nil {
			t.Errorf("api: %v", err)
		}
	}()
	client := &http.Client{Timeout: 5 * time.Second}
	base := "http://" + cfg.HTTP.Addr
	token := ""
	shareAccess := ""
	request := func(method, path string, body any, status int) map[string]any {
		t.Helper()
		var reader io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			reader = bytes.NewReader(b)
		}
		req, _ := http.NewRequestWithContext(ctx, method, base+path, reader)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("If-Match", "1")
		if shareAccess != "" {
			req.Header.Set("X-Share-Token", shareAccess)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Idempotency-Key", uuid.NewString())
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		b, _ := io.ReadAll(response.Body)
		if response.StatusCode != status {
			t.Fatalf("%s %s: status %d: %s", method, path, response.StatusCode, b)
		}
		var result map[string]any
		_ = json.Unmarshal(b, &result)
		return result
	}
	await := func(label string, fn func() bool) {
		t.Helper()
		until := time.Now().Add(35 * time.Second)
		for time.Now().Before(until) {
			if fn() {
				return
			}
			if ctx.Err() != nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("timed out: %s", label)
	}
	await("api", func() bool {
		response, err := client.Get(base + "/health/live")
		if err != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == 200
	})
	username := "async_" + uuid.NewString()[:8]
	password := "async-smoke-password"
	reg := request("POST", "/api/v1/auth/register", map[string]any{"username": username, "password": password}, 201)
	userID := reg["user"].(map[string]any)["id"].(string)
	token = request("POST", "/api/v1/auth/login", map[string]any{"login": username, "password": password}, 200)["access_token"].(string)
	root := request("GET", "/api/v1/folders/root", nil, 200)["id"].(string)
	request("POST", "/api/v1/search", map[string]any{"query": "disabled", "mode": "hybrid"}, 503)
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 15), uint8(y * 15), 100, 255})
		}
	}
	seed := uuid.New()
	img.Set(0, 0, color.RGBA{seed[0], seed[1], seed[2], 255})
	img.Set(1, 0, color.RGBA{seed[3], seed[4], seed[5], 255})
	var pngData bytes.Buffer
	_ = png.Encode(&pngData, img)
	content := pngData.Bytes()
	hash := sha256.Sum256(content)
	declared := hex.EncodeToString(hash[:])
	created := request("POST", "/api/v1/uploads", map[string]any{"folder_id": root, "filename": "real.png", "sha256": declared, "size_bytes": len(content), "mime_type": "image/png"}, 201)["upload"].(map[string]any)
	sessionID := created["id"].(string)
	var admitted int64
	_ = business.Table("outbox_events").Where("aggregate_id=?", sessionID).Count(&admitted).Error
	if admitted < 1 {
		t.Fatal("API did not commit event without background services")
	}
	t.Log("API registration/login/upload admission succeeded with unavailable MQ/control settings")
	for _, role := range []string{"async-control", "upload-worker", "object-worker", "media-worker", "gc-worker"} {
		role := role
		start(role, func(c context.Context, cfg config.Config, l *slog.Logger) error { return RunAsync(c, cfg, l, role) })
	}
	await("multipart initialized", func() bool { return request("GET", "/api/v1/uploads/"+sessionID, nil, 200)["status"] == "created" })
	signed := request("POST", "/api/v1/uploads/"+sessionID+"/parts/presign", map[string]any{"part_numbers": []int{1}}, 200)["parts"].([]any)[0].(map[string]any)["url"].(string)
	put, _ := http.NewRequestWithContext(ctx, "PUT", signed, bytes.NewReader(content))
	response, err := client.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("real multipart upload", response.StatusCode)
	}
	etag := strings.Trim(response.Header.Get("ETag"), "\"")
	request("POST", "/api/v1/uploads/"+sessionID+"/parts/confirm", map[string]any{"parts": []map[string]any{{"part_number": 1, "etag": etag, "size_bytes": len(content)}}}, 200)
	request("POST", "/api/v1/uploads/"+sessionID+"/complete", nil, 202)
	var finished map[string]any
	await("file completion", func() bool {
		finished = request("GET", "/api/v1/uploads/"+sessionID, nil, 200)
		return finished["status"] == "completed"
	})
	fileID := finished["completed_entry_id"].(string)
	download := request("GET", "/api/v1/files/"+fileID+"/download-url", nil, 200)["url"].(string)
	response, err = client.Get(download)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !bytes.Equal(downloaded, content) {
		t.Fatal("download content mismatch")
	}
	objectID := finished["completed_object_id"].(string)
	await("thumbnail", func() bool {
		var count int64
		_ = business.Table("object_variants").Where("object_id=? AND status='ready'", objectID).Count(&count).Error
		return count == 1
	})
	await("staging cleanup", func() bool {
		var op struct{ State string }
		_ = business.Table("storage_operations").Select("state").Where("upload_session_id=? AND operation_type='staging.delete'", sessionID).Scan(&op).Error
		return op.State == "succeeded"
	})
	var files int64
	_ = business.Table("file_entries").Where("owner_id=? AND object_id=? AND deleted_at IS NULL", userID, objectID).Count(&files).Error
	if files != 1 {
		t.Fatal("duplicate result created files", files)
	}
	quotaResult := request("GET", "/api/v1/users/me/quota", nil, 200)
	if quotaResult["reserved_bytes"] != float64(0) || quotaResult["used_logical_bytes"] != float64(len(content)) {
		t.Fatal("quota", quotaResult)
	}
	t.Log("real multipart -> merge -> verify -> copy -> file/download -> thumbnail -> staging cleanup passed")
	// AI split publication is exercised by TestAISplitRealDependencies.
	// Reference and quota regression through the public share-import API.
	ownerToken := token
	share := request("POST", "/api/v1/shares", map[string]any{"file_id": fileID}, 201)
	shareID := share["share"].(map[string]any)["id"].(string)
	shareAccess = request("POST", "/api/v1/public/shares/resolve", map[string]any{"share_key": share["share_key"]}, 200)["share_access_token"].(string)
	other := request("POST", "/api/v1/auth/register", map[string]any{"username": "import_" + uuid.NewString()[:8], "password": password}, 201)
	token = other["access_token"].(string)
	otherRoot := request("GET", "/api/v1/folders/root", nil, 200)["id"].(string)
	imported := request("POST", "/api/v1/shares/"+shareID+"/import", map[string]any{"target_folder_id": otherRoot}, 201)["imported_file"].(map[string]any)
	var references int64
	_ = business.Table("file_objects").Select("reference_count").Where("id=?", objectID).Scan(&references).Error
	if references != 2 {
		t.Fatal("share import reference count", references)
	}
	otherQuota := request("GET", "/api/v1/users/me/quota", nil, 200)
	if otherQuota["used_logical_bytes"] != float64(len(content)) {
		t.Fatal("share import quota", otherQuota)
	}
	otherToken := token
	token = ownerToken
	// Hash mismatch must fail once, release only its reservation, and preserve
	// the canonical bytes shared by both owners.
	bad := request("POST", "/api/v1/uploads", map[string]any{"folder_id": root, "filename": "bad.png", "sha256": strings.Repeat("a", 64), "size_bytes": len(content), "mime_type": "image/png"}, 201)["upload"].(map[string]any)["id"].(string)
	await("bad upload initialized", func() bool { return request("GET", "/api/v1/uploads/"+bad, nil, 200)["status"] == "created" })
	url := request("POST", "/api/v1/uploads/"+bad+"/parts/presign", map[string]any{"part_numbers": []int{1}}, 200)["parts"].([]any)[0].(map[string]any)["url"].(string)
	partReq, _ := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewReader(content))
	partResponse, err := client.Do(partReq)
	if err != nil {
		t.Fatal(err)
	}
	partResponse.Body.Close()
	request("POST", "/api/v1/uploads/"+bad+"/parts/confirm", map[string]any{"parts": []map[string]any{{"part_number": 1, "etag": strings.Trim(partResponse.Header.Get("ETag"), "\""), "size_bytes": len(content)}}}, 200)
	request("POST", "/api/v1/uploads/"+bad+"/complete", nil, 202)
	await("hash mismatch convergence", func() bool { return request("GET", "/api/v1/uploads/"+bad, nil, 200)["status"] == "failed" })
	q := request("GET", "/api/v1/users/me/quota", nil, 200)
	if q["reserved_bytes"] != float64(0) || q["used_logical_bytes"] != float64(len(content)) {
		t.Fatal("failure quota", q)
	}
	// Cancel can race multipart creation; any late upload ID gets an abort intent.
	cancelled := request("POST", "/api/v1/uploads", map[string]any{"folder_id": root, "filename": "cancel.bin", "mime_type": "application/octet-stream", "sha256": strings.Repeat("b", 64), "size_bytes": 1}, 201)["upload"].(map[string]any)["id"].(string)
	request("DELETE", "/api/v1/uploads/"+cancelled, nil, 204)
	await("cancelled reservation released", func() bool {
		var reserved int64
		_ = business.Table("upload_sessions").Select("reserved_bytes").Where("id=?", cancelled).Scan(&reserved).Error
		return reserved == 0
	})
	// Move durable test timestamps instead of waiting a real 24-hour TTL. First
	// force an early notification and verify the command worker honors a later expiry.
	expired := request("POST", "/api/v1/uploads", map[string]any{"folder_id": root, "filename": "expiry.bin", "mime_type": "application/octet-stream", "sha256": strings.Repeat("c", 64), "size_bytes": 1}, 201)["upload"].(map[string]any)["id"].(string)
	expiryKey := "upload.expire:" + expired
	await("expiry task registered", func() bool {
		var count int64
		_ = control.Table("tasks").Where("semantic_key=?", expiryKey).Count(&count).Error
		return count == 1
	})
	expirySQL := "WITH changed AS (UPDATE tasks SET not_before=clock_timestamp() WHERE semantic_key=? RETURNING id) UPDATE task_deliveries SET not_before=clock_timestamp() WHERE task_id IN (SELECT id FROM changed)"
	if protocol == "3" {
		expirySQL = "UPDATE tasks SET not_before=clock_timestamp(),dispatch_not_before=clock_timestamp() WHERE semantic_key=?"
	}
	if err := control.Exec(expirySQL, expiryKey).Error; err != nil {
		t.Fatal(err)
	}
	await("early expiry rescheduled", func() bool {
		var generation int64
		column := "commit_attempts"
		if protocol == "3" {
			column = "attempts"
		}
		_ = control.Table("tasks").Select(column).Where("semantic_key=?", expiryKey).Scan(&generation).Error
		return generation > 0
	})
	if err := business.Exec("UPDATE upload_sessions SET expires_at=clock_timestamp()-interval '1 second' WHERE id=?", expired).Error; err != nil {
		t.Fatal(err)
	}
	if err := control.Exec(expirySQL, expiryKey).Error; err != nil {
		t.Fatal(err)
	}
	await("expiry committed", func() bool { return request("GET", "/api/v1/uploads/"+expired, nil, 200)["status"] == "expired" })
	// Removing one owner must not collect shared bytes. The final release goes
	// through authorization -> GC I/O -> committed deletion.
	var canonicalKey string
	_ = business.Table("file_objects").Select("object_key").Where("id=?", objectID).Scan(&canonicalKey).Error
	request("DELETE", "/api/v1/files/"+fileID, nil, 204)
	var status string
	_ = business.Table("file_objects").Select("status").Where("id=?", objectID).Scan(&status).Error
	if status != "ready" {
		t.Fatal("shared object collected", status)
	}
	token = otherToken
	request("DELETE", "/api/v1/files/"+imported["id"].(string), nil, 204)
	await("authorized object GC", func() bool {
		_ = business.Table("file_objects").Select("status").Where("id=?", objectID).Scan(&status).Error
		return status == "deleted"
	})
	if _, err := storage.StatObject(ctx, cfg.Storage.Bucket, canonicalKey, minio.StatObjectOptions{}); err == nil {
		t.Fatal("GC retained authorized object bytes")
	}
	t.Log("share import/reference/quota, hash mismatch, cancellation, expiry/rescheduling and authorized shared-object GC passed")
}

func TestAsyncCoreDirectRealDependencies(t *testing.T) {
	t.Setenv("TEST_ASYNC_PROTOCOL_VERSION", "3")
	TestAsyncCoreRealDependencies(t)
}
