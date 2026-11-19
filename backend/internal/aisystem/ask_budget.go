package aisystem

import "github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"

type askLimits struct {
	recall, candidates, parents, evidenceTokens, multiplier int
}

func (q *Query) askLimits(selected bool, fileCount int) askLimits {
	b := askLimits{recall: 40, candidates: 60, parents: 12, evidenceTokens: 8000, multiplier: 1}
	if selected {
		b.recall, b.candidates, b.parents, b.evidenceTokens = 80, 100, 24, 12000
		if fileCount > 1 {
			b.candidates = 240
		}
		b.multiplier = aiprotocol.SelectedAskMultiplier(fileCount)
	}
	if q.RerankCandidates > 0 {
		b.candidates = min(q.RerankCandidates, 240)
		b.recall = max(b.recall, b.candidates)
	}
	if q.RerankTopK > 0 {
		b.parents = min(b.parents, q.RerankTopK)
	}
	b.recall *= b.multiplier
	b.candidates *= b.multiplier
	b.parents *= b.multiplier
	b.evidenceTokens *= b.multiplier
	return b
}
