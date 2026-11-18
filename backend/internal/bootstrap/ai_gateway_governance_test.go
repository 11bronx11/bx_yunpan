package bootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pbv2"
	"github.com/11bronx11/bx_yunpan/backend/internal/aigateway"
	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dependencies"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/grpcx"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/httpapi"
	"github.com/11bronx11/bx_yunpan/backend/internal/testdb"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	redis "github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/status"
)

// Transport failures are injected after the real HTTP gateway and authenticated
// gRPC server. Full retrieval/authorization is covered by the split-chain test.
type gatewayRPCProbe struct {
	pbv2.UnimplementedAIQueryServiceServer
	calls    atomic.Int32
	fail     atomic.Bool
	first    atomic.Bool
	code     codes.Code
	failure  error
	block    bool
	entered  chan struct{}
	canceled chan struct{}
}

func (s *gatewayRPCProbe) call(ctx context.Context) error {
	s.calls.Add(1)
	if s.block {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		select {
		case s.canceled <- struct{}{}:
		default:
		}
		return status.FromContextError(ctx.Err()).Err()
	}
	if s.fail.Load() || s.first.CompareAndSwap(true, false) {
		if s.failure != nil {
			return s.failure
		}
		return status.Error(s.code, "controlled online fault")
	}
	return nil
}

func (s *gatewayRPCProbe) Search(ctx context.Context, _ *pbv2.SearchRequest) (*pbv2.SearchResponse, error) {
	if err := s.call(ctx); err != nil {
		return nil, err
	}
	return &pbv2.SearchResponse{}, nil
}

func (s *gatewayRPCProbe) Ask(ctx context.Context, _ *pbv2.AskRequest) (*pbv2.AskResponse, error) {
	if err := s.call(ctx); err != nil {
		return nil, err
	}
	return &pbv2.AskResponse{Answer: "controlled answer"}, nil
}

type gatewayProbeResponse struct {
	Code int
	Body map[string]any
	Err  error
}
type gatewayProbeRequest func(context.Context, string, string) gatewayProbeResponse

