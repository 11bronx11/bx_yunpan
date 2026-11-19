package grpcx

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pbv2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type askDeadlineQuery struct {
	pbv2.UnimplementedAIQueryServiceServer
}

func (*askDeadlineQuery) Ask(ctx context.Context, _ *pbv2.AskRequest) (*pbv2.AskResponse, error) {
	deadline, _ := ctx.Deadline()
	return &pbv2.AskResponse{Answer: fmt.Sprint(time.Until(deadline).Milliseconds())}, nil
}

func (*askDeadlineQuery) Search(ctx context.Context, _ *pbv2.SearchRequest) (*pbv2.SearchResponse, error) {
	deadline, _ := ctx.Deadline()
	return &pbv2.SearchResponse{Coverage: &pbv2.Coverage{Mode: fmt.Sprint(time.Until(deadline).Milliseconds())}}, nil
}

func TestSelectedAskDeadlinePassesClientInterceptor(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	pbv2.RegisterAIQueryServiceServer(server, &askDeadlineQuery{})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	options := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() })}
	options = append(options, ClientInterceptors(ClientOptions{Timeout: 2 * time.Second, AskTimeout: time.Second})...)
	conn, err := grpc.NewClient("passthrough:///ask-budget-test", options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := pbv2.NewAIQueryServiceClient(conn)
	for _, tc := range []struct {
		name                      string
		override, outer, expected time.Duration
	}{
		{"ordinary", 0, time.Minute, time.Second},
		{"selected", 5 * time.Second, time.Minute, 5 * time.Second},
		{"caller_cancellation", 5 * time.Second, time.Second, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), tc.outer)
			defer cancel()
			ctx = WithAskTimeout(ctx, tc.override)
			out, err := client.Ask(ctx, &pbv2.AskRequest{})
			if err != nil {
				t.Fatal(err)
			}
			millis, err := strconv.ParseInt(out.Answer, 10, 64)
			remaining := time.Duration(millis) * time.Millisecond
			if err != nil || remaining > tc.expected || remaining < tc.expected-500*time.Millisecond {
				t.Fatalf("remaining=%s, expected %s: %v", remaining, tc.expected, err)
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := client.Search(WithAskTimeout(ctx, 5*time.Second), &pbv2.SearchRequest{Mode: "fulltext"})
	if err != nil {
		t.Fatal(err)
	}
	millis, err := strconv.ParseInt(out.Coverage.Mode, 10, 64)
	if err != nil || millis > 2000 || millis < 1500 {
		t.Fatalf("Ask override changed Search deadline: %s, %v", out.Coverage.Mode, err)
	}
}
