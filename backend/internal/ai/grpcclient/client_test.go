package grpcclient

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pb"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/breaker"
)

// stubServer 用可控的返回值模拟 aisvc，并统计每个方法被调用的次数。
type stubServer struct {
	pb.UnimplementedAIServiceServer

	searchCalls    atomic.Int32
	askCalls       atomic.Int32
	reprocessCalls atomic.Int32

	// failFirst 指定前 N 次调用返回 err，之后成功。
	failFirst int32
	err       error
	// hitsBeforeError 为正时，Search 先推送这么多条消息再报错，
	// 用来验证"已经推过消息就不重试"。
	hitsBeforeError int
}

func (s *stubServer) Search(request *pb.SearchRequest, stream pb.AIService_SearchServer) error {
	attempt := s.searchCalls.Add(1)
	for range s.hitsBeforeError {
		if err := stream.Send(&pb.SearchHit{MatchType: "hybrid", File: &pb.FileRef{Id: uuid.New().String()}}); err != nil {
			return err
		}
	}
	if attempt <= s.failFirst {
		return s.err
	}
	return stream.Send(&pb.SearchHit{MatchType: "hybrid", Score: 1, File: &pb.FileRef{Id: uuid.New().String()}})
}

func (s *stubServer) Ask(context.Context, *pb.AskRequest) (*pb.AskResponse, error) {
	if s.askCalls.Add(1) <= s.failFirst {
		return nil, s.err
	}
	return &pb.AskResponse{Answer: "ok"}, nil
}

func (s *stubServer) Reprocess(context.Context, *pb.ReprocessRequest) (*pb.ReprocessResponse, error) {
	if s.reprocessCalls.Add(1) <= s.failFirst {
		return nil, s.err
	}
	return &pb.ReprocessResponse{Task: &pb.Task{Id: uuid.New().String(), Status: "pending"}}, nil
}

func newTestClient(t *testing.T, stub *stubServer, config Config) *Client {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	pb.RegisterAIServiceServer(server, stub)
	go func() { _ = server.Serve(listener) }()

	config.Target = "passthrough:///bufnet"
	config.DialOptions = append(config.DialOptions, grpc.WithContextDialer(
		func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) },
	))
	if config.CallTimeout == 0 {
		config.CallTimeout = 5 * time.Second
	}
	client, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		server.Stop()
	})
	return client
}

func retryPolicy(attempts int) RetryPolicy {
	return RetryPolicy{MaxAttempts: attempts, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}
}

