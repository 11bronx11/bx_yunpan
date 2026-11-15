package ai

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
)

// ComputeReprocess publishes only AI-derived data. The user-visible job is
// finalized by committer after the durable control result is available.
func (s *Service) ComputeReprocess(ctx context.Context, objectID uuid.UUID, body []byte) error {
	var p eventcontract.AIReprocessRequestedV1
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	return s.processObjectWithMetadata(ctx, objectID, true, IndexMetadata{RunID: p.RunID, ContentHash: p.ContentHash, DocumentVersionID: p.DocumentVersionID, PipelineVersion: p.PipelineVersion, ModelVersion: p.ModelVersion, ParserProfileVersion: p.ParserProfileVersion, TraceID: p.TraceID})
}
