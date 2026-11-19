package config

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/serviceauth"
)

type AISplit struct {
	Ask                                                                           bool
	ProjectionNamespace, TextIndexEpoch, VectorIndexEpoch                         string
	TLS                                                                           serviceauth.TLS
	QueryDSN, ComputeDSN, CommitDSN, MigrationDSN                                 string
	CoreAddr, CoreAdvertise, CoreTarget, QueryAddr, QueryAdvertise, QueryTarget   string
	CoreToken, QueryToken, ParserToken                                            string
	ScopeSigningSeed, ScopePublicKey, ProcessingSigningSeed, ProcessingPublicKey  string
	ProfileID, ProfileRevision, EmbeddingRevision, SummaryRevision, PromptVersion string
	Vectors, Summary                                                              bool
	QueryTimeout, AskTimeout, ReadTimeout                                         time.Duration
	ScopeLimit, SummaryCalls                                                      int
}

func loadAISplit() AISplit {
	return AISplit{
		Ask:                 envBool("AI_ASK_ENABLED", true),
		ProjectionNamespace: env("AI_PROJECTION_NAMESPACE", "bx-ai"), TextIndexEpoch: env("AI_TEXT_INDEX_EPOCH", env("SEARCH_INDEX_EPOCH", "v1")), VectorIndexEpoch: env("AI_VECTOR_INDEX_EPOCH", env("SEARCH_INDEX_EPOCH", "v1")),
		TLS:      serviceauth.TLS{CA: env("INTERNAL_GRPC_TLS_CA", ""), Cert: env("INTERNAL_GRPC_TLS_CERT", ""), Key: env("INTERNAL_GRPC_TLS_KEY", ""), ServerName: env("INTERNAL_GRPC_TLS_SERVER_NAME", "")},
		QueryDSN: env("AI_QUERY_DSN", ""), ComputeDSN: env("AI_COMPUTE_DSN", ""), CommitDSN: env("AI_COMMIT_DSN", ""), MigrationDSN: env("AI_MIGRATION_DSN", ""),
		CoreAddr: env("CORE_INTERNAL_GRPC_ADDR", ":51053"), CoreAdvertise: env("CORE_INTERNAL_ADVERTISE_ADDR", "core-internal:51053"), CoreTarget: env("CORE_INTERNAL_TARGET", "core-internal:51053"),
		QueryAddr: env("AI_QUERY_GRPC_ADDR", ":51054"), QueryAdvertise: env("AI_QUERY_ADVERTISE_ADDR", "ai-query:51054"), QueryTarget: env("AI_QUERY_TARGET", "ai-query:51054"),
		CoreToken: env("CORE_INTERNAL_SERVICE_TOKEN", ""), QueryToken: env("AI_QUERY_SERVICE_TOKEN", ""), ParserToken: env("PARSER_SERVICE_TOKEN", ""),
		ScopeSigningSeed: env("AI_SCOPE_SIGNING_SEED", ""), ScopePublicKey: env("AI_SCOPE_PUBLIC_KEY", ""), ProcessingSigningSeed: env("AI_PROCESSING_SIGNING_SEED", ""), ProcessingPublicKey: env("AI_PROCESSING_PUBLIC_KEY", ""),
		ProfileID: env("AI_PROFILE_ID", "default"), ProfileRevision: env("AI_PROFILE_REVISION", "v1"), EmbeddingRevision: env("AI_EMBEDDING_REVISION", "config-v1"), SummaryRevision: env("AI_SUMMARY_REVISION", "config-v1"), PromptVersion: env("AI_SUMMARY_PROMPT_VERSION", "overview-v1"),
		Vectors: envBool("AI_VECTORS_ENABLED", true), Summary: envBool("AI_SUMMARY_ENABLED", true), QueryTimeout: envDuration("AI_QUERY_TIMEOUT", 10*time.Second), AskTimeout: envDuration("AI_ASK_TIMEOUT", 60*time.Second), ReadTimeout: envDuration("CORE_INTERNAL_TIMEOUT", 5*time.Second), ScopeLimit: envInt("AI_SCOPE_MAX_FILES", 10000), SummaryCalls: envInt("AI_SUMMARY_MAX_CALLS", 128),
	}
}

func (c Config) AIProfile() aiprotocol.Profile {
	return aiprotocol.Profile{ID: c.AISplit.ProfileID, Revision: c.AISplit.ProfileRevision, Parser: c.Parser.ProfileVersion, Chunker: rag.ParentChildVersion, EmbeddingProvider: c.AI.Provider, EmbeddingModel: c.AI.EmbeddingModel, EmbeddingRevision: c.AISplit.EmbeddingRevision, SummaryProvider: c.AI.Provider, SummaryModel: c.AI.ChatModel, SummaryRevision: c.AISplit.SummaryRevision, VisionProvider: c.AI.Provider, VisionModel: c.AI.VisionModel, Prompt: c.AISplit.PromptVersion, Dimension: c.AI.Dimension, EnableVectors: c.AISplit.Vectors, EnableSummary: c.AISplit.Summary}
}

