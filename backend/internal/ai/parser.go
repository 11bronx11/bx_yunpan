package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/google/uuid"
)

// ObjectReadAddress is deliberately an internal, read-only handle. Parser
// implementations must not receive a database writer or an authorization
// decision; the indexer owns the object bytes and the resulting AI version.
type ObjectReadAddress struct {
	MediaType string
	Data      []byte
}

// Section is the smallest parser output consumed by the existing chunker.
// It is an alias so the gateway does not create a second document model.
type Section = rag.Section

type ParseRequest struct {
	RunID                uuid.UUID
	ObjectID             uuid.UUID
	DocumentVersionID    uuid.UUID
	ContentHash          string
	MediaType            string
	ObjectAddress        ObjectReadAddress
	ParserProfileVersion string
	SizeLimitBytes       int64
	PageLimit            int
	PixelLimit           int64
	ChunkTokenBudget     int
	ChunkOverlapTokens   int
	MaxChunks            int
	ChunkerVersion       string
	TokenizerID          string
	TraceID              string
}

// IndexMetadata is carried by asynchronous payloads during replay. The
// object row and document-version lease remain authoritative; these fields
// provide stable correlation and allow old payloads to be upgraded in place.
type IndexMetadata struct {
	RunID                *uuid.UUID
	ContentHash          string
	DocumentVersionID    *uuid.UUID
	PipelineVersion      string
	ModelVersion         string
	ParserProfileVersion string
	TraceID              string
}

type ParseQuality struct {
	TextBytes        int     `json:"text_bytes"`
	TextDensity      float64 `json:"text_density"`
	EmptyPageRatio   float64 `json:"empty_page_ratio"`
	ReplacementRunes int     `json:"replacement_runes"`
}

type ParseResult struct {
	ArtifactURI        string       `json:"artifact_uri,omitempty"`
	ArtifactHash       string       `json:"artifact_hash,omitempty"`
	ParserID           string       `json:"parser_id"`
	ParserVersion      string       `json:"parser_version"`
	VisionModelVersion string       `json:"vision_model_version,omitempty"`
	Language           string       `json:"language,omitempty"`
	PageCount          int          `json:"page_count"`
	BlockCount         int          `json:"block_count"`
	Sections           []Section    `json:"sections"`
	Chunks             []rag.Chunk  `json:"chunks,omitempty"`
	ChunkerVersion     string       `json:"chunker_version,omitempty"`
	TokenizerID        string       `json:"tokenizer_id,omitempty"`
	ChunksTruncated    bool         `json:"chunks_truncated,omitempty"`
	Quality            ParseQuality `json:"quality"`
	Warnings           []string     `json:"warnings,omitempty"`
}

type ParserGateway interface {
	Parse(context.Context, ParseRequest) (ParseResult, error)
}

// ChunkingParser keeps parsing and chunking inside the dedicated parser
// process. The indexer receives ready-to-embed chunks with source metadata.
type ChunkingParser struct {
	inner   ParserGateway
	options rag.ChunkOptions
}

func NewChunkingParser(inner ParserGateway, options rag.ChunkOptions) *ChunkingParser {
	return &ChunkingParser{inner: inner, options: options}
}

func (p *ChunkingParser) Parse(ctx context.Context, request ParseRequest) (ParseResult, error) {
	if p == nil || p.inner == nil {
		return ParseResult{}, &ParseError{Code: "ai.parse.unavailable", Retryable: true}
	}
	result, err := p.inner.Parse(ctx, request)
	if err != nil {
		return ParseResult{}, err
	}
	if len(result.Chunks) != 0 {
		return result, nil
	}
	options := p.options
	if request.ChunkTokenBudget > 0 {
		options.TokenBudget = request.ChunkTokenBudget
	}
	if request.ChunkOverlapTokens > 0 {
		options.Overlap = request.ChunkOverlapTokens
	}
	if request.MaxChunks > 0 {
		options.MaxChunks = request.MaxChunks
	}
	if request.ChunkerVersion != "" {
		options.Version = request.ChunkerVersion
	}
	if request.TokenizerID != "" {
		options.TokenizerID = request.TokenizerID
	}
	chunked := rag.MakeTypedChunks(result.Sections, options)
	result.Chunks = chunked.Chunks
	result.ChunkerVersion = chunked.Version
	result.TokenizerID = chunked.TokenizerID
	result.ChunksTruncated = chunked.Truncated
	result.Warnings = append(result.Warnings, chunked.Warnings...)
	if chunked.Truncated {
		if len(chunked.Warnings) == 0 {
			result.Warnings = append(result.Warnings, "chunk limit reached; document was not fully indexed")
		}
	}
	return result, nil
}

// FallbackParser uses the built-in parser only when the primary parser reports
// an unsupported format. Transport failures remain retryable instead of
// silently moving CPU-heavy work back into the indexer process.
type FallbackParser struct {
	primary  ParserGateway
	fallback ParserGateway
}

func NewFallbackParser(primary, fallback ParserGateway) *FallbackParser {
	return &FallbackParser{primary: primary, fallback: fallback}
}

func (p *FallbackParser) Parse(ctx context.Context, request ParseRequest) (ParseResult, error) {
	if p == nil || p.primary == nil {
		if p != nil && p.fallback != nil {
			return p.fallback.Parse(ctx, request)
		}
		return ParseResult{}, &ParseError{Code: "ai.parse.unavailable", Retryable: true}
	}
	result, err := p.primary.Parse(ctx, request)
	if !IsParseUnsupported(err) || p.fallback == nil {
		return result, err
	}
	fallbackResult, fallbackErr := p.fallback.Parse(ctx, request)
	if fallbackErr != nil {
		return ParseResult{}, fallbackErr
	}
	fallbackResult.Warnings = append(fallbackResult.Warnings, "primary parser unsupported; built-in fallback used")
	return fallbackResult, nil
}

type ParseError struct {
	Code          string
	Retryable     bool
	Unsupported   bool
	Permanent     bool
	ResourceLimit bool
	Err           error
}

func (e *ParseError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return e.Code
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Err)
}

func (e *ParseError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func IsParseUnsupported(err error) bool {
	var parseErr *ParseError
	return errors.As(err, &parseErr) && parseErr.Unsupported
}

func IsParseRetryable(err error) bool {
	var parseErr *ParseError
	return errors.As(err, &parseErr) && parseErr.Retryable
}

func IsParsePermanent(err error) bool {
	var parseErr *ParseError
	return errors.As(err, &parseErr) && parseErr.Permanent
}

func IsParseResourceLimit(err error) bool {
	var parseErr *ParseError
	return errors.As(err, &parseErr) && parseErr.ResourceLimit
}

func parseFailureCode(err error) string {
	var parseErr *ParseError
	if errors.As(err, &parseErr) && strings.TrimSpace(parseErr.Code) != "" {
		return parseErr.Code
	}
	return "ai.parse.failed"
}

func parseProfile(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "builtin-v1"
	}
	return value
}
