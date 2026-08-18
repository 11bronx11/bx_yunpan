package grpcx

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrorMapper 把业务 error 转成 gRPC status。由调用方注入，避免 grpcx
// 反向依赖业务包。
type ErrorMapper func(error) *status.Status

// ServerOptions 描述服务端拦截器需要的依赖。
type ServerOptions struct {
	Logger  *slog.Logger
	Metrics *Metrics
	Mapper  ErrorMapper
}

// ServerInterceptors 返回服务端需要的 grpc.ServerOption：
// otelgrpc stats handler(恢复上游 trace 并开 server span)+
// Request ID 透传 → panic recover → 错误码映射 → 耗时与错误码指标。
//
// StatsHandler 在拦截器之前执行，所以拦截器与 handler 拿到的 ctx 里
// 已经带上了 server span,手动埋点会自动挂在它下面。
func ServerInterceptors(options ServerOptions) []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(unaryServerInterceptor(options)),
		grpc.ChainStreamInterceptor(streamServerInterceptor(options)),
	}
}

func unaryServerInterceptor(options ServerOptions) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		started := time.Now()
		requestID := requestIDFromIncoming(ctx)
		ctx = WithRequestID(ctx, requestID)

		response, err := func() (response any, err error) {
			defer func() {
				if recovered := recover(); recovered != nil {
					options.Logger.ErrorContext(ctx, "grpc panic recovered",
						"request_id", requestID, "method", info.FullMethod,
						"panic", recovered, "stack", string(debug.Stack()))
					err = status.Error(codes.Internal, "internal server error")
				}
			}()
			return handler(ctx, request)
		}()

		err = mapError(options.Mapper, err)
		code := status.Code(err)
		options.Metrics.observe(info.FullMethod, code.String(), time.Since(started))
		logCall(ctx, options.Logger, requestID, info.FullMethod, code, started, err)
		return response, err
	}
}

func streamServerInterceptor(options ServerOptions) grpc.StreamServerInterceptor {
	return func(service any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		started := time.Now()
		ctx := stream.Context()
		requestID := requestIDFromIncoming(ctx)
		ctx = WithRequestID(ctx, requestID)

		err := func() (err error) {
			defer func() {
				if recovered := recover(); recovered != nil {
					options.Logger.ErrorContext(ctx, "grpc panic recovered",
						"request_id", requestID, "method", info.FullMethod,
						"panic", recovered, "stack", string(debug.Stack()))
					err = status.Error(codes.Internal, "internal server error")
				}
			}()
			return handler(service, &contextServerStream{ServerStream: stream, ctx: ctx})
		}()

		err = mapError(options.Mapper, err)
		code := status.Code(err)
		options.Metrics.observe(info.FullMethod, code.String(), time.Since(started))
		logCall(ctx, options.Logger, requestID, info.FullMethod, code, started, err)
		return err
	}
}

// mapError 只对尚未是 gRPC status 的业务 error 做映射，已经是 status 的原样返回。
func mapError(mapper ErrorMapper, err error) error {
	if err == nil || mapper == nil {
		return err
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	return mapper(err).Err()
}

func logCall(ctx context.Context, logger *slog.Logger, requestID, method string, code codes.Code, started time.Time, err error) {
	if logger == nil {
		return
	}
	attributes := []any{
		"request_id", requestID, "method", method,
		"code", code.String(), "duration_ms", time.Since(started).Milliseconds(),
	}
	if err != nil {
		logger.ErrorContext(ctx, "grpc call", append(attributes, "error", err)...)
		return
	}
	logger.InfoContext(ctx, "grpc call", attributes...)
}

// contextServerStream 让 handler 通过 stream.Context() 拿到带 Request ID 的 context。
type contextServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *contextServerStream) Context() context.Context { return s.ctx }
