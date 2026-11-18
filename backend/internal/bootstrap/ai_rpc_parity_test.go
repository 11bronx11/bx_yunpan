package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pbv2"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/grpcx"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/registry"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/status"
)

func TestInternalClientsKeepResolversLocal(t *testing.T) {
	before := resolver.Get(registry.Scheme)
	cfg := config.Config{Registry: config.Registry{Enabled: true, Endpoints: []string{"127.0.0.1:1"}, DialTimeout: time.Second}}
	var wg sync.WaitGroup
	errors := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, cleanup, err := internalClient(cfg, "ai-query-v2", "", "fixture")
			if err == nil {
				_ = conn.Close()
			}
			cleanup()
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if resolver.Get(registry.Scheme) != before {
		t.Fatal("client changed the global resolver or borrowed another connection's lifecycle")
	}
}

type rpcParityService struct {
	pbv2.UnimplementedAIQueryServiceServer
	reads, models atomic.Int32
	mu            sync.Mutex
	parent        trace.TraceID
	requestID     string
}

func (s *rpcParityService) Search(ctx context.Context, r *pbv2.SearchRequest) (*pbv2.SearchResponse, error) {
	if r.Query == "panic" {
		panic("fixture panic")
	}
	if r.Query == "deadline" {
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	if r.Mode != "fulltext" {
		s.models.Add(1)
		return nil, status.Error(codes.Unavailable, "model fixture")
	}
	if s.reads.Add(1) == 1 {
		return nil, status.Error(codes.Unavailable, "read fixture")
	}
	s.mu.Lock()
	s.parent = trace.SpanContextFromContext(ctx).TraceID()
	s.requestID = grpcx.RequestID(ctx)
	s.mu.Unlock()
	return &pbv2.SearchResponse{}, nil
}

func (s *rpcParityService) Ask(context.Context, *pbv2.AskRequest) (*pbv2.AskResponse, error) {
	s.models.Add(1)
	return nil, status.Error(codes.Unavailable, "answer fixture")
}

// Real sockets and SDK exporter verify the actual split bootstrap wiring.
func TestInternalRPCTraceGovernance(t *testing.T) {
	oldProvider, oldPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	defer func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(oldProvider)
		otel.SetTextMapPropagator(oldPropagator)
	}()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	_ = l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := prometheus.NewRegistry()
	ctx = context.WithValue(ctx, splitRegistryKey{}, registry)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := &rpcParityService{}
	cfg := config.Config{AISplit: config.AISplit{QueryTimeout: 100 * time.Millisecond, AskTimeout: time.Second}, AIService: config.AIService{SearchMaxAttempts: 2, RetryBaseBackoff: time.Millisecond}}
	done := make(chan error, 1)
	go func() {
		done <- serveInternal(ctx, cfg, logger, "ai-query-v2", address, address, "fixture-token", "/yunpan.ai.v2.AIQueryService/", func(s *grpc.Server) { pbv2.RegisterAIQueryServiceServer(s, svc) })
	}()
	defer func() {
		cancel()
		if e := <-done; e != nil {
			t.Error(e)
		}
	}()
	conn, cleanup, e := internalClient(cfg, "ai-query-v2", address, "fixture-token", grpcx.NewMetrics(registry, "client"))
	if e != nil {
		t.Fatal(e)
	}
	defer cleanup()
	defer conn.Close()
	client := pbv2.NewAIQueryServiceClient(conn)
	readyCtx, readyCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readyCancel()
	conn.Connect()
	for state := conn.GetState(); state != connectivity.Ready; state = conn.GetState() {
		if !conn.WaitForStateChange(readyCtx, state) {
			t.Fatal("RPC connection did not become ready", readyCtx.Err())
		}
	}
	_, e = healthpb.NewHealthClient(conn).Check(readyCtx, &healthpb.HealthCheckRequest{Service: pbv2.AIQueryService_ServiceDesc.ServiceName})
	if e != nil {
		t.Fatal("RPC did not become ready", e)
	}
	rootCtx, root := otel.Tracer("parity").Start(grpcx.WithRequestID(ctx, "parity-request"), "http.test")
	_, e = client.Search(rootCtx, &pbv2.SearchRequest{Query: "read", Mode: "fulltext"})
	root.End()
	svc.mu.Lock()
	receivedID, receivedParent := svc.requestID, svc.parent
	svc.mu.Unlock()
	if e != nil || svc.reads.Load() != 2 || receivedID != "parity-request" || receivedParent != root.SpanContext().TraceID() {
		t.Fatal("read retry or propagation broken", e, svc.reads.Load(), receivedID, receivedParent)
	}
	for _, mode := range []string{"semantic", "hybrid"} {
		_, _ = client.Search(ctx, &pbv2.SearchRequest{Query: "model", Mode: mode})
	}
	_, _ = client.Ask(ctx, &pbv2.AskRequest{Question: "fixture"})
	if svc.models.Load() != 3 {
		t.Fatal("model RPC replayed", svc.models.Load())
	}
	_, e = client.Search(ctx, &pbv2.SearchRequest{Query: "panic", Mode: "fulltext"})
	if status.Code(e) != codes.Internal {
		t.Fatal("panic not recovered", e)
	}
	start := time.Now()
	_, e = client.Search(ctx, &pbv2.SearchRequest{Query: "deadline", Mode: "fulltext"})
	if status.Code(e) != codes.DeadlineExceeded || time.Since(start) > time.Second {
		t.Fatal("RPC deadline not bounded", e, time.Since(start))
	}
	bad, closeBad, e := internalClient(cfg, "ai-query-v2", address, "wrong-token")
	if e != nil {
		t.Fatal(e)
	}
	defer bad.Close()
	defer closeBad()
	_, e = pbv2.NewAIQueryServiceClient(bad).Search(ctx, &pbv2.SearchRequest{Mode: "fulltext"})
	if status.Code(e) != codes.Unauthenticated {
		t.Fatal("authentication lost", e)
	}
	families, e := registry.Gather()
	if e != nil {
		t.Fatal(e)
	}
	var metrics []string
	for _, f := range families {
		metrics = append(metrics, f.GetName())
	}
	if !strings.Contains(strings.Join(metrics, ","), "yunpan_grpc_server_handled_total") || !strings.Contains(strings.Join(metrics, ","), "yunpan_grpc_client_handled_total") {
		t.Fatal("RPC metrics not registered", metrics)
	}
	var clientSpan, serverSpan bool
	for _, span := range exporter.GetSpans() {
		if span.SpanContext.TraceID() == root.SpanContext().TraceID() {
			clientSpan = clientSpan || span.SpanKind == trace.SpanKindClient
			serverSpan = serverSpan || span.SpanKind == trace.SpanKindServer
		}
	}
	if !clientSpan || !serverSpan {
		t.Fatal("distributed trace missing client/server spans")
	}
}
