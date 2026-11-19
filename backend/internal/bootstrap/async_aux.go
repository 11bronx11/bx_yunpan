package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncjobs"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dependencies"
	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
)

func runAsyncAux(ctx context.Context, cfg config.Config, queue string, logger *slog.Logger, aux func(context.Context, asynccontrol.Task, asyncprotocol.Input) error) error {
	deps, err := dependencies.OpenAsync(ctx, cfg, "async-control")
	if err != nil {
		return err
	}
	defer deps.Close()
	s := asyncStore(deps, cfg)
	if err := s.CheckProtocol(ctx); err != nil {
		return err
	}
	broker := asynccontrol.NewBroker(cfg.TaskQueue.URL, cfg.Async.BrokerPrefix)
	defer broker.Close()
	computer := &asyncjobs.Computer{Control: s, Read: deps.Read, Auxiliary: aux}
	return s.RunWorker(ctx, broker, queue, cfg.Async.Concurrency, func(ctx context.Context, t asynccontrol.Task) (json.RawMessage, error) {
		out, err := computer.Compute(ctx, t)
		if err != nil {
			return nil, err
		}
		return json.Marshal(out)
	}, func(err error) bool {
		return errors.Is(err, taskqueue.SkipRetry) || errors.Is(err, asynccontrol.ErrInvalid)
	}, logger)
}

func runAsyncAI(ctx context.Context, cfg config.Config, logger *slog.Logger, service *ai.Service) error {
	index := ai.ObjectReadyHandler(service, logger)
	return runAsyncAux(ctx, cfg, "ai", logger, func(ctx context.Context, t asynccontrol.Task, in asyncprotocol.Input) error {
		if t.Kind == "ai.reprocess" {
			return service.ComputeReprocess(ctx, in.ObjectID, in.OriginalPayload)
		}
		return index(ctx, &taskqueue.Task{ID: t.ID.String(), Kind: "ai.index.v1", Queue: "ai", Body: in.OriginalPayload})
	})
}