func onlineGateway(t *testing.T, svc *gatewayRPCProbe, options func(*config.Config), limiter ai.RequestLimiter) gatewayProbeRequest {
	t.Helper()
	db := testdb.Business(t)
	owner := identity.User{ID: uuid.New(), Username: uuid.NewString(), PasswordHash: "fixture", Status: 1}
	owner.UsernameNormalized = owner.Username
	if err := db.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	if err := drive.NewService(db).ProvisionUser(db, owner.ID); err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	_ = l.Close()
	cfg := config.Config{AISplit: config.AISplit{QueryTimeout: time.Second, AskTimeout: time.Second}, AIService: config.AIService{SearchMaxAttempts: 3, RetryBaseBackoff: time.Millisecond, RetryMaxBackoff: 2 * time.Millisecond, BreakerMinRequests: 100, BreakerOpenTimeout: 150 * time.Millisecond, BreakerHalfOpenProbes: 2}}
	if options != nil {
		options(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	done := make(chan error, 1)
	go func() {
		done <- serveInternal(ctx, cfg, logger, "ai-query-v2", address, address, "online-token", "/yunpan.ai.v2.AIQueryService/", func(s *grpc.Server) { pbv2.RegisterAIQueryServiceServer(s, svc) })
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	conn, cleanup, err := internalClient(cfg, "ai-query-v2", address, "online-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); cleanup() })
	ready, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	conn.Connect()
	for state := conn.GetState(); state != connectivity.Ready; state = conn.GetState() {
		if !conn.WaitForStateChange(ready, state) {
			t.Fatal("online query socket not ready")
		}
	}
	h := &aigateway.HTTP{DB: db, Client: pbv2.NewAIQueryServiceClient(conn), ScopeKey: key, Enabled: true, Limiter: limiter, QueryTimeout: time.Second, AskTimeout: time.Second, Authenticate: func(c *gin.Context) { httpapi.SetPrincipalID(c, owner.ID) }}
	router := gin.New()
	h.RegisterRoutes(router.Group("/api/v1"))
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return func(ctx context.Context, mode, query string) gatewayProbeResponse {
		path, body := "/api/v1/search", map[string]any{"query": query, "mode": mode}
		if mode == "ask" {
			path, body = "/api/v1/ai/ask", map[string]any{"question": query}
		}
		payload, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+path, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		res, err := server.Client().Do(req)
		if err != nil {
			return gatewayProbeResponse{Err: err}
		}
		defer res.Body.Close()
		var out map[string]any
		err = json.NewDecoder(res.Body).Decode(&out)
		return gatewayProbeResponse{res.StatusCode, out, err}
	}
}

func TestAIGatewayRetryAndDeadline(t *testing.T) {
	for _, mode := range []string{"fulltext", "semantic", "hybrid", "ask"} {
		t.Run(mode, func(t *testing.T) {
			svc := &gatewayRPCProbe{code: codes.Unavailable}
			svc.first.Store(true)
			req := onlineGateway(t, svc, nil, nil)
			out := req(context.Background(), mode, "rollback")
			want, calls := 503, int32(1)
			if mode == "fulltext" {
				want, calls = 200, 2
			}
			if out.Err != nil || out.Code != want || svc.calls.Load() != calls {
				t.Fatalf("HTTP=%d calls=%d err=%v want HTTP=%d calls=%d", out.Code, svc.calls.Load(), out.Err, want, calls)
			}
		})
	}
	t.Run("read-retry-exhausted", func(t *testing.T) {
		svc := &gatewayRPCProbe{code: codes.Unavailable}
		svc.fail.Store(true)
		req := onlineGateway(t, svc, nil, nil)
		if out := req(context.Background(), "fulltext", "rollback"); out.Err != nil || out.Code != 503 || svc.calls.Load() != 3 {
			t.Fatal("read retry budget changed", out, svc.calls.Load())
		}
	})
	t.Run("retry-backoff-shares-deadline", func(t *testing.T) {
		svc := &gatewayRPCProbe{code: codes.Unavailable}
		svc.fail.Store(true)
		req := onlineGateway(t, svc, func(c *config.Config) {
			c.AISplit.QueryTimeout = 30 * time.Millisecond
			c.AIService.RetryBaseBackoff = 100 * time.Millisecond
			c.AIService.RetryMaxBackoff = time.Second
		}, nil)
		start := time.Now()
		if out := req(context.Background(), "fulltext", "rollback"); out.Err != nil || out.Code != 504 || svc.calls.Load() != 1 || time.Since(start) > time.Second {
			t.Fatal("retry backoff exceeded the total budget", out, svc.calls.Load(), time.Since(start))
		}
	})
	t.Run("total-deadline", func(t *testing.T) {
		svc := &gatewayRPCProbe{block: true, entered: make(chan struct{}, 1), canceled: make(chan struct{}, 1)}
		req := onlineGateway(t, svc, func(c *config.Config) { c.AISplit.QueryTimeout = 80 * time.Millisecond }, nil)
		start := time.Now()
		out := req(context.Background(), "fulltext", "rollback")
		if out.Err != nil || out.Code != 504 || out.Body["error"].(map[string]any)["code"] != "ai.query_timeout" || svc.calls.Load() != 1 || time.Since(start) > time.Second {
			t.Fatalf("deadline/replay changed: out=%+v calls=%d duration=%s", out, svc.calls.Load(), time.Since(start))
		}
	})
	t.Run("client-cancellation", func(t *testing.T) {
		svc := &gatewayRPCProbe{block: true, entered: make(chan struct{}, 1), canceled: make(chan struct{}, 1)}
		req := onlineGateway(t, svc, nil, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan gatewayProbeResponse, 1)
		go func() { done <- req(ctx, "ask", "rollback") }()
		select {
		case <-svc.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("Ask did not reach server")
		}
		cancel()
		select {
		case <-svc.canceled:
		case <-time.After(time.Second):
			t.Fatal("HTTP cancellation did not reach query")
		}
		if out := <-done; out.Err == nil || svc.calls.Load() != 1 {
			t.Fatal("canceled Ask replayed or returned success", out, svc.calls.Load())
		}
	})
}

func TestAIGatewayCircuitIsolationAndRecovery(t *testing.T) {
	svc := &gatewayRPCProbe{code: codes.Unavailable}
	svc.fail.Store(true)
	req := onlineGateway(t, svc, func(c *config.Config) { c.AIService.SearchMaxAttempts = 1; c.AIService.BreakerMinRequests = 2 }, nil)
	for _, mode := range []string{"ask", "semantic", "hybrid", "fulltext"} {
		t.Run(mode, func(t *testing.T) {
			before := svc.calls.Load()
			for range 2 {
				if out := req(context.Background(), mode, "rollback"); out.Code != 503 {
					t.Fatal(out)
				}
			}
			if out := req(context.Background(), mode, "rollback"); out.Code != 503 || svc.calls.Load() != before+2 {
				t.Fatal("open circuit called downstream", out, svc.calls.Load()-before)
			}
		})
	}
	svc.fail.Store(false)
	time.Sleep(180 * time.Millisecond)
	for _, mode := range []string{"ask", "semantic", "hybrid", "fulltext"} {
		before := svc.calls.Load()
		for range 3 {
			if out := req(context.Background(), mode, "rollback"); out.Code != 200 || out.Err != nil {
				t.Fatal("circuit did not recover", mode, out)
			}
		}
		if svc.calls.Load() != before+3 {
			t.Fatal("recovery did not call query", mode)
		}
	}
}

func TestAIGatewayRequestFailuresDoNotOpenCircuit(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{"scope-not-ready", grpcx.RequestUnavailable("ai.semantic_unavailable"), 503},
		{"scope-changed", status.Error(codes.Aborted, "ai.scope_changed"), 409},
		{"scope-invalid", status.Error(codes.FailedPrecondition, "ai.scope_invalid"), 409},
		{"provider-quota", ai.GRPCStatus(ai.ErrQuota).Err(), 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &gatewayRPCProbe{failure: tc.err}
			svc.fail.Store(true)
			req := onlineGateway(t, svc, func(c *config.Config) { c.AIService.BreakerMinRequests = 2 }, nil)
			for range 5 {
				if out := req(context.Background(), "fulltext", "rollback"); out.Err != nil || out.Code != tc.code {
					t.Fatal("request failure changed its public contract", out)
				}
			}
			if svc.calls.Load() != 5 {
				t.Fatal("request failure was retried or opened a shared circuit", svc.calls.Load())
			}
			svc.fail.Store(false)
			if out := req(context.Background(), "fulltext", "rollback"); out.Err != nil || out.Code != 200 || svc.calls.Load() != 6 {
				t.Fatal("request failures blocked a healthy query", out, svc.calls.Load())
			}
		})
	}
}

func TestAIGatewayConcurrentRateLimitsAndRedisFailure(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("dedicated Redis required")
	}
	r := redis.NewClient(&redis.Options{Addr: addr})
	defer r.Close()
	limiter := ai.NewRequestLimiter(r, ai.RateLimits{SearchPerMinute: 3, AskPerMinute: 2})
	svc := &gatewayRPCProbe{}
	req := onlineGateway(t, svc, nil, limiter)
	var wg sync.WaitGroup
	responses := make(chan gatewayProbeResponse, 24)
	for _, mode := range []string{"fulltext", "ask"} {
		for range 12 {
			wg.Add(1)
			go func(mode string) { defer wg.Done(); responses <- req(context.Background(), mode, "rollback") }(mode)
		}
	}
	wg.Wait()
	close(responses)
	counts := map[int]int{}
	for out := range responses {
		if out.Err != nil {
			t.Fatal(out.Err)
		}
		counts[out.Code]++
	}
	if counts[200] != 5 || counts[429] != 19 || svc.calls.Load() != 5 {
		t.Fatal("rate limit raced or reached query", counts, svc.calls.Load())
	}
	// Redis failure deliberately allows requests. Use a real failed TCP
	// connection to verify this policy separately from healthy Redis limits.
	bad := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 20 * time.Millisecond, ReadTimeout: 20 * time.Millisecond, WriteTimeout: 20 * time.Millisecond})
	defer bad.Close()
	openSvc := &gatewayRPCProbe{}
	open := onlineGateway(t, openSvc, nil, ai.NewRequestLimiter(bad, ai.RateLimits{SearchPerMinute: 1, AskPerMinute: 1}))
	for range 2 {
		if out := open(context.Background(), "ask", "rollback"); out.Code != 200 || out.Err != nil {
			t.Fatal("Redis outage did not follow fail-open policy", out)
		}
	}
	if openSvc.calls.Load() != 2 {
		t.Fatal("Redis failure blocked query")
	}
}

