package aisystem

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/lib/pq"
	"gorm.io/gorm"
)

// Keep the distance ORDER BY and LIMIT on the published view so PostgreSQL can
// use ai_vectors_cosine. Materializing eligible vectors first would hide that
// ordering. Keep UUID tie-breaking outside the limited distance scan and apply
// it to an oversampled candidate set instead.
const pgVectorNearestSQL = `WITH nearest AS MATERIALIZED (
	SELECT child_id, embedding <=> ?::vector AS distance
	FROM ai_published_vectors
	WHERE embedding_set_id = ANY(?::uuid[])
	ORDER BY embedding <=> ?::vector
	LIMIT ?
)
SELECT child_id AS id, 1-distance AS score
FROM nearest ORDER BY distance, child_id LIMIT ?`

// An ANN scan can exhaust its scan budget before finding enough authorized
// vectors. In that case, retain the exact scoped fallback rather than return a
// short or empty result while eligible evidence still exists.
const pgVectorExactSQL = `WITH eligible AS MATERIALIZED (
	SELECT child_id, embedding FROM ai_published_vectors
	WHERE embedding_set_id = ANY(?::uuid[])
)
SELECT child_id AS id, 1-(embedding <=> ?::vector) AS score
FROM eligible ORDER BY embedding <=> ?::vector, child_id LIMIT ?`

func (q *Query) pgVector(ctx context.Context, ids []string, vector []float32, k int) ([]ranked, error) {
	if len(ids) == 0 {
		return nil, errors.New("vector.not_ready")
	}
	if k <= 0 {
		return nil, nil
	}
	var rows []ranked
	literal := vectorLiteral(vector)
	candidates := max(40, 4*k)
	err := q.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Iterative scanning continues past vectors rejected by the scope or
		// publication filters (pgvector >= 0.8). Settings must not leak through
		// the connection pool, including on query failure or cancellation.
		if err := tx.Exec(`SELECT set_config('hnsw.iterative_scan', 'strict_order', true), set_config('hnsw.ef_search', ?, true)`, strconv.Itoa(min(1000, max(100, k)))).Error; err != nil {
			return err
		}
		if err := tx.Raw(pgVectorNearestSQL, literal, pq.Array(ids), literal, candidates, k).Scan(&rows).Error; err != nil {
			return err
		}
		if len(rows) < k {
			rows = nil
			return tx.Raw(pgVectorExactSQL, pq.Array(ids), literal, literal, k).Scan(&rows).Error
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return rows, err
}