// Ask 非幂等且有 LLM 成本，必须只调一次。
func TestAskIsNotRetried(t *testing.T) {
	stub := &stubServer{failFirst: 5, err: status.Error(codes.Unavailable, "down")}
	client := newTestClient(t, stub, Config{AskRetry: retryPolicy(1)})

	_, _, err := client.Ask(context.Background(), uuid.New(), "问题", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ai.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if calls := stub.askCalls.Load(); calls != 1 {
		t.Fatalf("Ask called %d times, want exactly 1", calls)
	}
}

// Search 只读可重试：前两次 Unavailable，第三次成功。
func TestSearchRetriesOnUnavailable(t *testing.T) {
	stub := &stubServer{failFirst: 2, err: status.Error(codes.Unavailable, "down")}
	client := newTestClient(t, stub, Config{SearchRetry: retryPolicy(3)})

	hits, err := client.Search(context.Background(), uuid.New(), ai.SearchInput{Query: "q", Mode: "hybrid"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	if calls := stub.searchCalls.Load(); calls != 3 {
		t.Fatalf("Search called %d times, want 3", calls)
	}
}

// 已经把消息交给调用方后再失败不能重试，否则结果会重复。
func TestSearchDoesNotRetryAfterPartialStream(t *testing.T) {
	stub := &stubServer{failFirst: 5, err: status.Error(codes.Unavailable, "down"), hitsBeforeError: 2}
	client := newTestClient(t, stub, Config{SearchRetry: retryPolicy(3)})

	if _, err := client.Search(context.Background(), uuid.New(), ai.SearchInput{Query: "q", Mode: "hybrid"}); err == nil {
		t.Fatal("expected error")
	}
	if calls := stub.searchCalls.Load(); calls != 1 {
		t.Fatalf("Search called %d times, want exactly 1", calls)
	}
}

// InvalidArgument 之类的非瞬时错误重试没有意义。
func TestSearchDoesNotRetryInvalidArgument(t *testing.T) {
	stub := &stubServer{failFirst: 5, err: status.Error(codes.InvalidArgument, "bad")}
	client := newTestClient(t, stub, Config{SearchRetry: retryPolicy(3)})

	_, err := client.Search(context.Background(), uuid.New(), ai.SearchInput{Query: "", Mode: "hybrid"})
	if !errors.Is(err, ai.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if calls := stub.searchCalls.Load(); calls != 1 {
		t.Fatalf("Search called %d times, want 1", calls)
	}
}

// Reprocess 靠 Pipeline/Model Version 与 dedupe_key 幂等，可重试。
func TestReprocessRetries(t *testing.T) {
	stub := &stubServer{failFirst: 1, err: status.Error(codes.Unavailable, "down")}
	client := newTestClient(t, stub, Config{ReprocessRetry: retryPolicy(3)})

	task, err := client.RequestReprocess(context.Background(), uuid.New(), uuid.New(), "key")
	if err != nil {
		t.Fatalf("RequestReprocess: %v", err)
	}
	if task.Status != "pending" {
		t.Fatalf("status = %s", task.Status)
	}
	if calls := stub.reprocessCalls.Load(); calls != 2 {
		t.Fatalf("Reprocess called %d times, want 2", calls)
	}
}

// 熔断打开后请求不再打到下游。
func TestBreakerShortCircuitsAfterFailures(t *testing.T) {
	stub := &stubServer{failFirst: 1000, err: status.Error(codes.Unavailable, "down")}
	client := newTestClient(t, stub, Config{
		SearchRetry: retryPolicy(1),
		Breaker: breaker.Config{
			Window: time.Minute, Buckets: 4, MinRequests: 3, FailureRate: 0.5,
			OpenTimeout: time.Minute, HalfOpenProbes: 1,
		},
	})

	for range 3 {
		_, _ = client.Search(context.Background(), uuid.New(), ai.SearchInput{Query: "q", Mode: "hybrid"})
	}
	before := stub.searchCalls.Load()
	if _, err := client.Search(context.Background(), uuid.New(), ai.SearchInput{Query: "q", Mode: "hybrid"}); err == nil {
		t.Fatal("expected error once breaker is open")
	}
	if after := stub.searchCalls.Load(); after != before {
		t.Fatalf("breaker open but downstream called: %d -> %d", before, after)
	}
}

// 限流与配额不代表 aisvc 不健康，不应计入熔断失败率。
func TestRateLimitDoesNotTripBreaker(t *testing.T) {
	stub := &stubServer{failFirst: 1000, err: ai.GRPCStatus(ai.ErrRateLimited).Err()}
	client := newTestClient(t, stub, Config{
		SearchRetry: retryPolicy(1),
		Breaker: breaker.Config{
			Window: time.Minute, Buckets: 4, MinRequests: 3, FailureRate: 0.5,
			OpenTimeout: time.Minute, HalfOpenProbes: 1,
		},
	})

	for range 5 {
		_, err := client.Search(context.Background(), uuid.New(), ai.SearchInput{Query: "q", Mode: "hybrid"})
		if !errors.Is(err, ai.ErrRateLimited) {
			t.Fatalf("err = %v, want ErrRateLimited", err)
		}
	}
	// 5 次限流后仍应放行到下游，说明没被计入失败率。
	before := stub.searchCalls.Load()
	_, _ = client.Search(context.Background(), uuid.New(), ai.SearchInput{Query: "q", Mode: "hybrid"})
	if after := stub.searchCalls.Load(); after == before {
		t.Fatal("breaker tripped on rate limit responses")
	}
}
