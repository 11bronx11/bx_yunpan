// Package asyncprotocol defines the closed set of compute/commit contracts.
// Fields describe immutable inputs and necessary business preconditions, never SQL.
package asyncprotocol

import (
	"time"

	"github.com/google/uuid"
)

type Part struct {
	Number int    `json:"number"`
	ETag   string `json:"etag"`
}
type Input struct {
	OperationID       uuid.UUID `json:"operation_id,omitempty"`
	SessionID         uuid.UUID `json:"session_id,omitempty"`
	OwnerID           uuid.UUID `json:"owner_id,omitempty"`
	ObjectID          uuid.UUID `json:"object_id,omitempty"`
	ContentGeneration int64     `json:"content_generation,omitempty"`
	Bucket            string    `json:"bucket"`
	Key               string    `json:"key"`
	TargetBucket      string    `json:"target_bucket,omitempty"`
	TargetKey         string    `json:"target_key,omitempty"`
	UploadID          string    `json:"upload_id,omitempty"`
	SHA256            string    `json:"sha256,omitempty"`
	Size              int64     `json:"size,omitempty"`
	Mime              string    `json:"mime,omitempty"`
	Parts             []Part    `json:"parts,omitempty"`
	// A GC generation changes only for an actual new lifecycle, not reference churn.
	DeletionGeneration int64     `json:"deletion_generation,omitempty"`
	VariantKeys        []string  `json:"variant_keys,omitempty"`
	OriginalPayload    []byte    `json:"original_payload,omitempty"`
	EventID            uuid.UUID `json:"event_id,omitempty"`
	EventType          string    `json:"event_type,omitempty"`
}
type Output struct {
	UploadID    string `json:"upload_id,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	Size        int64  `json:"size,omitempty"`
	Bucket      string `json:"bucket,omitempty"`
	Key         string `json:"key,omitempty"`
	Mime        string `json:"mime,omitempty"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	Noop        bool   `json:"noop,omitempty"`
	Convergence bool   `json:"convergence,omitempty"`
	Error       string `json:"error,omitempty"`
}
type Decision struct {
	Outcome      string
	RescheduleAt *time.Time
}
