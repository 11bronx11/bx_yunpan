// Package core exposes read-only business facts over authenticated internal RPC.
package core

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/core/pb"
	"github.com/11bronx11/bx_yunpan/backend/internal/ownerepoch"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/serviceauth"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/storageurl"
)

type Service struct {
	pb.UnimplementedCoreReadServiceServer
	DB                      *gorm.DB
	ScopeKey, ProcessingKey ed25519.PublicKey
	Presigner               *storageurl.Presigner
}
type fileRow struct {
	FileID, ObjectID, FolderID            uuid.UUID
	ContentGeneration, EntryVersion, Size int64
	Name, Path, MimeType                  string
	UpdatedAt                             time.Time
}

func fileProto(f fileRow) *pb.FileRef {
	return &pb.FileRef{FileId: f.FileID.String(), ObjectId: f.ObjectID.String(), FolderId: f.FolderID.String(), ContentGeneration: f.ContentGeneration, EntryVersion: f.EntryVersion, Name: f.Name, Path: f.Path, MimeType: f.MimeType, Size: f.Size, UpdatedAt: f.UpdatedAt.UTC().Format(time.RFC3339Nano)}
}
func invalidScope(ids ...uuid.UUID) error {
	st := status.New(codes.FailedPrecondition, "ai.scope_invalid")
	if len(ids) == 0 {
		return st.Err()
	}
	detail := &errdetails.BadRequest{}
	for _, id := range ids[:min(50, len(ids))] {
		detail.FieldViolations = append(detail.FieldViolations, &errdetails.BadRequest_FieldViolation{Field: id.String(), Description: "unavailable"})
	}
	with, err := st.WithDetails(detail)
	if err != nil {
		return st.Err()
	}
	return with.Err()
}
func (s *Service) grant(ctx context.Context, token string) (aiprotocol.ScopeGrant, error) {
	var g aiprotocol.ScopeGrant
	if serviceauth.Verify(s.ScopeKey, token, "core-retrieval-v1", &g) != nil || g.Principal == uuid.Nil || g.ExpiresAt <= time.Now().Unix() || g.Action != "retrieve" || g.Scope.Normalize() != nil {
		return g, status.Error(codes.PermissionDenied, "invalid scope grant")
	}
	e, err := ownerepoch.Current(s.DB.WithContext(ctx), g.Principal)
	if err != nil {
		return g, status.Error(codes.Unavailable, "core unavailable")
	}
	if e.ContentEpoch != g.ContentEpoch || e.ACLEpoch != g.ACLEpoch {
		return g, status.Error(codes.Aborted, "ai.scope_changed")
	}
	return g, nil
}
func snapshot(g aiprotocol.ScopeGrant) string {
	return aiprotocol.HashJSON([]any{g.Principal, g.Scope, g.ContentEpoch, g.ACLEpoch})
}
func (s *Service) OpenRetrievalScope(ctx context.Context, r *pb.OpenScopeRequest) (*pb.ScopeResponse, error) {
	g, err := s.grant(ctx, r.GetGrant())
	if err != nil {
		return nil, err
	}
	if g.Scope.Mode == "selected" {
		rows, e := s.files(ctx, g, g.Scope.FileIDs)
		if e != nil {
			return nil, e
		}
		if len(rows) != len(g.Scope.FileIDs) {
			missing := []uuid.UUID{}
			for _, id := range g.Scope.FileIDs {
				if !slices.ContainsFunc(rows, func(f fileRow) bool { return f.FileID == id }) {
					missing = append(missing, id)
				}
			}
			return nil, invalidScope(missing...)
		}
	}
	if g.Scope.Mode == "folder" {
		var n int64
		if err := s.DB.WithContext(ctx).Table("folders").Where("id=? AND owner_id=? AND deleted_at IS NULL", *g.Scope.FolderID, g.Principal).Count(&n).Error; err != nil {
			return nil, err
		}
		if n != 1 {
			return nil, invalidScope()
		}
	}
	return &pb.ScopeResponse{SnapshotId: snapshot(g), ContentEpoch: g.ContentEpoch, AclEpoch: g.ACLEpoch, ExpiresAt: g.ExpiresAt}, nil
}

