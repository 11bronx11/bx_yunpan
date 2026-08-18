package grpcserver

import (
	"context"

	"github.com/google/uuid"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pb"
)

// Server 是 pb.AIServiceServer 的实现，只做协议转换与限流，业务逻辑全部
// 转调既有 ai.Service。限流放在这一层：多副本 API 各自计数会让总量失控，
// 放在 aisvc 侧才能真正保护下游 LLM 配额。
type Server struct {
	pb.UnimplementedAIServiceServer
	service *ai.Service
	limiter ai.RequestLimiter
}

func New(service *ai.Service, limiter ai.RequestLimiter) *Server {
	return &Server{service: service, limiter: limiter}
}

func (s *Server) Search(request *pb.SearchRequest, stream pb.AIService_SearchServer) error {
	ctx := stream.Context()
	ownerID, err := uuid.Parse(request.GetOwnerId())
	if err != nil {
		return ai.ErrInvalid
	}
	if err := s.allow(ctx, ownerID, "search"); err != nil {
		return err
	}
	folderID, err := optionalUUID(request.FolderId)
	if err != nil {
		return ai.ErrInvalid
	}
	hits, err := s.service.Search(ctx, ownerID, ai.SearchInput{
		Query:             request.GetQuery(),
		Mode:              request.GetMode(),
		FolderID:          folderID,
		IncludeSubfolders: request.GetIncludeSubfolders(),
		MimeTypes:         request.GetMimeTypes(),
		Limit:             int(request.GetLimit()),
	})
	if err != nil {
		return err
	}
	for _, hit := range hits {
		if err := stream.Send(hitToProto(hit)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) Ask(ctx context.Context, request *pb.AskRequest) (*pb.AskResponse, error) {
	ownerID, err := uuid.Parse(request.GetOwnerId())
	if err != nil {
		return nil, ai.ErrInvalid
	}
	if err := s.allow(ctx, ownerID, "ask"); err != nil {
		return nil, err
	}
	folderID, err := optionalUUID(request.FolderId)
	if err != nil {
		return nil, ai.ErrInvalid
	}
	fileIDs := make([]uuid.UUID, 0, len(request.GetFileIds()))
	for _, raw := range request.GetFileIds() {
		fileID, err := uuid.Parse(raw)
		if err != nil {
			return nil, ai.ErrInvalid
		}
		fileIDs = append(fileIDs, fileID)
	}
	answer, citations, err := s.service.Ask(ctx, ownerID, request.GetQuestion(), folderID, fileIDs)
	if err != nil {
		return nil, err
	}
	return &pb.AskResponse{Answer: answer, Citations: citationsToProto(citations)}, nil
}

func (s *Server) Reprocess(ctx context.Context, request *pb.ReprocessRequest) (*pb.ReprocessResponse, error) {
	ownerID, err := uuid.Parse(request.GetOwnerId())
	if err != nil {
		return nil, ai.ErrInvalid
	}
	if err := s.allow(ctx, ownerID, "reprocess"); err != nil {
		return nil, err
	}
	fileID, err := uuid.Parse(request.GetFileId())
	if err != nil {
		return nil, ai.ErrNotFound
	}
	task, err := s.service.RequestReprocess(ctx, ownerID, fileID, request.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	return &pb.ReprocessResponse{Task: taskToProto(task)}, nil
}

// allow 在限流后端故障时放行，与既有 HTTP 中间件的降级行为一致：
// 限流是保护措施，不该因为 Redis 抖动直接拒绝业务。
func (s *Server) allow(ctx context.Context, ownerID uuid.UUID, scope string) error {
	if s.limiter == nil {
		return nil
	}
	allowed, err := s.limiter.Allow(ctx, ownerID, scope)
	if err != nil {
		return nil
	}
	if !allowed {
		return ai.ErrRateLimited
	}
	return nil
}

func optionalUUID(raw *string) (*uuid.UUID, error) {
	if raw == nil || *raw == "" {
		return nil, nil
	}
	parsed, err := uuid.Parse(*raw)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
