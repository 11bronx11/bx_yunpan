package ai

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestDisabledBackendReturnsUnavailable(t *testing.T) {
	backend := DisabledBackend{}
	ownerID := uuid.New()

	if _, err := backend.Search(context.Background(), ownerID, SearchInput{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Search error = %v, want ErrUnavailable", err)
	}
	if _, _, err := backend.Ask(context.Background(), ownerID, "question", nil, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Ask error = %v, want ErrUnavailable", err)
	}
	if _, err := backend.RequestReprocess(context.Background(), ownerID, uuid.New(), "key"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("RequestReprocess error = %v, want ErrUnavailable", err)
	}
}
