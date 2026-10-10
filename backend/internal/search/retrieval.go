package search

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// RetrievalRequest is the stable query boundary. Backend-specific DSL and
// payload types must remain inside the adapter implementation.
type RetrievalRequest struct {
	OwnerID       uuid.UUID
	TargetVersion string
	IndexEpoch    int64
	Query         string
	FolderID      *uuid.UUID
	FileIDs       []uuid.UUID
	Limit         int
	Oversample    int
	Timeout       time.Duration
}

type RetrievalCandidate struct {
	ChunkID           uuid.UUID
	ObjectID          uuid.UUID
	DocumentVersionID uuid.UUID
	FileEntryID       *uuid.UUID
	Score             float64
	IndexEpoch        int64
	PipelineVersion   string
	ModelVersion      string
	MatchType         string
}

type RetrievalBackend interface {
	SearchText(context.Context, RetrievalRequest) ([]RetrievalCandidate, error)
	SearchVector(context.Context, RetrievalRequest, []float32) ([]RetrievalCandidate, error)
	Health(context.Context) error
}

// RetrievalRouter combines the independent text and vector services behind
// one narrow boundary used by ai-query.
type RetrievalRouter struct {
	Text   RetrievalBackend
	Vector RetrievalBackend
}

func NewRetrievalRouter(text, vector RetrievalBackend) *RetrievalRouter {
	return &RetrievalRouter{Text: text, Vector: vector}
}
func (r *RetrievalRouter) SearchText(ctx context.Context, request RetrievalRequest) ([]RetrievalCandidate, error) {
	if r == nil || r.Text == nil {
		return nil, ErrUnavailable
	}
	return r.Text.SearchText(ctx, request)
}
func (r *RetrievalRouter) SearchVector(ctx context.Context, request RetrievalRequest, vector []float32) ([]RetrievalCandidate, error) {
	if r == nil || r.Vector == nil {
		return nil, ErrUnavailable
	}
	return r.Vector.SearchVector(ctx, request, vector)
}
func (r *RetrievalRouter) Health(ctx context.Context) error {
	if r == nil {
		return ErrUnavailable
	}
	if r.Text != nil {
		if err := r.Text.Health(ctx); err != nil {
			return err
		}
	}
	if r.Vector != nil {
		if err := r.Vector.Health(ctx); err != nil {
			return err
		}
	}
	return nil
}

var (
	ErrUnavailable = errors.New("search backend unavailable")
	ErrPermanent   = errors.New("search backend rejected request")
)

type HTTPError struct {
	Backend    string
	StatusCode int
	Retryable  bool
	// ErrorType and Reason preserve a bounded upstream error classification
	// for adapters that need to distinguish an idempotent conflict from a
	// malformed request (for example Elasticsearch 400 responses).
	ErrorType string
	Reason    string
}

func (e *HTTPError) Error() string {
	if e == nil {
		return ""
	}
	if e.StatusCode == 0 {
		return e.Backend + ": request failed"
	}
	return e.Backend + ": request failed with status " + http.StatusText(e.StatusCode)
}

func (e *HTTPError) Unwrap() error {
	if e != nil && e.Retryable {
		return ErrUnavailable
	}
	return ErrPermanent
}
