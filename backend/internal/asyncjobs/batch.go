package asyncjobs

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/ownerepoch"
)

// CommitBatch groups only compatible work in the same commit domain. Complex
// final-file/GC authorization branches retain their individual transactions.
// A rollback falls back to bounded single-result commits, preserving each ACK.
func (c *Committer) CommitBatch(ctx context.Context, notices []asynccontrol.Notice) []error {
	type item struct {
		index    int
		t        asynccontrol.Task
		r        asynccontrol.Result
		in       asyncprotocol.Input
		out      asyncprotocol.Output
		decision asyncprotocol.Decision
	}
	results := make([]error, len(notices))
	groups := map[string][]*item{}
	var singles []int
	for i, n := range notices {
		t, r, err := c.Control.LoadResult(ctx, n)
		if errors.Is(err, asynccontrol.ErrObsolete) {
			continue
		}
		if err != nil {
			singles = append(singles, i)
			continue
		}
		if asynccontrol.Terminal(t.State) && t.State != asynccontrol.NeedsAttention {
			continue
		}
		switch t.Kind {
		case "multipart.create", "multipart.complete", "staging.delete", "variant.delete", "output.delete", "thumbnail", "upload.expire", "failure.converge":
		default:
			singles = append(singles, i)
			continue
		}
		x := &item{index: i, t: t, r: r}
		if json.Unmarshal(t.Input, &x.in) != nil || json.Unmarshal(r.Body, &x.out) != nil {
			singles = append(singles, i)
			continue
		}
		if x.out.Error != "" && !x.out.Convergence {
			singles = append(singles, i)
			continue
		}
		groups[t.ShardKey] = append(groups[t.ShardKey], x)
	}
	for _, group := range groups {
		if len(group) == 1 {
			singles = append(singles, group[0].index)
			continue
		}
		sort.Slice(group, func(i, j int) bool { return group[i].t.ResourceKey < group[j].t.ResourceKey })
		traceCtx, span := asynccontrol.BeginTrace(ctx, group[0].t, "commit_batch")
		bounded, cancel := context.WithTimeout(identity.BatchQuota(ownerepoch.Batch(ownerepoch.Background(traceCtx))), c.timeout())
		err := c.Business.WithContext(bounded).Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SET LOCAL lock_timeout='2s'; SET LOCAL statement_timeout='10s'").Error; err != nil {
				return err
			}
			for _, x := range group {
				d, err := c.commitTx(tx, x.t, x.r, x.in, x.out)
				if err != nil {
					return err
				}
				if d.RescheduleAt != nil {
					return asynccontrol.ErrInvalid
				}
				x.decision = d
			}
			return identity.FlushQuota(tx)
		})
		cancel()
		span.End()
		if err != nil {
			for _, x := range group {
				singles = append(singles, x.index)
			}
			continue
		}
		for _, x := range group {
			if c.AfterCommit != nil {
				if err := c.AfterCommit(); err != nil {
					results[x.index] = err
					continue
				}
			}
			results[x.index] = c.Control.Report(ctx, x.t, x.r, x.decision.Outcome)
		}
	}
	for _, i := range singles {
		results[i] = c.Commit(ctx, notices[i])
	}
	return results
}
