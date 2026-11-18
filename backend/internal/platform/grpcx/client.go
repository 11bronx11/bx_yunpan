package grpcx

import (
	"context"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/breaker"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type ClientOptions struct {
	StatsHandled        bool
	Timeout, AskTimeout time.Duration
	ReadAttempts        int
	RetryBase, RetryCap time.Duration
	Breaker             breaker.Config
	Metrics             *Metrics
}

// ClientInterceptors gives each RPC method its own breaker. A failed costly
// Ask must not open the fulltext circuit. Retries share a single total budget;
// only model-free reads may be replayed, never Ask or semantic/hybrid Search.
func ClientInterceptors(o ClientOptions) []grpc.DialOption {
	var mu sync.Mutex
	circuits := map[string]*breaker.Breaker{}
	interceptor := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		started := time.Now()
		timeout := o.Timeout
		if strings.HasSuffix(method, "/Ask") && o.AskTimeout > 0 {
			timeout = o.AskTimeout
		}
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		ctx = InjectRequestID(ctx)
		key := method
		attempts := 1
		if search, ok := req.(interface{ GetMode() string }); ok && strings.HasSuffix(method, "/Search") {
			mode := search.GetMode()
			if mode != "fulltext" && mode != "semantic" && mode != "hybrid" {
				mode = "invalid"
			}
			key += ":" + mode
			if mode == "fulltext" {
				attempts = max(1, o.ReadAttempts)
			}
		} else if !strings.HasSuffix(method, "/Ask") && !strings.HasSuffix(method, "/Search") {
			attempts = max(1, o.ReadAttempts)
		}
		mu.Lock()
		circuit := circuits[key]
		if circuit == nil {
			circuit = breaker.New(o.Breaker)
			circuits[key] = circuit
		}
		mu.Unlock()
		var err error
		defer func() { o.Metrics.observe(method, status.Code(err).String(), time.Since(started)) }()
		for n := 0; n < attempts; n++ {
			if ctx.Err() != nil {
				err = status.FromContextError(ctx.Err()).Err()
				return err
			}
			done, e := circuit.Allow()
			if e != nil {
				err = status.Error(codes.Unavailable, "internal RPC circuit open")
				return err
			}
			err = invoke(ctx, method, req, reply, cc, opts...)
			code := status.Code(err)
			done(code != codes.Unavailable && code != codes.DeadlineExceeded && code != codes.Internal && code != codes.Unknown)
			if err == nil || n+1 == attempts || code != codes.Unavailable {
				return err
			}
			base, cap := o.RetryBase, o.RetryCap
			if base <= 0 {
				base = 20 * time.Millisecond
			}
			if cap <= 0 {
				cap = time.Second
			}
			delay := min(base*time.Duration(1<<min(n, 20)), cap)
			delay = delay/2 + time.Duration(rand.Int64N(int64(delay/2)+1))
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				err = status.FromContextError(ctx.Err()).Err()
				return err
			case <-timer.C:
			}
		}
		return err
	}
	options := []grpc.DialOption{grpc.WithChainUnaryInterceptor(interceptor)}
	if !o.StatsHandled {
		options = append(options, grpc.WithStatsHandler(otelgrpc.NewClientHandler()))
	}
	return options
}
