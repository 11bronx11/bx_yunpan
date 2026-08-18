package ai

import (
	"errors"
	"net/http"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrRateLimited 与 ErrQuota 都映射到 gRPC ResourceExhausted，靠 ErrorInfo.Reason
// 携带的 AppCode 区分，避免 HTTP 层丢失原始语义。
var ErrRateLimited = errors.New("AI request rate limit exceeded")

// ErrorMapping 是业务 error、gRPC status、HTTP status 三层映射的唯一来源。
// 新增业务错误只改这张表，不要在 handler 或拦截器里散落 switch。
type ErrorMapping struct {
	AppCode string
	Err     error
	Code    codes.Code
	Status  int
	Message string
}

var errorMappings = []ErrorMapping{
	{AppCode: "ai.not_found", Err: ErrNotFound, Code: codes.NotFound, Status: http.StatusNotFound, Message: "AI resource not found"},
	{AppCode: "ai.invalid_request", Err: ErrInvalid, Code: codes.InvalidArgument, Status: http.StatusUnprocessableEntity, Message: "invalid AI request"},
	{AppCode: "ai.rate_limited", Err: ErrRateLimited, Code: codes.ResourceExhausted, Status: http.StatusTooManyRequests, Message: "AI request rate limit exceeded"},
	{AppCode: "ai.quota_exhausted", Err: ErrQuota, Code: codes.ResourceExhausted, Status: http.StatusTooManyRequests, Message: "AI provider quota exhausted"},
	{AppCode: "ai.unavailable", Err: ErrUnavailable, Code: codes.Unavailable, Status: http.StatusServiceUnavailable, Message: "AI provider unavailable"},
	{AppCode: "ai.processing", Err: ErrProcessing, Code: codes.FailedPrecondition, Status: http.StatusConflict, Message: "AI document is already processing"},
}

const internalAppCode = "internal.error"

// ErrorMappings 供测试遍历断言三层映射一致。
func ErrorMappings() []ErrorMapping { return errorMappings }

// GRPCStatus 把业务 error 转成 gRPC status，并在 details 里带上 AppCode。
func GRPCStatus(err error) *status.Status {
	for _, mapping := range errorMappings {
		if errors.Is(err, mapping.Err) {
			return statusWithAppCode(mapping.Code, mapping.Message, mapping.AppCode)
		}
	}
	return statusWithAppCode(codes.Internal, "internal server error", internalAppCode)
}

func statusWithAppCode(code codes.Code, message, appCode string) *status.Status {
	base := status.New(code, message)
	detailed, err := base.WithDetails(&errdetails.ErrorInfo{Reason: appCode, Domain: "ai.yunpan"})
	if err != nil {
		return base
	}
	return detailed
}

// FromGRPCError 把 gRPC 错误还原成业务 error，优先按 details 里的 AppCode 匹配，
// 缺失时退回按 gRPC code 匹配，未知一律当 ErrUnavailable 之外的内部错误。
func FromGRPCError(err error) error {
	if err == nil {
		return nil
	}
	grpcStatus, ok := status.FromError(err)
	if !ok {
		return err
	}
	if appCode := appCodeFromDetails(grpcStatus); appCode != "" {
		for _, mapping := range errorMappings {
			if mapping.AppCode == appCode {
				return mapping.Err
			}
		}
	}
	switch grpcStatus.Code() {
	case codes.NotFound:
		return ErrNotFound
	case codes.InvalidArgument:
		return ErrInvalid
	case codes.ResourceExhausted:
		return ErrQuota
	case codes.Unavailable, codes.DeadlineExceeded:
		return ErrUnavailable
	default:
		return err
	}
}

func appCodeFromDetails(grpcStatus *status.Status) string {
	for _, detail := range grpcStatus.Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok {
			return info.GetReason()
		}
	}
	return ""
}

// HTTPStatus 把业务 error 映射为 HTTP 状态码与响应体 code。
func HTTPStatus(err error) (int, string, string) {
	for _, mapping := range errorMappings {
		if errors.Is(err, mapping.Err) {
			return mapping.Status, mapping.AppCode, mapping.Message
		}
	}
	return http.StatusInternalServerError, internalAppCode, "internal server error"
}
