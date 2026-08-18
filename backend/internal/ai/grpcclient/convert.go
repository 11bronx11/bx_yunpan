package grpcclient

import (
	"time"

	"github.com/google/uuid"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pb"
)

// hitFromProto 还原成 ai.SearchHit。File 必须保持与 ai.candidateFile 一致的
// map 形状与值类型（uuid.UUID / int64 / time.Time），否则 HTTP JSON 契约会变。
func hitFromProto(hit *pb.SearchHit) ai.SearchHit {
	return ai.SearchHit{
		File:      fileFromProto(hit.GetFile()),
		Score:     hit.GetScore(),
		MatchType: hit.GetMatchType(),
		Citations: citationsFromProto(hit.GetCitations()),
	}
}

func fileFromProto(file *pb.FileRef) map[string]any {
	if file == nil {
		return nil
	}
	return map[string]any{
		"type":       "file",
		"id":         parseUUID(file.GetId()),
		"owner_id":   parseUUID(file.GetOwnerId()),
		"folder_id":  parseUUID(file.GetFolderId()),
		"object_id":  parseUUID(file.GetObjectId()),
		"name":       file.GetName(),
		"size_bytes": file.GetSizeBytes(),
		"mime_type":  file.GetMimeType(),
		"version":    file.GetVersion(),
		"created_at": parseTime(file.GetCreatedAt().AsTime(), file.GetCreatedAt() != nil),
		"updated_at": parseTime(file.GetUpdatedAt().AsTime(), file.GetUpdatedAt() != nil),
	}
}

func citationsFromProto(citations []*pb.Citation) []ai.Citation {
	converted := make([]ai.Citation, 0, len(citations))
	for _, citation := range citations {
		item := ai.Citation{
			ID:       citation.GetId(),
			FileID:   parseUUID(citation.GetFileId()),
			FileName: citation.GetFileName(),
			Excerpt:  citation.GetExcerpt(),
		}
		if citation.PageNumber != nil {
			page := int(citation.GetPageNumber())
			item.PageNumber = &page
		}
		if citation.Section != nil {
			section := citation.GetSection()
			item.Section = &section
		}
		converted = append(converted, item)
	}
	return converted
}

func taskFromProto(task *pb.Task) ai.Task {
	if task == nil {
		return ai.Task{}
	}
	converted := ai.Task{
		ID:           parseUUID(task.GetId()),
		TaskType:     task.GetTaskType(),
		ResourceType: task.GetResourceType(),
		ResourceID:   parseUUID(task.GetResourceId()),
		Status:       task.GetStatus(),
		Progress:     int(task.GetProgress()),
		Attempt:      int(task.GetAttempt()),
		ErrorCode:    task.ErrorCode,
		ErrorMessage: task.ErrorMessage,
	}
	if task.GetCreatedAt() != nil {
		converted.CreatedAt = task.GetCreatedAt().AsTime()
	}
	if task.GetUpdatedAt() != nil {
		converted.UpdatedAt = task.GetUpdatedAt().AsTime()
	}
	return converted
}

func parseUUID(raw string) uuid.UUID {
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil
	}
	return parsed
}

func parseTime(value time.Time, present bool) time.Time {
	if !present {
		return time.Time{}
	}
	return value
}
