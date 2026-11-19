package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/aisystem"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncjobs"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/11bronx11/bx_yunpan/backend/internal/identity"
	"github.com/11bronx11/bx_yunpan/backend/internal/media"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dependencies"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/serviceauth"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/tracing"
	"github.com/11bronx11/bx_yunpan/backend/internal/upload"
)

func asyncStore(deps *dependencies.AsyncSet, cfg config.Config) *asynccontrol.Store {
	s := asynccontrol.New(deps.Control)
	if cfg.Async.ProtocolVersion == 3 {
		s.ProtocolVersion = 3
	}
	s.PlanAbandoned = asyncjobs.AbandonedOutputs(cfg.Async.OutputSafety)
	s.Critical = deps.Critical
	s.MaxAttempts = cfg.Async.MaxAttempts
	s.MaxDispatches = cfg.Async.MaxDispatches
	for k, d := range s.Registry {
		d.Lifetime = cfg.Async.TaskLifetime
		s.Registry[k] = d
	}
	s.Lease = cfg.Async.Lease
	s.RetryBase = cfg.Async.RetryBase
	s.RetryCap = cfg.Async.RetryCap
	s.CommitWindow = cfg.Async.CommitWindow
	aisystem.Register(s.Registry)
	planner := &aisystem.Planner{ProjectionBackends: cfg.Search.WriteBackends, TextEpoch: cfg.AITextEpoch(), VectorEpoch: cfg.AIVectorEpoch(), TargetNamespace: cfg.AISplit.ProjectionNamespace, TargetEpoch: cfg.AIProjectionEpoch(), Profile: cfg.AIProfile()}
	s.PlanContinuations = planner.Continuations
	s.PlanCompleted = planner.Completed
	s.PlanFailure = planner.Failure
	s.Observe = planner.Observe
	s.AfterTerminal = planner.Finish
	return s
}

