package grpcserver

import (
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pb"
)

func hitToProto(hit ai.SearchHit) *pb.SearchHit {
	return &pb.SearchHit{
		File:      fileToProto(hit.File),
		Score:     hit.Score,
		MatchType: hit.MatchType,
		Citations: citationsToProto(hit.Citations),
	}
}

// SearchHit.File 是 map[string]any，由 ai.candidateFile 构造，字段固定。
func fileToProto(file map[string]any) *pb.FileRef {
	if file == nil {
		return nil
	}
	return &pb.FileRef{
		Id:        uuidField(file, "id"),
		OwnerId:   uuidField(file, "owner_id"),
		FolderId:  uuidField(file, "folder_id"),
		ObjectId:  uuidField(file, "object_id"),
		Name:      stringField(file, "name"),
		SizeBytes: int64Field(file, "size_bytes"),
		MimeType:  stringField(file, "mime_type"),
		Version:   int64Field(file, "version"),
		CreatedAt: timeField(file, "created_at"),
		UpdatedAt: timeField(file, "updated_at"),
	}
}

func citationsToProto(citations []ai.Citation) []*pb.Citation {
	converted := make([]*pb.Citation, 0, len(citations))
	for _, citation := range citations {
		item := &pb.Citation{
			Id:       citation.ID,
			FileId:   citation.FileID.String(),
			FileName: citation.FileName,
			Excerpt:  citation.Excerpt,
		}
		if citation.PageNumber != nil {
			page := int32(*citation.PageNumber)
			item.PageNumber = &page
		}
		if citation.Section != nil {
			item.Section = citation.Section
		}
		converted = append(converted, item)
	}
	return converted
}

func taskToProto(task ai.Task) *pb.Task {
	converted := &pb.Task{
		Id:           task.ID.String(),
		TaskType:     task.TaskType,
		ResourceType: task.ResourceType,
		ResourceId:   task.ResourceID.String(),
		Status:       task.Status,
		Progress:     int32(task.Progress),
		Attempt:      int32(task.Attempt),
		CreatedAt:    timestamppb.New(task.CreatedAt),
		UpdatedAt:    timestamppb.New(task.UpdatedAt),
	}
	converted.ErrorCode = task.ErrorCode
	converted.ErrorMessage = task.ErrorMessage
	return converted
}

func uuidField(file map[string]any, key string) string {
	if value, ok := file[key].(uuid.UUID); ok {
		return value.String()
	}
	return ""
}

func stringField(file map[string]any, key string) string {
	value, _ := file[key].(string)
	return value
}

func int64Field(file map[string]any, key string) int64 {
	value, _ := file[key].(int64)
	return value
}

func timeField(file map[string]any, key string) *timestamppb.Timestamp {
	if value, ok := file[key].(time.Time); ok {
		return timestamppb.New(value)
	}
	return nil
}
