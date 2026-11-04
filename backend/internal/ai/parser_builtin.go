package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

type BuiltinParser struct{ provider Provider }

func NewBuiltinParser(provider Provider) *BuiltinParser { return &BuiltinParser{provider: provider} }

func (p *BuiltinParser) Parse(ctx context.Context, request ParseRequest) (ParseResult, error) {
	if request.ObjectID == uuid.Nil || len(request.ObjectAddress.Data) == 0 {
		return ParseResult{}, &ParseError{Code: "ai.parse.invalid_request", Err: errors.New("object and data are required")}
	}
	if request.SizeLimitBytes > 0 && int64(len(request.ObjectAddress.Data)) > request.SizeLimitBytes {
		return ParseResult{}, &ParseError{Code: "ai.parse.resource_limit", ResourceLimit: true, Err: errors.New("object exceeds size limit")}
	}
	sections, err := extractWithLimits(ctx, p.provider, request.MediaType, request.ObjectAddress.Data, request.PageLimit, request.PixelLimit)
	if err != nil {
		if errors.Is(err, errResourceLimit) {
			return ParseResult{}, &ParseError{Code: "ai.parse.resource_limit", ResourceLimit: true, Err: err}
		}
		if errors.Is(err, errQualityFailure) {
			return ParseResult{}, &ParseError{Code: "ai.parse.quality_failed", Permanent: true, Err: err}
		}
		if errors.Is(err, errUnsupported) {
			return ParseResult{}, &ParseError{Code: "ai.parse.unsupported", Unsupported: true, Err: err}
		}
		if errors.Is(err, errInvalidInput) {
			return ParseResult{}, &ParseError{Code: "ai.parse.invalid_content", Permanent: true, Err: err}
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return ParseResult{}, &ParseError{Code: "ai.parse.timeout", Retryable: true, Err: err}
		}
		return ParseResult{}, &ParseError{Code: "ai.parse.failed", Retryable: true, Err: err}
	}
	quality := qualityOf(sections)
	if quality.TextBytes == 0 {
		return ParseResult{}, &ParseError{Code: "ai.parse.quality_failed", Permanent: true, Err: errors.New("parsed content is empty")}
	}
	digest := sha256.Sum256(request.ObjectAddress.Data)
	return ParseResult{
		ArtifactHash: hex.EncodeToString(digest[:]), ParserID: "builtin", ParserVersion: parseProfile(request.ParserProfileVersion),
		Sections: sections, PageCount: countPages(sections), BlockCount: len(sections), Quality: quality,
	}, nil
}

func qualityOf(sections []extractedSection) ParseQuality {
	bytes, replacements := 0, 0
	for _, section := range sections {
		bytes += len([]byte(section.Text))
		replacements += strings.Count(section.Text, "\uFFFD")
	}
	density := 0.0
	if bytes > 0 {
		density = float64(len([]rune(strings.Join(sectionTexts(sections), " ")))) / float64(bytes)
	}
	return ParseQuality{TextBytes: bytes, TextDensity: density, ReplacementRunes: replacements}
}

func sectionTexts(sections []extractedSection) []string {
	result := make([]string, len(sections))
	for i := range sections {
		result[i] = sections[i].Text
	}
	return result
}
func countPages(sections []extractedSection) int {
	pages := 0
	for _, section := range sections {
		if section.PageNumber != nil && *section.PageNumber > pages {
			pages = *section.PageNumber
		}
	}
	return pages
}

var _ ParserGateway = (*BuiltinParser)(nil)

// Keep the UTF-8 check close to quality code so parser implementations cannot
// accidentally treat replacement characters as high quality text.
func validParsedText(sections []extractedSection) bool {
	for _, section := range sections {
		if !utf8.ValidString(section.Text) {
			return false
		}
	}
	return true
}