func isSplitRole(role string) bool {
	return role == "core-internal" || role == "ai-query" || role == "ai-committer" || strings.HasPrefix(role, "ai-") && strings.HasSuffix(role, "-worker") || role == "ai-text-projector" || role == "ai-vector-projector"
}

func (c Config) validateAISplit() error {
	a := c.AISplit
	if err := a.TLS.Validate(); err != nil {
		return err
	}
	if a.CoreToken == "" || a.ScopePublicKey == "" || a.ProcessingPublicKey == "" {
		return errors.New("internal service token and grant public keys required")
	}
	if c.Role == "core-internal" {
		if c.Async.BusinessReadDSN == "" {
			return errors.New("core-internal requires business readonly DSN")
		}
		return nil
	}
	dsn := a.CommitDSN
	if c.Role == "ai-query" {
		dsn = a.QueryDSN
	}
	if c.Role == "ai-committer" {
		dsn = a.CommitDSN
	}
	if dsn == "" || dsn == c.Postgres.PrimaryDSN || dsn == c.Async.ControlDSN {
		return errors.New("explicit independent AI DSN required")
	}
	if c.AI.Dimension != 1024 || a.ScopeLimit < 50 || a.ScopeLimit > 100000 || a.SummaryCalls < 2 {
		return errors.New("invalid AI dimension or resource budgets")
	}
	if c.AI.Provider != "fake" && c.AI.Provider != "dashscope" {
		return errors.New("AI_PROVIDER must be fake or dashscope")
	}
	if c.AI.RequestTimeout <= 0 || a.ReadTimeout <= 0 {
		return errors.New("AI model and core read timeouts must be positive")
	}
	needsModel := (c.Role == "ai-embedding-worker" && a.Vectors) || (c.Role == "ai-summary-worker" && a.Summary) || (c.Role == "ai-query" && (a.Ask || a.Vectors || c.AI.RerankEnabled))
	if needsModel && c.AI.Provider == "dashscope" && c.AI.APIKey == "" {
		return errors.New("configured model role requires provider credentials")
	}
	if c.Role == "ai-query" {
		if a.QueryToken == "" {
			return errors.New("AI query service token required")
		}
		if a.QueryTimeout <= 0 || a.AskTimeout <= 0 || c.AI.RerankCandidates <= 0 || c.AI.RerankTopK <= 0 || c.AI.RerankTopK > c.AI.RerankCandidates || c.AI.RerankTimeout <= 0 {
			return errors.New("invalid AI query or rerank budgets")
		}
		if c.AI.RerankEnabled && (strings.TrimSpace(c.AI.RerankModel) == "" || c.AI.Provider == "dashscope" && strings.TrimSpace(c.AI.RerankBaseURL) == "") {
			return errors.New("enabled reranker requires model and provider endpoint")
		}
		if c.Search.ReadBackend != "postgres" && c.Search.ReadBackend != "es_qdrant" && c.Search.ReadBackend != "shadow" {
			return errors.New("invalid AI search read backend")
		}
		if c.Search.ExternalTimeout <= 0 || c.Search.ShadowSampleRate < 0 || c.Search.ShadowSampleRate > 1 {
			return errors.New("invalid AI external search budgets")
		}
		return nil
	}
	if c.Role == "ai-content-worker" && c.AI.MaxObjectBytes <= 0 {
		return errors.New("AI content object size limit must be positive")
	}
	if (c.Role == "ai-text-projector" || c.Role == "ai-vector-projector") && c.Search.WriteTimeout <= 0 {
		return errors.New("AI projection write timeout must be positive")
	}
	if err := c.Async.validateRuntime(); err != nil {
		return err
	}
	if c.Async.BrokerPrefix == c.Async.LegacyBrokerPrefix {
		return errors.New("AI worker needs a distinct v2 broker prefix")
	}
	if c.Async.ControlDSN == "" || c.TaskQueue.URL == "" {
		return errors.New("AI asynchronous roles require control PG and RabbitMQ")
	}
	return nil
}

// AIProjectionEpoch accepts the established v1 form and positive numeric epochs.
func (c Config) AIProjectionEpoch() int64 {
	epoch, err := strconv.ParseInt(strings.TrimPrefix(c.Search.IndexEpoch, "v"), 10, 64)
	if err != nil || epoch < 1 {
		return 1
	}
	return epoch
}

func (c Config) AITextEpoch() int64 {
	epoch, _ := strconv.ParseInt(strings.TrimPrefix(c.AISplit.TextIndexEpoch, "v"), 10, 64)
	return max(1, epoch)
}

func (c Config) AIVectorEpoch() int64 {
	epoch, _ := strconv.ParseInt(strings.TrimPrefix(c.AISplit.VectorIndexEpoch, "v"), 10, 64)
	return max(1, epoch)
}
