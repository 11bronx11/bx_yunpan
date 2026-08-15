package upload

import (
	"errors"
	"testing"
)

func TestVerificationFailureCodeRecognizesStorageThreshold(t *testing.T) {
	err := errors.New("Storage backend has reached its minimum free drive threshold. Please delete a few objects to proceed.")
	if got := verificationFailureCode(err); got != "upload.storage_unavailable" {
		t.Fatalf("error code = %q", got)
	}
}

func TestVerificationFailureCodeFallsBackToGenericError(t *testing.T) {
	if got := verificationFailureCode(errors.New("connection reset")); got != "upload.verification_failed" {
		t.Fatalf("error code = %q", got)
	}
}
