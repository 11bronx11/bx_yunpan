package ai

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestSearchRejectsOversizedQuery(t *testing.T) {
	service := &Service{}
	_, err := service.Search(context.Background(), uuid.New(), SearchInput{
		Query: strings.Repeat("x", maxSearchQueryRunes+1), Mode: "name",
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected invalid search request, got %v", err)
	}
}

func TestAskRejectsOversizedQuestion(t *testing.T) {
	service := &Service{}
	_, _, err := service.Ask(context.Background(), uuid.New(), strings.Repeat("x", maxQuestionRunes+1), nil, nil)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected invalid ask request, got %v", err)
	}
}
