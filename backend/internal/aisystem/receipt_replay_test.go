package aisystem

import (
	"context"
	"errors"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
)

func (p *Publisher) Commit(ctx context.Context, n asynccontrol.Notice) error {
	t, r, err := p.Control.LoadResult(ctx, n)
	if errors.Is(err, asynccontrol.ErrObsolete) {
		return nil
	}
	if err != nil {
		return err
	}
	if t.EffectTarget != asynccontrol.EffectAI || r.EffectTarget != asynccontrol.EffectAI {
		return asynccontrol.ErrConflict
	}
	ctx, span := asynccontrol.BeginTrace(ctx, t, "commit")
	defer span.End()
	outcome, err := p.Publish(ctx, t, r)
	if err != nil {
		return err
	}
	return p.Control.Report(ctx, t, r, outcome)
}
