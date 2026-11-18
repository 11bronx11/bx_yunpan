package aisystem

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
)

func (q *Query) sampleShadow(query, mode string) bool {
	if q.ShadowProjector == nil || q.ShadowRecorder == nil {
		return false
	}
	rate := q.ShadowSampleRate
	if rate <= 0 || rate >= 1 {
		return true
	}
	hash := sha256.Sum256([]byte(mode + "\x00" + query))
	return float64(binary.BigEndian.Uint64(hash[:8])>>11)/float64(uint64(1)<<53) < rate
}

func (q *Query) shadowBranch(ctx context.Context, backend string, versions []string, query string, vector []float32, k, batchSize, mergeK int) ([]ranked, error) {
	var all []ranked
	for start := 0; start < len(versions); start += batchSize {
		rows, _, err := q.ShadowProjector.external(ctx, backend, versions[start:min(start+batchSize, len(versions))], query, vector, k)
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)
	}
	return mergeRanks(all, mergeK), nil
}

func (q *Query) recordShadow(mode string, pg, external []ranked, err error, duration time.Duration) {
	ids := func(rows []ranked) ([]string, map[string]bool) {
		all := []string{}
		seen := map[string]bool{}
		for _, r := range rows {
			if !seen[r.ID.String()] {
				all = append(all, r.ID.String())
				seen[r.ID.String()] = true
			}
		}
		sort.Strings(all)
		return all, seen
	}
	pgIDs, pgSet := ids(pg)
	exIDs, _ := ids(external)
	overlap := 0
	for _, id := range exIDs {
		if pgSet[id] {
			overlap++
		}
	}
	observation := ai.ShadowObservation{Mode: mode, PostgresCount: len(pgIDs), ExternalCount: len(exIDs), OverlapCount: overlap, PostgresDigest: hashIDs(pgIDs), ExternalDigest: hashIDs(exIDs), ExternalDuration: duration}
	if err != nil {
		observation.ExternalError = "projection.unavailable"
	}
	q.ShadowRecorder.Record(observation)
}

func hashIDs(ids []string) string {
	raw := []byte{}
	for _, id := range ids {
		raw = append(raw, id...)
		raw = append(raw, '\n')
	}
	return "sha256:" + aiprotocol.Hash(raw)
}
