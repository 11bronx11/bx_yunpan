package ai

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

// ShadowObservation contains only aggregate/digest data. Query text and
// document content are intentionally absent so it is safe to export with
// normal application metrics or structured logs.
type ShadowObservation struct {
	Mode             string
	PostgresCount    int
	ExternalCount    int
	OverlapCount     int
	PostgresDigest   string
	ExternalDigest   string
	ExternalError    string
	ExternalDuration time.Duration
}

type ShadowRecorder interface {
	Record(ShadowObservation)
}

// MemoryShadowRecorder is a bounded in-process recorder useful for local
// rehearsal and tests. Production callers can provide a metrics-backed
// implementation through Service.WithShadowRecorder.
type MemoryShadowRecorder struct {
	mu      sync.Mutex
	limit   int
	entries []ShadowObservation
}

func NewMemoryShadowRecorder(limit int) *MemoryShadowRecorder {
	if limit <= 0 {
		limit = 256
	}
	return &MemoryShadowRecorder{limit: limit}
}

func (r *MemoryShadowRecorder) Record(value ShadowObservation) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) == r.limit {
		copy(r.entries, r.entries[1:])
		r.entries = r.entries[:r.limit-1]
	}
	r.entries = append(r.entries, value)
}

func (r *MemoryShadowRecorder) Snapshot() []ShadowObservation {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ShadowObservation(nil), r.entries...)
}

func shadowObservation(mode string, postgres, external []searchCandidate, externalErr error, duration time.Duration) ShadowObservation {
	postgresIDs := candidateIDs(postgres)
	externalIDs := candidateIDs(external)
	postgresSet := make(map[string]struct{}, len(postgresIDs))
	for _, id := range postgresIDs {
		postgresSet[id] = struct{}{}
	}
	overlap := 0
	for _, id := range externalIDs {
		if _, ok := postgresSet[id]; ok {
			overlap++
		}
	}
	value := ShadowObservation{Mode: mode, PostgresCount: len(postgresIDs), ExternalCount: len(externalIDs), OverlapCount: overlap, PostgresDigest: digestIDs(postgresIDs), ExternalDigest: digestIDs(externalIDs), ExternalDuration: duration}
	if externalErr != nil {
		value.ExternalError = externalErr.Error()
	}
	return value
}

func candidateIDs(values []searchCandidate) []string {
	ids := make([]string, 0, len(values))
	for _, value := range values {
		id := value.FileID.String()
		if value.ChunkID != nil {
			id += ":" + value.ChunkID.String()
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func digestIDs(ids []string) string {
	hash := sha256.New()
	for _, id := range ids {
		_, _ = hash.Write([]byte(id))
		_, _ = hash.Write([]byte{'\n'})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}