func TestAIGatewayRateLimitRedisWaitIsBounded(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" || os.Getenv("TEST_POSTGRES_DSN") == "" {
		t.Skip("dedicated Redis and business database required")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var connections []net.Conn
	var recovered atomic.Bool
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections = append(connections, connection)
			mu.Unlock()
			go func() {
				defer connection.Close()
				if !recovered.Load() {
					// Accept TCP and consume requests, but send no Redis reply.
					_, _ = io.Copy(io.Discard, connection)
					return
				}
				upstream, err := net.DialTimeout("tcp", addr, time.Second)
				if err != nil {
					return
				}
				defer upstream.Close()
				go func() { _, _ = io.Copy(upstream, connection); _ = upstream.Close() }()
				_, _ = io.Copy(connection, upstream)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-acceptDone
		mu.Lock()
		defer mu.Unlock()
		for _, connection := range connections {
			_ = connection.Close()
		}
	})
	// Open the same coordination client as RunAPI, including its timeout policy.
	cfg := config.Config{}
	cfg.Postgres.PrimaryDSN = os.Getenv("TEST_POSTGRES_DSN")
	cfg.Postgres.MaxOpenConns, cfg.Postgres.MaxIdleConns = 1, 1
	cfg.Redis.CoordinationAddr = listener.Addr().String()
	cfg.Storage.Endpoint, cfg.Storage.PublicEndpoint = "127.0.0.1:9000", "127.0.0.1:9000"
	cfg.Storage.AccessKey, cfg.Storage.SecretKey = "online-test", "online-test-secret"
	deps, err := dependencies.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer deps.Close()
	svc := &gatewayRPCProbe{}
	req := onlineGateway(t, svc, nil, ai.NewRequestLimiter(deps.RedisCoordination, ai.RateLimits{SearchPerMinute: 1}))
	start := time.Now()
	out := req(context.Background(), "fulltext", "rollback")
	duration := time.Since(start)
	t.Logf("unresponsive coordination Redis: HTTP=%d elapsed=%s query_calls=%d", out.Code, duration, svc.calls.Load())
	if out.Err != nil || out.Code != 200 || svc.calls.Load() != 1 || duration > time.Second {
		t.Fatal("rate limiting consumed the online query budget", out.Code, out.Err, duration)
	}
	recovered.Store(true)
	for _, want := range []int{200, 429} {
		if out := req(context.Background(), "fulltext", "rollback"); out.Err != nil || out.Code != want {
			t.Fatal("same coordination client did not resume rate limiting", out, want)
		}
	}
	if svc.calls.Load() != 2 {
		t.Fatal("recovered Redis failed to enforce the request limit", svc.calls.Load())
	}
}