// RunAsync wires only the dependencies required by one explicit role. Core
// workers use separate read and domain-write pools; control stays read-only.
func RunAsync(ctx context.Context, cfg config.Config, logger *slog.Logger, role string) error {
	ctx = outbox.WithAIProfile(ctx, cfg.AISplit.ProfileID, cfg.AISplit.ProfileRevision)
	shutdown, err := tracing.Setup(ctx, cfg.Tracing, "bx-"+role)
	if err != nil {
		return err
	}
	defer shutdown(context.WithoutCancel(ctx))
	deps, err := dependencies.OpenAsync(ctx, cfg, role)
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
	closeMonitor, err := s.Monitor(ctx, cfg.Worker.MetricsAddr, map[bool]string{true: "control:", false: "worker:"}[role == "async-control"]+cfg.App.InstanceID, deps.Read, broker, logger)
	if err != nil {
		return err
	}
	defer closeMonitor()
	switch role {
	case "async-control":
		if err := asyncjobs.CheckImport(ctx, deps.Read, s); err != nil {
			return err
		}
		processingKey, err := serviceauth.Private(cfg.AISplit.ProcessingSigningSeed)
		if err != nil && cfg.AI.Enabled {
			return err
		}
		aiPlanner := &aisystem.Planner{ProjectionBackends: cfg.Search.WriteBackends, TextEpoch: cfg.AITextEpoch(), VectorEpoch: cfg.AIVectorEpoch(), TargetNamespace: cfg.AISplit.ProjectionNamespace, TargetEpoch: cfg.AIProjectionEpoch(), Profile: cfg.AIProfile(), ProcessingKey: processingKey}
		relay := &asynccontrol.Relay{Control: s, Business: deps.Read, Source: "business-primary", Rows: cfg.Async.RelayRows, Bytes: cfg.Async.RelayBytes, Plan: (&asyncjobs.Planner{Read: deps.Read, AI: cfg.AI.Enabled, Search: cfg.Search.Enabled, GCDelay: cfg.Outbox.GCDelay, OutputSafety: cfg.Async.OutputSafety, AIIntent: aiPlanner.Intent}).Plan}
		return runAsyncGroup(ctx,
			func(c context.Context) error {
				return relay.Run(c, cfg.App.InstanceID, cfg.Async.PollMin, cfg.Async.PollMax, logger)
			},
			func(c context.Context) error { return s.RunScheduler(c, broker, cfg.App.InstanceID, logger) },
			func(c context.Context) error { return s.RunDeadLetters(c, broker, logger) },
			func(c context.Context) error {
				return s.RunModule(c, "maintenance", cfg.App.InstanceID, func(live context.Context, _ uuid.UUID) error {
					for live.Err() == nil {
						if err := s.RecoverLegacyOutputs(live, 100, cfg.Async.OutputSafety); err != nil {
							return err
						}
						if err := s.Maintain(live, 100, cfg.Async.DetailRetention); err != nil {
							return err
						}
						if !asynccontrol.Pause(live, time.Minute) {
							break
						}
					}
					return live.Err()
				}, logger)
			},
		)
	case "upload-worker", "object-worker", "media-worker", "gc-worker":
		storage, err := minio.New(cfg.Storage.Endpoint, &minio.Options{Creds: credentials.NewStaticV4(cfg.Storage.AccessKey, cfg.Storage.SecretKey, ""), Secure: cfg.Storage.Secure, Region: cfg.Storage.Region})
		if err != nil {
			return err
		}
		if role == "gc-worker" {
			lifecycleCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := asyncjobs.EnsureLifecycle(lifecycleCtx, storage, cfg.Storage.Bucket, cfg.Async.StagingDays, cfg.Upload.SessionTTL+cfg.Async.TaskLifetime+cfg.Async.CommitWindow+cfg.Async.OutputSafety)
			cancel()
			if err != nil {
				return err
			}
		}
		objects := objectstore.NewService(deps.Business)
		quota := identity.NewService(deps.Business, nil, nil, cfg.Identity.DefaultQuotaBytes)
		uploads := upload.NewService(deps.Business, nil, nil, cfg.Storage.Bucket, drive.NewService(deps.Business), objects, quota, cfg.Upload)
		applier := &asyncjobs.Committer{Business: deps.Business, Control: s, Upload: uploads, Objects: objects, Media: media.NewService(deps.Business, objects, nil, nil, cfg.Storage.Bucket, cfg.Storage.ReadURLTTL), GCDelay: cfg.Outbox.GCDelay, Auxiliary: commitAuxiliary}
		computer := &asyncjobs.Computer{Control: s, Read: deps.Read, Storage: storage}
		queue := map[string]string{"upload-worker": "upload", "object-worker": "object", "media-worker": "media", "gc-worker": "gc"}[role]
		compute := func(c context.Context, t asynccontrol.Task) (json.RawMessage, error) {
			out, err := computer.Compute(c, t)
			if err != nil {
				return nil, err
			}
			return json.Marshal(out)
		}
		direct := &asyncjobs.DirectWorker{Computer: computer, Committer: applier}
		worker := func(c context.Context) error {
			if s.ProtocolVersion == 3 {
				return s.RunDirectWorker(c, broker, queue, cfg.Async.Concurrency, direct, func(err error) bool { return errors.Is(err, asynccontrol.ErrInvalid) }, logger)
			}
			return s.RunWorker(c, broker, queue, cfg.Async.Concurrency, compute, func(err error) bool { return errors.Is(err, asynccontrol.ErrInvalid) }, logger, applier.Apply)
		}
		if role != "gc-worker" {
			return worker(ctx)
		}
		return runAsyncGroup(ctx, worker, func(c context.Context) error {
			if s.ProtocolVersion == 3 {
				return s.RunDirectWorker(c, broker, "core-command", 1, direct, func(err error) bool { return errors.Is(err, asynccontrol.ErrInvalid) }, logger)
			}
			return s.RunWorker(c, broker, "core-command", 1, nil, func(err error) bool { return errors.Is(err, asynccontrol.ErrInvalid) }, logger, applier.Apply)
		}, func(c context.Context) error {
			return s.RunModule(c, "business-event-retention", cfg.App.InstanceID, func(live context.Context, _ uuid.UUID) error {
				for live.Err() == nil {
					bounded, cancel := context.WithTimeout(live, 5*time.Second)
					err := asyncjobs.MaintainEvents(bounded, deps.Business, s, 100, cfg.Async.DetailRetention)
					cancel()
					if err != nil {
						return err
					}
					if !asynccontrol.Pause(live, time.Minute) {
						break
					}
				}
				return live.Err()
			}, logger)
		}, func(c context.Context) error {
			return s.RunModule(c, "expiry-scan", cfg.App.InstanceID, func(live context.Context, token uuid.UUID) error {
				for live.Err() == nil {
					bounded, cancel := context.WithTimeout(live, 5*time.Second)
					err := s.ScanExpiry(bounded, deps.Read, token, cfg.Async.ScanBatch)
					cancel()
					if err != nil {
						return err
					}
					logger.Info("async expiry scan completed", "limit", cfg.Async.ScanBatch)
					if !asynccontrol.Pause(live, cfg.Async.ScanInterval) {
						break
					}
				}
				return live.Err()
			}, logger)
		})
	default:
		return fmt.Errorf("unknown async role %s", role)
	}
}

func runAsyncGroup(ctx context.Context, fns ...func(context.Context) error) error {
	live, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, len(fns))
	var wg sync.WaitGroup
	for _, fn := range fns {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- fn(live) }()
	}
	err := <-errs
	cancel()
	wg.Wait()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func commitAuxiliary(tx *gorm.DB, t asynccontrol.Task, in asyncprotocol.Input, out asyncprotocol.Output) (asyncprotocol.Decision, error) {
	d := asyncprotocol.Decision{Outcome: asynccontrol.Succeeded}
	if t.Kind != "ai.reprocess" {
		return d, asynccontrol.ErrInvalid
	}
	status := "succeeded"
	var code any
	var message any
	if out.Convergence || out.Error != "" {
		status = "failed"
		d.Outcome = asynccontrol.Failed
		code = "ai.reprocess_failed"
		message = out.Error
	}
	q := tx.Table("async_tasks").Where("id=? AND owner_id=? AND resource_id=?", in.EventID, in.OwnerID, in.ObjectID).Updates(map[string]any{"status": status, "progress": 100, "error_code": code, "error_message": message, "version": gorm.Expr("version+1"), "updated_at": gorm.Expr("clock_timestamp()")})
	if q.Error != nil {
		return d, q.Error
	}
	if q.RowsAffected != 1 {
		return d, asynccontrol.ErrConflict
	}
	return d, nil
}
