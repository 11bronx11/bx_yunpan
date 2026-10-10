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
	TextBytes        int
	TextDensity      float64
	EmptyPageRatio   float64
	ReplacementRunes int
}

type ParseResult struct {
	ArtifactURI   string
	ArtifactHash  string
	ParserID      string
	ParserVersion string
	Language      string
	PageCount     int
	BlockCount    int
	Sections      []Section
	Quality       ParseQuality
	Warnings      []string
}

type ParserGateway interface {
	Parse(context.Context, ParseRequest) (ParseResult, error)
}

type ParseError struct {
	Code        string
	Retryable   bool
	Unsupported bool
	Err         error
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

func parseProfile(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "builtin-v1"
	}
	return value
}
