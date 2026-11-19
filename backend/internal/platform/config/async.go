package config

import (
	"errors"
	"strings"
	"time"
)

type Async struct {
	BrokerPrefix, LegacyBrokerPrefix                                 string
	MaxAttempts, MaxDispatches, StagingDays                          int
	TaskLifetime                                                     time.Duration
	ControlDSN, BusinessReadDSN                                      string
	ControlMaxOpen, ControlMaxIdle, CriticalMaxOpen                  int
	RelayRows, RelayBytes                                            int
	PollMin, PollMax, Lease                                          time.Duration
	Concurrency                                                      int
	ScanInterval                                                     time.Duration
	ScanBatch                                                        int
	RetryBase, RetryCap, CommitWindow, OutputSafety, DetailRetention time.Duration
}

func loadAsync() Async {
	return Async{
		BrokerPrefix:       env("ASYNC_BROKER_PREFIX", "bx.async.v2"),
		LegacyBrokerPrefix: env("ASYNC_LEGACY_BROKER_PREFIX", "bx.async.v1"),
		MaxAttempts:        envInt("ASYNC_MAX_ATTEMPTS", 12), MaxDispatches: envInt("ASYNC_MAX_DISPATCHES", 20), StagingDays: envInt("ASYNC_STAGING_DAYS", 14), TaskLifetime: envDuration("ASYNC_TASK_LIFETIME", 24*time.Hour),
		ControlDSN: env("ASYNC_CONTROL_DSN", ""), BusinessReadDSN: env("POSTGRES_READONLY_DSN", ""),
		ControlMaxOpen: envInt("ASYNC_CONTROL_MAX_OPEN_CONNS", 8), ControlMaxIdle: envInt("ASYNC_CONTROL_MAX_IDLE_CONNS", 2), CriticalMaxOpen: envInt("ASYNC_CONTROL_CRITICAL_CONNS", 2),
		RelayRows: envInt("ASYNC_RELAY_ROWS", 200), RelayBytes: envInt("ASYNC_RELAY_BYTES", 1<<20), PollMin: envDuration("ASYNC_POLL_MIN", 100*time.Millisecond), PollMax: envDuration("ASYNC_POLL_MAX", 2*time.Second), Lease: envDuration("ASYNC_LEASE", 30*time.Second),
		Concurrency:  envInt("ASYNC_WORKER_CONCURRENCY", 2),
		ScanInterval: envDuration("ASYNC_GC_SCAN_INTERVAL", 5*time.Minute), ScanBatch: envInt("ASYNC_GC_SCAN_BATCH", 100), RetryBase: envDuration("ASYNC_RETRY_BASE", 5*time.Second), RetryCap: envDuration("ASYNC_RETRY_CAP", 5*time.Minute), CommitWindow: envDuration("ASYNC_COMMIT_WINDOW", 24*time.Hour), OutputSafety: envDuration("ASYNC_OUTPUT_SAFETY", time.Hour), DetailRetention: envDuration("ASYNC_DETAIL_RETENTION", 7*24*time.Hour),
	}
}

func (c Config) validateAsync() error {
	a := c.Async
	if a.BrokerPrefix == "bx.async.v1" {
		return errors.New("ASYNC_BROKER_PREFIX must use an isolated v2 topology; old messages require async-handover")
	}
	if strings.TrimSpace(c.App.InstanceID) == "" || c.Postgres.MaxOpenConns < 1 || c.Postgres.MaxIdleConns < 0 || c.Postgres.MaxIdleConns > c.Postgres.MaxOpenConns {
		return errors.New("invalid async instance or business pool bounds")
	}
	if c.Role == "upload-worker" || c.Role == "object-worker" || c.Role == "media-worker" || c.Role == "gc-worker" || c.Role == "maintenance-worker" || c.Role == "async-import" {
		if c.Storage.Endpoint == "" || c.Storage.Bucket == "" || c.Storage.AccessKey == "" || c.Storage.SecretKey == "" {
			return errors.New("storage endpoint, bucket and credentials are required for storage roles")
		}
	}
	if strings.TrimSpace(a.ControlDSN) == "" || strings.TrimSpace(a.BusinessReadDSN) == "" {
		return errors.New("ASYNC_CONTROL_DSN and POSTGRES_READONLY_DSN are required for async roles")
	}
	if a.ControlDSN == a.BusinessReadDSN || a.ControlDSN == c.Postgres.PrimaryDSN {
		return errors.New("control database must be independent of business database")
	}
	if c.TaskQueue.URL == "" {
		return errors.New("RABBITMQ_URL is required for async roles")
	}
	if a.MaxAttempts < 1 || a.MaxDispatches < 1 || a.StagingDays < 1 || a.TaskLifetime <= 0 || time.Duration(a.StagingDays)*24*time.Hour <= c.Upload.SessionTTL+a.TaskLifetime+a.CommitWindow+a.OutputSafety {
		return errors.New("invalid async retry/lifecycle budgets")
	}
	if err := a.validateRuntime(); err != nil {
		return err
	}

	if (c.Role == "upload-worker" || c.Role == "object-worker" || c.Role == "media-worker" || c.Role == "gc-worker") && c.Postgres.PrimaryDSN == "" {
		return errors.New("domain worker requires business write DSN")
	}
	return nil
}

func (a Async) validateRuntime() error {
	if a.ControlMaxOpen < 1 || a.ControlMaxIdle < 0 || a.ControlMaxIdle > a.ControlMaxOpen || a.CriticalMaxOpen < 1 || a.RelayRows < 1 || a.RelayRows > 2000 || a.RelayBytes < 1024 || a.RelayBytes > 16<<20 || a.Concurrency < 1 || a.Concurrency > 128 || a.ScanBatch < 1 {
		return errors.New("invalid async bounds")
	}
	if a.PollMin <= 0 || a.PollMax < a.PollMin || a.Lease < 3*time.Second || a.RetryBase <= 0 || a.RetryCap < a.RetryBase || a.CommitWindow <= 0 || a.OutputSafety < 10*time.Minute || a.ScanInterval <= 0 || a.DetailRetention <= a.CommitWindow {
		return errors.New("invalid async time budgets")
	}
	if a.MaxAttempts < 1 || a.MaxDispatches < 1 {
		return errors.New("invalid async attempt budgets")
	}
	return nil
}
