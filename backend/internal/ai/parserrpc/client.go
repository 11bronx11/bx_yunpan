package parserrpc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pb"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const MaxResponseBytes = 8 << 20

type Client struct {
	conn    *grpc.ClientConn
	client  pb.DocumentParserServiceClient
	timeout time.Duration
}

func NewClient(target string, timeout time.Duration) (*Client, error) {
	if target == "" {
		return nil, errors.New("document parser gRPC target is required")
	}
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallSendMsgSize(MaxRequestBytes),
			grpc.MaxCallRecvMsgSize(MaxResponseBytes),
		),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		return nil, fmt.Errorf("create document parser client: %w", err)
	}
	return &Client{conn: conn, client: pb.NewDocumentParserServiceClient(conn), timeout: timeout}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) Parse(ctx context.Context, request ai.ParseRequest) (ai.ParseResult, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	response, err := c.client.ParseDocument(ctx, &pb.ParseDocumentRequest{
		RunId: request.RunID.String(), ObjectId: request.ObjectID.String(),
		DocumentVersionId: request.DocumentVersionID.String(), ContentHash: request.ContentHash,
		MediaType: request.MediaType, Content: request.ObjectAddress.Data,
		ParserProfileVersion: request.ParserProfileVersion, SizeLimitBytes: request.SizeLimitBytes,
		PageLimit: int32(request.PageLimit), PixelLimit: request.PixelLimit, TraceId: request.TraceID,
		ChunkTokenBudget: int32(request.ChunkTokenBudget), ChunkOverlapTokens: int32(request.ChunkOverlapTokens),
		MaxChunks: int32(request.MaxChunks), ChunkerVersion: request.ChunkerVersion, TokenizerId: request.TokenizerID,
	})
	if err != nil {
		return ai.ParseResult{}, parserError(err)
	}
	return ParseResultFromProto(response), nil
}

func parserError(err error) error {
	message := status.Convert(err).Message()
	switch status.Code(err) {
	case codes.Unimplemented:
		return &ai.ParseError{Code: "ai.parse.unsupported", Unsupported: true, Err: errors.New(message)}
	case codes.ResourceExhausted:
		return &ai.ParseError{Code: message, ResourceLimit: true, Err: errors.New(message)}
	case codes.FailedPrecondition:
		return &ai.ParseError{Code: message, Permanent: true, Err: errors.New(message)}
	case codes.InvalidArgument:
		return &ai.ParseError{Code: message, Err: errors.New(message)}
	case codes.Canceled, codes.DeadlineExceeded:
		return &ai.ParseError{Code: "ai.parse.timeout", Retryable: true, Err: err}
	case codes.Unavailable, codes.Internal, codes.Unknown:
		return &ai.ParseError{Code: "ai.parse.failed", Retryable: true, Err: err}
	default:
		return &ai.ParseError{Code: "ai.parse.failed", Err: err}
	}
}

func ParseResultFromProto(response *pb.ParseDocumentResponse) ai.ParseResult {
	if response == nil {
		return ai.ParseResult{}
	}
	result := ai.ParseResult{
		ArtifactURI: response.GetArtifactUri(), ArtifactHash: response.GetArtifactHash(),
		ParserID: response.GetParserId(), ParserVersion: response.GetParserVersion(),
		Language: response.GetLanguage(), PageCount: int(response.GetPageCount()),
		BlockCount: int(response.GetBlockCount()), Warnings: append([]string(nil), response.GetWarnings()...),
		ChunkerVersion: response.GetChunkerVersion(), TokenizerID: response.GetTokenizerId(),
		ChunksTruncated: response.GetChunksTruncated(),
	}
	if quality := response.GetQuality(); quality != nil {
		result.Quality = ai.ParseQuality{
			TextBytes: int(quality.GetTextBytes()), TextDensity: quality.GetTextDensity(),
			EmptyPageRatio: quality.GetEmptyPageRatio(), ReplacementRunes: int(quality.GetReplacementRunes()),
		}
	}
	for _, item := range response.GetSections() {
		section := ai.Section{
			Text: item.GetText(), SourceKind: item.GetSourceKind(), Section: item.GetSection(), Kind: item.GetKind(), HeadingPath: item.GetHeadingPath(),
			PageEnd: int(item.GetPageEnd()), SlideNumber: int(item.GetSlideNumber()), SheetName: item.GetSheetName(),
			RowStart: int(item.GetRowStart()), RowEnd: int(item.GetRowEnd()),
			ColumnStart: int(item.GetColumnStart()), ColumnEnd: int(item.GetColumnEnd()),
			SourceStart: item.GetSourceStart(), SourceEnd: item.GetSourceEnd(), HasHeader: item.GetHasHeader(),
			MergedRanges: append([]string(nil), item.GetMergedRanges()...), Encoding: item.GetEncoding(),
		}
		if item.GetPageNumber() > 0 {
			page := int(item.GetPageNumber())
			section.PageNumber = &page
		}
		result.Sections = append(result.Sections, section)
	}
	for _, item := range response.GetChunks() {
		chunk := rag.Chunk{
			Content: item.GetContent(), SourceKind: item.GetSourceKind(), Kind: item.GetKind(), HeadingPath: item.GetHeadingPath(),
			PageStart: int(item.GetPageStart()), PageEnd: int(item.GetPageEnd()), SlideNumber: int(item.GetSlideNumber()),
			SheetName: item.GetSheetName(), RowStart: int(item.GetRowStart()), RowEnd: int(item.GetRowEnd()),
			ColumnStart: int(item.GetColumnStart()), ColumnEnd: int(item.GetColumnEnd()),
			SourceStart: item.GetSourceStart(), SourceEnd: item.GetSourceEnd(),
			TokenCount: int(item.GetEstimatedTokenCount()), TokenCountIsEstimate: item.GetTokenCountIsEstimate(),
			MergedRanges: append([]string(nil), item.GetMergedRanges()...), Encoding: item.GetEncoding(),
		}
		if item.GetPageStart() > 0 {
			page := int(item.GetPageStart())
			chunk.PageNumber = &page
		}
		result.Chunks = append(result.Chunks, chunk)
	}
	return result
}

var _ ai.ParserGateway = (*Client)(nil)
