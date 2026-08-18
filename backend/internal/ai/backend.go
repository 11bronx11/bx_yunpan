package ai

import (
	"context"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
)

// Backend 是 AI 写路径与重活的抽象：进程内的 *Service 与经 gRPC 调 aisvc 的
// grpcclient.Client 都实现它。API 侧只依赖这个接口，不再直接持有 *Service。
type Backend interface {
	Search(ctx context.Context, ownerID uuid.UUID, input SearchInput) ([]SearchHit, error)
	Ask(ctx context.Context, ownerID uuid.UUID, question string, folderID *uuid.UUID, fileIDs []uuid.UUID) (string, []Citation, error)
	RequestReprocess(ctx context.Context, ownerID, fileID uuid.UUID, idempotencyKey string) (Task, error)
}

var _ Backend = (*Service)(nil)

// DisabledBackend keeps the core drive API available when AI is explicitly
// disabled. AI write/search routes fail immediately instead of waiting for a
// gRPC connection to a service that is intentionally not running.
type DisabledBackend struct{}

var _ Backend = DisabledBackend{}

func (DisabledBackend) Search(context.Context, uuid.UUID, SearchInput) ([]SearchHit, error) {
	return nil, ErrUnavailable
}

func (DisabledBackend) Ask(context.Context, uuid.UUID, string, *uuid.UUID, []uuid.UUID) (string, []Citation, error) {
	return "", nil, ErrUnavailable
}

func (DisabledBackend) RequestReprocess(context.Context, uuid.UUID, uuid.UUID, string) (Task, error) {
	return Task{}, ErrUnavailable
}

// Reader 承载 AI 文档与任务的只读查询。这两条路径只读 ai_documents 与
// async_tasks，不写 ai_* 表，因此 API 进程可以直接查库，不必绕一次 RPC。
type Reader struct {
	db    *gorm.DB
	drive *drive.Service
}

func NewReader(db *gorm.DB, driveService *drive.Service) *Reader {
	return &Reader{db: db, drive: driveService}
}

func (r *Reader) GetDocument(ownerID, fileID uuid.UUID) (drive.FileView, Document, error) {
	file, err := r.drive.File(ownerID, fileID)
	if err != nil {
		return drive.FileView{}, Document{}, ErrNotFound
	}
	var document Document
	if err := r.db.Where("object_id = ?", file.ObjectID).First(&document).Error; err != nil {
		return file, Document{}, ErrNotFound
	}
	return file, document, nil
}

func (r *Reader) GetTask(ownerID, taskID uuid.UUID) (Task, error) {
	var task Task
	if err := r.db.Where("id = ? AND owner_id = ?", taskID, ownerID).First(&task).Error; err != nil {
		return Task{}, ErrNotFound
	}
	return task, nil
}
