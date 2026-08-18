package grpcclient

import (
	"context"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/breaker"
)

// RetryPolicy 描述单个接口的重试策略。重试必须按接口性质分别定：
//   - Search 只读、可重试；
//   - Ask 调 LLM 非幂等且有成本，MaxAttempts=1 即不重试；
//   - Reprocess 靠 Pipeline/Model Version 幂等，可重试。
type RetryPolicy struct {
	MaxAttempts int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

func (p RetryPolicy) attempts() int {
	if p.MaxAttempts <= 0 {
		return 1
	}
	return p.MaxAttempts
}

// retryable 只对 Unavailable 与 DeadlineExceeded 重试：前者是下游不可达，
// 后者可能是单副本抖动。其余错误码（InvalidArgument、NotFound、
// ResourceExhausted 等）重试没有意义，只会放大下游压力。
func retryable(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded:
		return true
	default:
		return false
	}
}

// backoffFor 计算指数退避 + 抖动。抖动取 [50%, 100%] 区间，避免多副本
// 在同一时刻齐步重试形成尖峰。
func (p RetryPolicy) backoffFor(attempt int) time.Duration {
	base := p.BaseBackoff
	if base <= 0 {
		base = 20 * time.Millisecond
	}
	maximum := p.MaxBackoff
	if maximum <= 0 {
		maximum = time.Second
	}
	scaled := time.Duration(float64(base) * math.Pow(2, float64(attempt-1)))
	if scaled > maximum {
		scaled = maximum
	}
	return scaled/2 + time.Duration(rand.Int64N(int64(scaled/2)+1))
}

// call 在熔断与重试保护下执行一次远程调用。
//
// attempt 返回的 retriable 表示"这次失败在语义上是否还能安全重试"。
// 对 server streaming 来说，一旦已经把消息交给了调用方就不能再重来，
// 因此由 attempt 自己判断，而不是只看错误码。
func call(ctx context.Context, circuit *breaker.Breaker, policy RetryPolicy, attempt func(context.Context) (retriable bool, err error)) error {
	attempts := policy.attempts()
	var lastErr error
	for index := 1; index <= attempts; index++ {
		if ctx.Err() != nil {
			return contextError(ctx, lastErr)
		}
		done, err := circuit.Allow()
		if err != nil {
			// 熔断打开：直接失败，不占用下游资源。
			if lastErr != nil {
				return lastErr
			}
			return status.Error(codes.Unavailable, "AI service circuit breaker is open")
		}
		retriableFailure, callErr := attempt(ctx)
		done(callErr == nil || !countsAsFailure(callErr))
		if callErr == nil {
			return nil
		}
		lastErr = callErr
		if !retriableFailure || !retryable(callErr) || index == attempts {
			return callErr
		}
		wait := policy.backoffFor(index)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return contextError(ctx, lastErr)
		case <-timer.C:
		}
	}
	return lastErr
}

// countsAsFailure 决定哪些错误计入熔断失败率。客户端传参错误、资源不存在、
// 限流与配额耗尽都是上游或业务侧问题，不代表 aisvc 不健康，不应计入。
func countsAsFailure(err error) bool {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.NotFound, codes.ResourceExhausted, codes.FailedPrecondition, codes.AlreadyExists, codes.PermissionDenied, codes.Unauthenticated:
		return false
	default:
		return true
	}
}

// isStreamEnd 判断 server streaming 是否正常结束。
func isStreamEnd(err error) bool {
	return errors.Is(err, io.EOF)
}

func contextError(ctx context.Context, lastErr error) error {
	if lastErr != nil {
		return lastErr
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "AI call deadline exceeded")
	}
	return status.Error(codes.Canceled, "AI call canceled")
}
