package taskqueue

import "context"

type metadataKey struct{}

func WithMetadata(ctx context.Context, t Task) context.Context {
	return context.WithValue(ctx, metadataKey{}, t)
}
func GetRetryCount(ctx context.Context) (int, bool) {
	t, ok := ctx.Value(metadataKey{}).(Task)
	return t.RetryCount, ok
}
func GetMaxRetry(ctx context.Context) (int, bool) {
	t, ok := ctx.Value(metadataKey{}).(Task)
	return t.MaxRetry, ok
}
func GetTaskID(ctx context.Context) (string, bool) {
	t, ok := ctx.Value(metadataKey{}).(Task)
	return t.ID, ok
}
