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

func TestReciprocalRankFusionKeepsChunkCandidatesPerFile(t *testing.T) {
	fileID := uuid.New()
	firstChunk, secondChunk := uuid.New(), uuid.New()
	fulltext := []searchCandidate{
		{FileID: fileID, ChunkID: &firstChunk, Name: "notes.md", Score: 0.9},
		{FileID: fileID, ChunkID: &secondChunk, Name: "notes.md", Score: 0.8},
	}
	semantic := []searchCandidate{{FileID: fileID, ChunkID: &secondChunk, Name: "notes.md", Score: 0.95}}
	hits := reciprocalRankFusion(fulltext, semantic, 30)
	if len(hits) != 1 {
		t.Fatalf("expected one public file hit, got %d", len(hits))
	}
	if len(hits[0].candidates) != 2 {
		t.Fatalf("expected two distinct chunk candidates for reranking, got %d", len(hits[0].candidates))
	}
	if len(hits[0].Citations) != 2 {
		t.Fatalf("expected two public excerpts, got %d", len(hits[0].Citations))
	}
}

func TestAskRejectsTooManyFileIDs(t *testing.T) {
	service := &Service{}
	fileIDs := make([]uuid.UUID, maxAskFileIDs+1)
	_, _, err := service.Ask(context.Background(), uuid.New(), "question", nil, fileIDs)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected invalid file scope, got %v", err)
	}
}
