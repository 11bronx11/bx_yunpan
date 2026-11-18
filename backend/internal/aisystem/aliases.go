package aisystem

import corepb "github.com/11bronx11/bx_yunpan/backend/internal/core/pb"

func candidateFiles(c candidate) []*corepb.FileRef {
	if len(c.Aliases) > 0 {
		return c.Aliases
	}
	return []*corepb.FileRef{c.File}
}
