package grpcx

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RequestUnavailable preserves the public Unavailable contract for data that
// is not ready in this request's scope. It must neither trip a shared dependency
// circuit nor be retried without a change to the request's data.
func RequestUnavailable(message string) error {
	s := status.New(codes.Unavailable, message)
	detailed, err := s.WithDetails(&errdetails.ErrorInfo{Domain: "internal.rpc", Reason: "REQUEST_UNAVAILABLE"})
	if err != nil {
		return s.Err()
	}
	return detailed.Err()
}

func isRequestUnavailable(err error) bool {
	s := status.Convert(err)
	if s.Code() != codes.Unavailable {
		return false
	}
	for _, detail := range s.Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok && info.Domain == "internal.rpc" && info.Reason == "REQUEST_UNAVAILABLE" {
			return true
		}
	}
	return false
}
