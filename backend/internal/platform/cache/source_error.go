package cache

import "errors"

type sourceLoadError struct {
	cause error
}

func (e *sourceLoadError) Error() string { return e.cause.Error() }
func (e *sourceLoadError) Unwrap() error { return e.cause }

// SourceLoadCause unwraps an error returned by the authoritative loader. A
// caller can use it to avoid issuing the same failed database query twice.
func SourceLoadCause(err error) (error, bool) {
	var sourceErr *sourceLoadError
	if !errors.As(err, &sourceErr) {
		return nil, false
	}
	return sourceErr.cause, true
}