// Folder scope is resolved in business SQL. No authorization replica is sent to
// an external index and all name/path metadata comes from this current scope.
func (s *Service) query(ctx context.Context, g aiprotocol.ScopeGrant) *gorm.DB {
	q := s.DB.WithContext(ctx).Table("file_entries e").Select("e.id AS file_id,e.object_id,e.folder_id,e.version AS entry_version,e.name,(WITH RECURSIVE ancestors AS (SELECT f.id,f.parent_id,f.name,0 AS depth FROM folders f WHERE f.id=e.folder_id AND f.owner_id=e.owner_id AND f.deleted_at IS NULL UNION ALL SELECT f.id,f.parent_id,f.name,a.depth+1 FROM folders f JOIN ancestors a ON f.id=a.parent_id WHERE f.owner_id=e.owner_id AND f.deleted_at IS NULL AND a.depth<128) SELECT '/'||COALESCE(string_agg(name,'/' ORDER BY depth DESC) FILTER(WHERE parent_id IS NOT NULL)||'/','')||e.name FROM ancestors) AS path,e.updated_at,o.content_generation,o.size_bytes AS size,o.mime_type").Joins("JOIN file_objects o ON o.id=e.object_id").Where("e.owner_id=? AND e.deleted_at IS NULL AND o.status='ready'", g.Principal)
	if g.Scope.Mode == "selected" {
		q = q.Where("e.id IN ?", g.Scope.FileIDs)
	}
	if g.Scope.Mode == "folder" {
		if g.Scope.IncludeSubfolders {
			q = q.Where(`e.folder_id IN (WITH RECURSIVE scope_folders AS (SELECT id FROM folders WHERE id=? AND owner_id=? AND deleted_at IS NULL UNION ALL SELECT f.id FROM folders f JOIN scope_folders p ON f.parent_id=p.id WHERE f.owner_id=? AND f.deleted_at IS NULL) SELECT id FROM scope_folders)`, *g.Scope.FolderID, g.Principal, g.Principal)
		} else {
			q = q.Where("e.folder_id=?", *g.Scope.FolderID)
		}
	}
	return q
}
func (s *Service) files(ctx context.Context, g aiprotocol.ScopeGrant, ids []uuid.UUID) ([]fileRow, error) {
	var rows []fileRow
	err := s.query(ctx, g).Where("e.id IN ?", ids).Find(&rows).Error
	return rows, err
}

type cursor struct{ Snapshot, Filter, Name, ID, Updated string }

