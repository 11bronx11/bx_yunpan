package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/parserrpc"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pbv2"
	"github.com/11bronx11/bx_yunpan/backend/internal/aisystem"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/core"
	corepb "github.com/11bronx11/bx_yunpan/backend/internal/core/pb"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dependencies"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/logging"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/registry"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/serviceauth"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/storageurl"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/resolver"
	"gorm.io/gorm"
)

func internalClient(cfg config.Config, name, target, token string) (*grpc.ClientConn, func(), error) {
	cleanup := func() {}
	if cfg.Registry.Enabled {
		etcd, err := registry.Open(cfg.Registry)
		if err != nil {
			return nil, cleanup, err
		}
		resolver.Register(registry.NewBuilder(etcd, slog.Default(), registry.NewMetrics(prometheus.NewRegistry())))
		target = registry.Scheme + ":///services/" + name
		cleanup = func() { _ = etcd.Close() }
	}
	transport, err := serviceauth.ClientTransport(cfg.AISplit.TLS)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(transport), grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`), grpc.WithUnaryInterceptor(serviceauth.UnaryClient(token)), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(8<<20)))
	if err != nil {
		cleanup()
	}
	return conn, cleanup, err
}
func serveInternal(ctx context.Context, cfg config.Config, logger *slog.Logger, name, addr, advertise, token, prefix string, register func(*grpc.Server)) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	options := []grpc.ServerOption{grpc.UnaryInterceptor(serviceauth.UnaryServer(token, prefix))}
	transport, err := serviceauth.ServerTransport(cfg.AISplit.TLS)
	if err != nil {
		return err
	}
	if transport != nil {
		options = append(options, grpc.Creds(transport))
	}
	srv := grpc.NewServer(options...)
	register(srv)
	h := health.NewServer()
	healthpb.RegisterHealthServer(srv, h)
	h.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	if cfg.Registry.Enabled {
		etcd, err := registry.Open(cfg.Registry)
		if err != nil {
			return err
		}
		defer etcd.Close()
		reg := registry.NewRegistrar(etcd, logger, "/services/"+name+"/"+cfg.App.InstanceID, advertise, cfg.Registry.LeaseTTL)
		if err := reg.Register(ctx); err != nil {
			return err
		}
		defer func() {
			c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = reg.Deregister(c)
		}()
	}
	errs := make(chan error, 1)
	go func() { errs <- srv.Serve(listener) }()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		h.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
		done := make(chan struct{})
		go func() { srv.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			srv.Stop()
		}
		return nil
	}
}
func RunAISplit(ctx context.Context, cfg config.Config, logger *slog.Logger, role string) error {
	if role == "core-internal" {
		db, sqlDB, err := dependencies.OpenRoleDB(ctx, cfg, cfg.Async.BusinessReadDSN, true)
		if err != nil {
			return err
		}
		defer sqlDB.Close()
		stopMonitor, err := splitMonitor(ctx, cfg, db, nil, logger)
		if err != nil {
			return err
		}
		defer stopMonitor()
		scopeKey, err := serviceauth.Public(cfg.AISplit.ScopePublicKey)
		if err != nil {
			return err
		}
		processingKey, err := serviceauth.Public(cfg.AISplit.ProcessingPublicKey)
		if err != nil {
			return err
		}
		signer, err := storageurl.New(cfg.Storage.Endpoint, cfg.Storage.AccessKey, cfg.Storage.SecretKey, cfg.Storage.Region, cfg.Storage.Secure, "")
		if err != nil {
			return err
		}
		svc := &core.Service{DB: db, ScopeKey: scopeKey, ProcessingKey: processingKey, Presigner: signer}
		return serveInternal(ctx, cfg, logger, "core-internal-v1", cfg.AISplit.CoreAddr, cfg.AISplit.CoreAdvertise, cfg.AISplit.CoreToken, "/yunpan.core.v1.CoreReadService/", func(s *grpc.Server) { corepb.RegisterCoreReadServiceServer(s, svc) })
	}
	db, sqlDB, err := dependencies.OpenAI(ctx, cfg, role)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	coreConn, closeCore, err := internalClient(cfg, "core-internal-v1", cfg.AISplit.CoreTarget, cfg.AISplit.CoreToken)
	if err != nil {
		return err
	}
	defer closeCore()
	defer coreConn.Close()
	coreClient := corepb.NewCoreReadServiceClient(coreConn)
	epoch, err := strconv.ParseInt(cfg.Search.IndexEpoch, 10, 64)
	if err != nil || epoch < 1 {
		epoch = 1
	}
	projector := &aisystem.Projector{DB: db, ES: cfg.Search.ESEndpoint, QD: cfg.Search.QdrantEndpoint, ESUser: cfg.Search.ESUsername, ESPassword: cfg.Search.ESPassword, QDKey: cfg.Search.QdrantAPIKey, Epoch: epoch, HTTP: &http.Client{Timeout: cfg.Search.WriteTimeout}}
	if role == "ai-query" {
		stopMonitor, err := splitMonitor(ctx, cfg, db, nil, logger)
		if err != nil {
			return err
		}
		defer stopMonitor()
		svc := &aisystem.Query{DisableAsk: !cfg.AISplit.Ask, DB: db, Core: coreClient, Provider: ai.NewProvider(cfg.AI), Reranker: ai.NewReranker(cfg.AI), Profile: cfg.AIProfile(), Projector: projector, ScopeLimit: cfg.AISplit.ScopeLimit}
		return serveInternal(ctx, cfg, logger, "ai-query-v2", cfg.AISplit.QueryAddr, cfg.AISplit.QueryAdvertise, cfg.AISplit.QueryToken, "/yunpan.ai.v2.AIQueryService/", func(s *grpc.Server) { pbv2.RegisterAIQueryServiceServer(s, svc) })
	}
	ctrl, ctrlSQL, err := dependencies.OpenRoleDB(ctx, cfg, cfg.Async.ControlDSN, false)
	if err != nil {
		return err
	}
	defer ctrlSQL.Close()
	critical, criticalSQL, err := dependencies.OpenRoleDB(ctx, cfg, cfg.Async.ControlDSN, false)
	if err != nil {
		return err
	}
	defer criticalSQL.Close()
	store := asyncStore(&dependencies.AsyncSet{Control: ctrl, Critical: critical}, cfg)
	aisystem.Register(store.Registry)
	planner := &aisystem.Planner{TextEpoch: cfg.AITextEpoch(), VectorEpoch: cfg.AIVectorEpoch(), TargetNamespace: cfg.AISplit.ProjectionNamespace, TargetEpoch: cfg.AIProjectionEpoch(), Profile: cfg.AIProfile()}
	store.PlanContinuations = planner.Continuations
	store.PlanFailure = planner.Failure
	store.Observe = planner.Observe
	store.AfterTerminal = planner.Finish
	broker := asynccontrol.NewBroker(cfg.TaskQueue.URL, store.Shards, cfg.Async.BrokerPrefix)
	defer broker.Close()
	stopMonitor, err := splitMonitor(ctx, cfg, db, broker, logger)
	if err != nil {
		return err
	}
	defer stopMonitor()
	if err := store.CheckLayout(ctx); err != nil {
		return err
	}
	if role == "ai-committer" {
		publisher := &aisystem.Publisher{DB: db, Control: store}
		return store.RunCommittersFor(ctx, broker, asynccontrol.EffectAI, cfg.App.InstanceID, publisher.Commit, logger)
	}
	parserTarget := cfg.Parser.Target
	closeParserRegistry := func() {}
	if cfg.Registry.Enabled && role == "ai-content-worker" {
		client, e := registry.Open(cfg.Registry)
		if e != nil {
			return e
		}
		closeParserRegistry = func() { _ = client.Close() }
		resolver.Register(registry.NewBuilder(client, logger, registry.NewMetrics(prometheus.NewRegistry())))
		parserTarget = registry.Scheme + "://" + cfg.Parser.Prefix
	}
	defer closeParserRegistry()
	transport, err := serviceauth.ClientTransport(cfg.AISplit.TLS)
	if err != nil {
		return err
	}
	parser, err := parserrpc.NewClient(parserTarget, cfg.Parser.CallTimeout, grpc.WithTransportCredentials(transport), grpc.WithUnaryInterceptor(serviceauth.UnaryClient(cfg.AISplit.ParserToken)))
	if err != nil {
		return err
	}
	defer parser.Close()
	computer := &aisystem.Computer{DB: db, Core: coreClient, Parser: parser, Embedding: ai.NewProvider(cfg.AI), Summary: ai.NewProvider(cfg.AI), Profile: cfg.AIProfile(), HTTP: &http.Client{Timeout: cfg.Parser.CallTimeout}, SummaryCalls: cfg.AISplit.SummaryCalls, Project: projector.Project}
	queue := map[string]string{"ai-content-worker": "ai-content", "ai-embedding-worker": "ai-embedding", "ai-summary-worker": "ai-summary", "ai-text-projector": "ai-project-text", "ai-vector-projector": "ai-project-vector"}[role]
	if queue == "" {
		return fmt.Errorf("unknown AI role %s", role)
	}
	worker := func(c context.Context, q string, n int) error {
		return store.RunWorker(c, broker, q, n, computer.Compute, func(e error) bool { return errors.Is(e, asynccontrol.ErrInvalid) }, logger)
	}
	if role == "ai-content-worker" {
		return runAsyncGroup(ctx, func(c context.Context) error { return worker(c, queue, cfg.Async.Concurrency) }, func(c context.Context) error { return worker(c, "ai-maintenance", 1) })
	}
	return worker(ctx, queue, cfg.Async.Concurrency)
}

// RunAIRole is shared by explicit roles and the narrow legacy aliases.
func RunAIRole(role string) {
	cfg, err := config.LoadForRole(role)
	if err != nil {
		slog.Error("configuration", "error", err)
		os.Exit(1)
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		addr := cfg.Worker.MetricsAddr
		if strings.HasPrefix(addr, ":") {
			addr = "127.0.0.1" + addr
		}
		client := &http.Client{Timeout: 3 * time.Second}
		resp, e := client.Get("http://" + addr + "/health/ready")
		if e != nil {
			os.Exit(1)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			os.Exit(1)
		}
		return
	}
	logger := logging.New(cfg.App.LogLevel, cfg.App.Env).With("process", role, "instance_id", cfg.App.InstanceID)
	slog.SetDefault(logger)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err = RunAISplit(ctx, cfg, logger, role); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("role stopped", "error", err)
		os.Exit(1)
	}
}
func splitMonitor(ctx context.Context, cfg config.Config, db *gorm.DB, broker *asynccontrol.Broker, logger *slog.Logger) (func(), error) {
	if cfg.Worker.MetricsAddr == "" {
		return func() {}, nil
	}
	l, err := net.Listen("tcp", cfg.Worker.MetricsAddr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		c, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		var n int
		e := db.WithContext(c).Raw("SELECT 1").Scan(&n).Error
		if e == nil && broker != nil {
			e = broker.Ready(c)
		}
		if e != nil {
			http.Error(w, "dependencies unavailable", 503)
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "bx_ai_role_up{role=%q} 1\n", cfg.Role)
		if cfg.Role != "core-internal" {
			var rows []struct {
				Capability, LatestBuildState string
				Count                        int64
			}
			if db.WithContext(r.Context()).Table("ai_published_capabilities").Select("capability,latest_build_state,count(*)").Group("capability,latest_build_state").Scan(&rows).Error == nil {
				for _, v := range rows {
					fmt.Fprintf(w, "bx_ai_capabilities{capability=%q,state=%q} %d\n", v.Capability, v.LatestBuildState, v.Count)
				}
			}
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second, WriteTimeout: 5 * time.Second}
	go func() {
		if e := server.Serve(l); e != nil && e != http.ErrServerClosed {
			logger.Error("AI monitor stopped", "error", e)
		}
	}()
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	return func() { stop(); _ = server.Close() }, nil
}
