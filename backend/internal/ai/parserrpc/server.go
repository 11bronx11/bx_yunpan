package parserrpc

import (
	"context"
	"errors"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pb"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const MaxRequestBytes = 41 << 20

type Server struct {
	pb.UnimplementedDocumentParserServiceServer
	parser ai.ParserGateway
	sem    chan struct{}
}

func NewServer(parser ai.ParserGateway, maxConcurrent int) *Server {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	return &Server{parser: parser, sem: make(chan struct{}, maxConcurrent)}
}

func (s *Server) ParseDocument(ctx context.Context, request *pb.ParseDocumentRequest) (*pb.ParseDocumentResponse, error) {
	if request == nil {
		return nil, status.Error(codes.InvalidArgument, "ai.parse.invalid_request")
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	if s.parser == nil {
		return nil, status.Error(codes.FailedPrecondition, "ai.parse.unavailable")
	}

	parsed, err := s.parser.Parse(ctx, ParseRequestFromProto(request))
	if err != nil {
		return nil, parserStatus(err)
	}
	return ParseResultToProto(parsed), nil
}

func parserStatus(err error) error {
	var parseErr *ai.ParseError
	if !errors.As(err, &parseErr) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return status.FromContextError(err).Err()
		}
		return status.Error(codes.Internal, "ai.parse.failed")
	}
	switch {
	case parseErr.Unsupported:
		return status.Error(codes.Unimplemented, parseErr.Code)
	case parseErr.ResourceLimit:
		return status.Error(codes.ResourceExhausted, parseErr.Code)
	case parseErr.Retryable:
		if errors.Is(parseErr, context.Canceled) || errors.Is(parseErr, context.DeadlineExceeded) {
			return status.FromContextError(parseErr).Err()
		}
		return status.Error(codes.Unavailable, parseErr.Code)
	case parseErr.Permanent:
		return status.Error(codes.FailedPrecondition, parseErr.Code)
	case parseErr.Code == "ai.parse.invalid_request":
		return status.Error(codes.InvalidArgument, parseErr.Code)
	default:
		return status.Error(codes.Internal, parseErr.Code)
	}
}

func ParseRequestFromProto(request *pb.ParseDocumentRequest) ai.ParseRequest {
	return ai.ParseRequest{
		RunID:                parseOptionalUUID(request.GetRunId()),
		ObjectID:             parseOptionalUUID(request.GetObjectId()),
		DocumentVersionID:    parseOptionalUUID(request.GetDocumentVersionId()),
		ContentHash:          request.GetContentHash(),
		MediaType:            request.GetMediaType(),
		ObjectAddress:        ai.ObjectReadAddress{MediaType: request.GetMediaType(), Data: request.GetContent()},
		ParserProfileVersion: request.GetParserProfileVersion(),
		SizeLimitBytes:       request.GetSizeLimitBytes(),
		PageLimit:            int(request.GetPageLimit()),
		PixelLimit:           request.GetPixelLimit(),
		ChunkTokenBudget:     int(request.GetChunkTokenBudget()),
		ChunkOverlapTokens:   int(request.GetChunkOverlapTokens()),
		MaxChunks:            int(request.GetMaxChunks()),
		ChunkerVersion:       request.GetChunkerVersion(),
		TokenizerID:          request.GetTokenizerId(),
		TraceID:              request.GetTraceId(),
	}
}

func ParseResultToProto(result ai.ParseResult) *pb.ParseDocumentResponse {
	response := &pb.ParseDocumentResponse{
		ArtifactUri: result.ArtifactURI, ArtifactHash: result.ArtifactHash,
		ParserId: result.ParserID, ParserVersion: result.ParserVersion, Language: result.Language,
		PageCount: int32(result.PageCount), BlockCount: int32(result.BlockCount),
		Quality: &pb.ParseQuality{
			TextBytes: int32(result.Quality.TextBytes), TextDensity: result.Quality.TextDensity,
			EmptyPageRatio: result.Quality.EmptyPageRatio, ReplacementRunes: int32(result.Quality.ReplacementRunes),
		},
		Warnings:       append([]string(nil), result.Warnings...),
		ChunkerVersion: result.ChunkerVersion, TokenizerId: result.TokenizerID,
		ChunksTruncated: result.ChunksTruncated,
	}
	for _, section := range result.Sections {
		item := &pb.ParsedSection{
			Text: section.Text, SourceKind: section.SourceKind, Section: section.Section, Kind: section.Kind, HeadingPath: section.HeadingPath,
			PageEnd: int32(section.PageEnd), SlideNumber: int32(section.SlideNumber), SheetName: section.SheetName,
			RowStart: int32(section.RowStart), RowEnd: int32(section.RowEnd),
			ColumnStart: int32(section.ColumnStart), ColumnEnd: int32(section.ColumnEnd),
			SourceStart: section.SourceStart, SourceEnd: section.SourceEnd, HasHeader: section.HasHeader,
			MergedRanges: append([]string(nil), section.MergedRanges...), Encoding: section.Encoding,
		}
		if section.PageNumber != nil {
			item.PageNumber = int32(*section.PageNumber)
		}
		response.Sections = append(response.Sections, item)
	}
	for _, chunk := range result.Chunks {
		response.Chunks = append(response.Chunks, &pb.ParsedChunk{
			Content: chunk.Content, SourceKind: chunk.SourceKind, Kind: chunk.Kind, HeadingPath: chunk.HeadingPath,
			PageStart: int32(chunk.PageStart), PageEnd: int32(chunk.PageEnd), SlideNumber: int32(chunk.SlideNumber),
			SheetName: chunk.SheetName, RowStart: int32(chunk.RowStart), RowEnd: int32(chunk.RowEnd),
			ColumnStart: int32(chunk.ColumnStart), ColumnEnd: int32(chunk.ColumnEnd),
			SourceStart: chunk.SourceStart, SourceEnd: chunk.SourceEnd,
			EstimatedTokenCount: int32(chunk.TokenCount), TokenCountIsEstimate: chunk.TokenCountIsEstimate,
			MergedRanges: append([]string(nil), chunk.MergedRanges...), Encoding: chunk.Encoding,
		})
	}
	return response
}

func parseOptionalUUID(value string) uuid.UUID {
	parsed, _ := uuid.Parse(value)
	return parsed
}

var _ pb.DocumentParserServiceServer = (*Server)(nil)
