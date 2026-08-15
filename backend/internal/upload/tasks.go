package upload

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/minio/minio-go/v7"
)

func VerifyHandler(service *Service) asynq.HandlerFunc {
	return func(ctx context.Context, task *asynq.Task) error {
		var payload struct {
			UploadID uuid.UUID `json:"upload_id"`
		}
		if err := json.Unmarshal(task.Payload(), &payload); err != nil {
			return asynq.SkipRetry
		}
		err := service.Verify(ctx, payload.UploadID)
		if err == nil {
			return nil
		}
		retried, retryOK := asynq.GetRetryCount(ctx)
		maxRetry, maxRetryOK := asynq.GetMaxRetry(ctx)
		if retryOK && maxRetryOK && retried >= maxRetry {
			if failErr := service.FailVerification(ctx, payload.UploadID, verificationFailureCode(err)); failErr != nil {
				return failErr
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
