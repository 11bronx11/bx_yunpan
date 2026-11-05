package upload

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

func VerifyHandler(service *Service) taskqueue.HandlerFunc {
	return func(ctx context.Context, task *taskqueue.Task) error {
		var payload struct {
			UploadID uuid.UUID `json:"upload_id"`
		}
		if err := json.Unmarshal(task.Payload(), &payload); err != nil {
			return taskqueue.SkipRetry
		}
		err := service.Verify(ctx, payload.UploadID)
		if err == nil {
			return nil
		}
		retried, retryOK := taskqueue.GetRetryCount(ctx)
		maxRetry, maxRetryOK := taskqueue.GetMaxRetry(ctx)
		if errors.Is(err, storageops.ErrExhausted) || (retryOK && maxRetryOK && retried >= maxRetry) {
			if failErr := service.FailVerification(ctx, payload.UploadID, verificationFailureCode(err)); failErr != nil {
				return failErr
			}
			if errors.Is(err, storageops.ErrExhausted) {
				return errors.Join(taskqueue.SkipRetry, err)
			}
		}
		return err
	}
}

func verificationFailureCode(err error) string {
	response := minio.ToErrorResponse(err)
	if response.Code == "XMinioStorageFull" || strings.Contains(strings.ToLower(err.Error()), "minimum free drive threshold") {
		return "upload.storage_unavailable"
	}
	return "upload.verification_failed"
}
