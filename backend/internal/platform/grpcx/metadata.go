// Package grpcx 承载 gRPC 服务端与客户端共用的拦截器、指标与元数据约定。
package grpcx

import (
	"context"

	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"
)

// MetadataRequestID 与 HTTP 侧的 X-Request-ID 对齐，让一次业务请求在
// API 与 aisvc 的日志里能用同一个 ID 串起来。
const MetadataRequestID = "x-request-id"

type requestIDKey struct{}

// WithRequestID 把 Request ID 放进 context，供服务端日志使用。
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

// RequestID 读取 context 里的 Request ID，缺失时返回空串。
func RequestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}

// requestIDFromIncoming 从 incoming metadata 取 Request ID，缺失时新生成一个，
// 保证 aisvc 被直接调用（无上游 API）时日志也有可关联的 ID。
func requestIDFromIncoming(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if values := md.Get(MetadataRequestID); len(values) > 0 && values[0] != "" {
			return values[0]
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		id = uuid.New()
	}
	return id.String()
}

// InjectRequestID 把 context 里的 Request ID 写入 outgoing metadata。
func InjectRequestID(ctx context.Context) context.Context {
	requestID := RequestID(ctx)
	if requestID == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, MetadataRequestID, requestID)
}
