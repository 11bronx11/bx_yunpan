package asyncjobs

import (
	"context"
	"errors"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
)

// Direct receipt replay helper for domain transaction regression tests.
func (c *Committer) Commit(ctx context.Context, n asynccontrol.Notice) error {
	t, r, err := c.Control.LoadResult(ctx, n)
	if errors.Is(err, asynccontrol.ErrObsolete) {
		return nil
	}
	if err != nil {
		return err
	}
	if asynccontrol.Terminal(t.State) {
		return nil
	}
	outcome, due, err := c.Apply(ctx, t, r)
	if err != nil {
		if errors.Is(err, asynccontrol.ErrConflict) || errors.Is(err, asynccontrol.ErrInvalid) {
			return c.Control.IsolateResult(ctx, n, err.Error())
		}
		return err
	}
	if due != nil {
		return c.Control.Reschedule(ctx, t, r, *due)
	}
	return c.Control.Report(ctx, t, r, outcome)
}
