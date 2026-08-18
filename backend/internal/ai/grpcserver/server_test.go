package grpcserver

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pb"
)

// recordingStream 收集 Send 出去的消息，便于断言流式返回。
type recordingStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent []*pb.SearchHit
}

func (s *recordingStream) Context() context.Context { return s.ctx }

func (s *recordingStream) Send(hit *pb.SearchHit) error {
	s.sent = append(s.sent, hit)
	return nil
}

type stubLimiter struct {
	allowed bool
	err     error
	scopes  []string
}

func (l *stubLimiter) Allow(_ context.Context, _ uuid.UUID, scope string) (bool, error) {
	l.scopes = append(l.scopes, scope)
	return l.allowed, l.err
}

// owner_id 非法 UUID 必须在进业务逻辑前被拒，service 为 nil 也不会 panic。
func TestSearchRejectsInvalidOwnerID(t *testing.T) {
	server := New(nil, nil)
	stream := &recordingStream{ctx: context.Background()}
	err := server.Search(&pb.SearchRequest{OwnerId: "not-a-uuid"}, stream)
	if err != ai.ErrInvalid {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestAskRejectsInvalidFileID(t *testing.T) {
	server := New(nil, nil)
	_, err := server.Ask(context.Background(), &pb.AskRequest{
		OwnerId: uuid.New().String(), Question: "问题", FileIds: []string{"bad"},
	})
	if err != ai.ErrInvalid {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// 限流在 aisvc 侧生效，拒绝时返回 ErrRateLimited 而非 ErrQuota。
func TestLimiterRejectionReturnsRateLimited(t *testing.T) {
	limiter := &stubLimiter{allowed: false}
	server := New(nil, limiter)

	stream := &recordingStream{ctx: context.Background()}
	if err := server.Search(&pb.SearchRequest{OwnerId: uuid.New().String()}, stream); err != ai.ErrRateLimited {
		t.Fatalf("Search err = %v, want ErrRateLimited", err)
	}
	if _, err := server.Ask(context.Background(), &pb.AskRequest{OwnerId: uuid.New().String()}); err != ai.ErrRateLimited {
		t.Fatalf("Ask err = %v, want ErrRateLimited", err)
	}
	if _, err := server.Reprocess(context.Background(), &pb.ReprocessRequest{OwnerId: uuid.New().String()}); err != ai.ErrRateLimited {
		t.Fatalf("Reprocess err = %v, want ErrRateLimited", err)
	}
	want := []string{"search", "ask", "reprocess"}
	for index, scope := range want {
		if limiter.scopes[index] != scope {
			t.Errorf("scope[%d] = %s, want %s", index, limiter.scopes[index], scope)
		}
	}
}

// 限流后端故障时放行：限流是保护措施，不该因 Redis 抖动拒绝业务。
func TestLimiterErrorFailsOpen(t *testing.T) {
	limiter := &stubLimiter{allowed: false, err: context.DeadlineExceeded}
	server := New(nil, limiter)
	// 放行后走到 uuid.Parse(file_id) 才失败，说明没有被限流挡住。
	_, err := server.Reprocess(context.Background(), &pb.ReprocessRequest{
		OwnerId: uuid.New().String(), FileId: "bad",
	})
	if err != ai.ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound (limiter should fail open)", err)
	}
}

// 转换层要保证 map 形状与值类型稳定，否则 HTTP JSON 契约会变。
func TestHitToProtoPreservesFields(t *testing.T) {
	fileID := uuid.New()
	now := time.Now().UTC().Truncate(time.Second)
	page := 3
	section := "第一章"
	hit := ai.SearchHit{
		File: map[string]any{
			"type": "file", "id": fileID, "owner_id": uuid.New(), "folder_id": uuid.New(),
			"object_id": uuid.New(), "name": "报告.pdf", "size_bytes": int64(2048),
			"mime_type": "application/pdf", "version": int64(7),
			"created_at": now, "updated_at": now,
		},
		Score: 0.75, MatchType: "hybrid",
		Citations: []ai.Citation{{
			ID: "c1", FileID: fileID, FileName: "报告.pdf",
			PageNumber: &page, Section: &section, Excerpt: "摘录",
		}},
	}

	converted := hitToProto(hit)
	if converted.GetFile().GetId() != fileID.String() {
		t.Errorf("file id = %s", converted.GetFile().GetId())
	}
	if converted.GetFile().GetSizeBytes() != 2048 || converted.GetFile().GetVersion() != 7 {
		t.Error("size or version lost")
	}
	if !converted.GetFile().GetCreatedAt().AsTime().Equal(now) {
		t.Error("created_at lost")
	}
	if converted.GetScore() != 0.75 || converted.GetMatchType() != "hybrid" {
		t.Error("score or match type lost")
	}
	citation := converted.GetCitations()[0]
	if citation.GetPageNumber() != 3 || citation.GetSection() != section || citation.GetExcerpt() != "摘录" {
		t.Error("citation fields lost")
	}
}

// 可选字段缺失时不应被填成零值假数据。
func TestCitationOptionalFieldsStayUnset(t *testing.T) {
	converted := citationsToProto([]ai.Citation{{ID: "c1", FileID: uuid.New()}})
	if converted[0].PageNumber != nil {
		t.Error("page_number should stay unset")
	}
	if converted[0].Section != nil {
		t.Error("section should stay unset")
	}
}

func TestTaskToProtoCarriesTimestamps(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	code := "ai.reprocess_failed"
	task := ai.Task{
		ID: uuid.New(), TaskType: "ai.reprocess", ResourceType: "file", ResourceID: uuid.New(),
		Status: "failed", Progress: 100, Attempt: 2, ErrorCode: &code,
		CreatedAt: now, UpdatedAt: now,
	}
	converted := taskToProto(task)
	if converted.GetErrorCode() != code {
		t.Errorf("error_code = %s", converted.GetErrorCode())
	}
	if converted.ErrorMessage != nil {
		t.Error("error_message should stay unset")
	}
	if !converted.GetUpdatedAt().AsTime().Equal(now) {
		t.Error("updated_at lost")
	}
	if converted.GetCreatedAt() == nil || converted.GetCreatedAt().AsTime() != timestamppb.New(now).AsTime() {
		t.Error("created_at lost")
	}
}