func (s *Service) StreamScopeMembers(ctx context.Context, r *pb.ScopeMembersRequest) (*pb.ScopeMembersResponse, error) {
	g, err := s.grant(ctx, r.GetGrant())
	if err != nil {
		return nil, err
	}
	limit := int(r.GetLimit())
	if limit <= 0 {
		limit = 100
	}
	if limit > 200 || len(r.Query) > 512 || len(r.MimeTypes) > 30 {
		return nil, status.Error(codes.InvalidArgument, "invalid scope page")
	}
	sort := r.Sort
	if sort == "" {
		sort = "name_asc"
	}
	if sort != "name_asc" && sort != "updated_desc" {
		return nil, status.Error(codes.InvalidArgument, "invalid sort")
	}
	filter := aiprotocol.HashJSON([]any{r.Query, r.MimeTypes, sort})
	var c cursor
	if r.Cursor != "" {
		b, e := base64.RawURLEncoding.DecodeString(r.Cursor)
		if e != nil || json.Unmarshal(b, &c) != nil || c.Snapshot != snapshot(g) || c.Filter != filter {
			return nil, status.Error(codes.Aborted, "ai.scope_changed")
		}
	}
	q := s.query(ctx, g)
	if r.Query != "" {
		q = q.Where("e.name ILIKE ? ESCAPE '\\'", "%"+strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(r.Query)+"%")
	}
	if len(r.MimeTypes) > 0 {
		q = q.Where("o.mime_type = ANY(?::text[])", pq.Array(r.MimeTypes))
	}
	if sort == "name_asc" {
		if c.ID != "" {
			q = q.Where("(e.name,e.id)>(?,?::uuid)", c.Name, c.ID)
		}
		q = q.Order("e.name,e.id")
	} else {
		if c.ID != "" {
			q = q.Where("(e.updated_at,e.id)<(?::timestamptz,?::uuid)", c.Updated, c.ID)
		}
		q = q.Order("e.updated_at DESC,e.id DESC")
	}
	var rows []fileRow
	if err := q.Limit(limit + 1).Find(&rows).Error; err != nil {
		return nil, err
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	out := &pb.ScopeMembersResponse{HasMore: more}
	for _, f := range rows {
		out.Files = append(out.Files, fileProto(f))
	}
	if len(rows) > 0 {
		last := rows[len(rows)-1]
		b, _ := json.Marshal(cursor{snapshot(g), filter, last.Name, last.FileID.String(), last.UpdatedAt.UTC().Format(time.RFC3339Nano)})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	// Epochs are checked again after loading facts; a concurrent change cannot
	// create a successful response containing members from two scope epochs.
	if _, err := s.grant(ctx, r.Grant); err != nil {
		return nil, err
	}
	return out, nil
}
func (s *Service) BatchGetFileRefs(ctx context.Context, r *pb.FileRefsRequest) (*pb.FileRefsResponse, error) {
	g, err := s.grant(ctx, r.Grant)
	if err != nil {
		return nil, err
	}
	if len(r.FileIds) > 100 {
		return nil, status.Error(codes.InvalidArgument, "too many file IDs")
	}
	ids := []uuid.UUID{}
	for _, v := range r.FileIds {
		id, e := uuid.Parse(v)
		if e != nil {
			return nil, invalidScope()
		}
		ids = append(ids, id)
	}
	rows, err := s.files(ctx, g, ids)
	if err != nil {
		return nil, err
	}
	out := &pb.FileRefsResponse{}
	for _, row := range rows {
		out.Files = append(out.Files, fileProto(row))
	}
	return out, nil
}
func (s *Service) ValidateEvidence(ctx context.Context, r *pb.ValidateEvidenceRequest) (*pb.ValidateEvidenceResponse, error) {
	g, err := s.grant(ctx, r.Grant)
	if err != nil {
		return nil, err
	}
	if len(r.Refs) > 240 {
		return nil, status.Error(codes.InvalidArgument, "too many evidence refs")
	}
	ids := []uuid.UUID{}
	for _, v := range r.Refs {
		id, e := uuid.Parse(v.FileId)
		if e != nil {
			return nil, invalidScope()
		}
		ids = append(ids, id)
	}
	rows, err := s.files(ctx, g, ids)
	if err != nil {
		return nil, err
	}
	out := &pb.ValidateEvidenceResponse{SnapshotId: snapshot(g)}
	for _, ref := range r.Refs {
		i := slices.IndexFunc(rows, func(f fileRow) bool {
			return f.FileID.String() == ref.FileId && f.ObjectID.String() == ref.ObjectId && f.ContentGeneration == ref.ContentGeneration && f.EntryVersion == ref.EntryVersion
		})
		if i < 0 {
			return nil, status.Error(codes.Aborted, "ai.scope_changed")
		}
		out.Files = append(out.Files, fileProto(rows[i]))
	}
	if _, err := s.grant(ctx, r.Grant); err != nil {
		return nil, err
	}
	return out, nil
}
func (s *Service) GetObjectForProcessing(ctx context.Context, r *pb.ProcessingRequest) (*pb.ProcessingResponse, error) {
	var g aiprotocol.ProcessingGrant
	if serviceauth.Verify(s.ProcessingKey, r.Grant, "core-processing-v1", &g) != nil || g.Action != "object.read" || g.Deadline <= time.Now().Unix() || g.InputIdentity != r.InputIdentity || g.IntentID.String() != r.IntentId || g.ObjectID.String() != r.ObjectId || g.ContentGeneration != r.ContentGeneration {
		return nil, status.Error(codes.PermissionDenied, "invalid processing grant")
	}
	var o struct {
		Bucket, ObjectKey, SHA256, MimeType string
		SizeBytes                           int64
	}
	q := s.DB.WithContext(ctx).Table("file_objects").Select("bucket,object_key,sha256,mime_type,size_bytes").Where("id=? AND content_generation=? AND sha256=? AND status='ready'", g.ObjectID, g.ContentGeneration, g.ContentHash).Take(&o)
	if q.Error != nil {
		return nil, invalidScope()
	}
	expiry := min(3*time.Minute, time.Until(time.Unix(g.Deadline, 0)))
	if expiry <= 0 || s.Presigner == nil {
		return nil, status.Error(codes.Unavailable, "object authorization unavailable")
	}
	u, err := s.Presigner.PresignedGetObject(ctx, o.Bucket, o.ObjectKey, expiry, nil)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "object authorization unavailable")
	}
	return &pb.ProcessingResponse{ObjectId: g.ObjectID.String(), ContentGeneration: g.ContentGeneration, Sha256: o.SHA256, Size: o.SizeBytes, MimeType: o.MimeType, ReadUrl: u.String(), ExpiresAt: time.Now().Add(expiry).Unix()}, nil
}

var _ = fmt.Sprint

// NameSearch remains available without any AI, control or broker dependency.
func NameSearch(ctx context.Context, s *Service, owner uuid.UUID, scope aiprotocol.Scope, query string, mimes []string, limit int) ([]*pb.FileRef, error) {
	if err := scope.Normalize(); err != nil {
		return nil, err
	}
	q := s.query(ctx, aiprotocol.ScopeGrant{Principal: owner, Scope: scope})
	q = q.Where("e.name ILIKE ? ESCAPE '\\'", "%"+strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(query)+"%")
	if len(mimes) > 0 {
		q = q.Where("o.mime_type = ANY(?::text[])", pq.Array(mimes))
	}
	if limit <= 0 {
		limit = 20
	}
	var rows []fileRow
	if err := q.Order("e.name,e.id").Limit(min(limit, 100)).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []*pb.FileRef{}
	for _, r := range rows {
		out = append(out, fileProto(r))
	}
	return out, nil
}
