package ai

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestShadowSamplingIsDeterministicAndBounded(t *testing.T) {
	owner := uuid.New()
	if !shadowSampled(owner, "query", "fulltext", 1) {
		t.Fatal("rate one must always sample")
	}
	if shadowSampled(owner, "query", "fulltext", 0) {
		t.Fatal("rate zero must never sample")
	}
	if shadowSampled(owner, "query", "fulltext", 0.37) != shadowSampled(owner, "query", "fulltext", 0.37) {
		t.Fatal("sampling decision is not deterministic")
	}
}

func TestMemoryShadowRecorderAndObservationDoNotStoreContent(t *testing.T) {
	fileID, chunkID := uuid.New(), uuid.New()
	postgres := []searchCandidate{{FileID: fileID, ChunkID: &chunkID, Content: stringPtrForTest("sensitive text")}}
	external := []searchCandidate{{FileID: fileID, ChunkID: &chunkID}}
	observation := shadowObservation("fulltext", postgres, external, errors.New("backend unavailable"), 12*time.Millisecond)
	if observation.OverlapCount != 1 || observation.PostgresCount != 1 || observation.ExternalCount != 1 || observation.ExternalError == "" {
		t.Fatalf("observation = %#v", observation)
	}
	if observation.PostgresDigest == "" || observation.ExternalDigest == "" {
		t.Fatal("candidate digests must be present")
	}
	recorder := NewMemoryShadowRecorder(1)
	recorder.Record(observation)
	recorder.Record(ShadowObservation{Mode: "semantic"})
	entries := recorder.Snapshot()
	if len(entries) != 1 || entries[0].Mode != "semantic" {
		t.Fatalf("bounded entries = %#v", entries)
	}
}

func stringPtrForTest(value string) *string { return &value }
