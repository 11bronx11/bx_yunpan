package aiprotocol

import "github.com/google/uuid"

// Published is the compact domain receipt used to replay fixed successors.
// It deliberately has no artifact, attempt generation or provider response.
type Published struct {
	OutputID          uuid.UUID `json:"output_id"`
	ObjectID          uuid.UUID `json:"object_id"`
	ContentGeneration int64     `json:"content_generation"`
	SourceID          uuid.UUID `json:"source_id,omitempty"`
	ContentVersionID  uuid.UUID `json:"content_version_id,omitempty"`
	EmbeddingSetID    uuid.UUID `json:"embedding_set_id,omitempty"`
	RowCount          int64     `json:"row_count"`
	Coverage          string    `json:"coverage,omitempty"`
}

func (p Published) Validate() error {
	if p.OutputID == uuid.Nil || p.ObjectID == uuid.Nil || p.ContentGeneration < 1 || p.RowCount < 0 {
		return ErrInvalid
	}
	return nil
}
