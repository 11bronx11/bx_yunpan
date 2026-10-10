package drive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/ownerepoch"
	platformcache "github.com/11bronx11/bx_yunpan/backend/internal/platform/cache"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/dblock"
)

var (
	ErrNotFound     = errors.New("drive item not found")
	ErrConflict     = errors.New("drive conflict")
	ErrInvalidInput = errors.New("invalid drive input")
	ErrNotEmpty     = errors.New("folder is not empty")
)

type Service struct {
	db    *gorm.DB
	cache *platformcache.JSONStore
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

func NewServiceWithCache(db *gorm.DB, store *platformcache.JSONStore) *Service {
	return &Service{db: db, cache: store}
}

type childrenCache struct {
	Folders []Folder
	Files   []FileView
}

var (
	ErrInvalidCursor = errors.New("invalid drive cursor")
	ErrCursorStale   = errors.New("drive cursor is stale")
)

type childPage struct {
	Folders []Folder
	Files   []FileView
	HasMore bool
	Cursor  string
}

type childCursor struct {
	Version string    `json:"v"`
	Scope   string    `json:"s"`
	Content int64     `json:"e"`
	ACL     int64     `json:"a"`
	Kind    int       `json:"k"`
	Name    string    `json:"n"`
	ID      uuid.UUID `json:"i"`
	Sort    string    `json:"o"`
}

type childItem struct {
	kind   int
	name   string
	id     uuid.UUID
	folder *Folder
	file   *FileView
}

const (
	childFolder = iota
	childFile
	childCursorVersion = "1"
)

func (s *Service) ProvisionUser(tx *gorm.DB, userID uuid.UUID) error {
	root := Folder{
		ID:             uuid.Must(uuid.NewV7()),
		OwnerID:        userID,
		Name:           "/",
		NameNormalized: "/",
		Version:        1,
	}
	if err := tx.Create(&root).Error; err != nil {
		return err
	}
	return ownerepoch.Ensure(tx, userID)
}

func (s *Service) Root(ownerID uuid.UUID) (Folder, error) {
	var folder Folder
	if err := s.db.Where("owner_id = ? AND parent_id IS NULL AND deleted_at IS NULL", ownerID).First(&folder).Error; err != nil {
		return Folder{}, ErrNotFound
	}
	return folder, nil
}

func (s *Service) Folder(ownerID, folderID uuid.UUID) (Folder, error) {
	return s.folder(ownerID, folderID)
}

func (s *Service) Children(ownerID, folderID uuid.UUID) ([]Folder, []FileView, error) {
	if _, err := s.folder(ownerID, folderID); err != nil {
		return nil, nil, err
	}
	if s.cache != nil {
		key := s.readCacheKey(ownerID, folderID, "children")
		var cached childrenCache
		err := s.cache.Load(context.Background(), key, &cached, func() (any, error) {
			folders, files, err := s.loadChildren(ownerID, folderID)
			return childrenCache{Folders: folders, Files: files}, err
		})
		if err == nil {
			return cached.Folders, cached.Files, nil
		}
	}
	return s.loadChildren(ownerID, folderID)
}

// ChildrenPage uses a bounded keyset query. The cursor is fenced by the
// owner's committed epochs: a write invalidates an in-flight page sequence so
// clients restart from page one instead of silently skipping or duplicating rows.
func (s *Service) ChildrenPage(ctx context.Context, ownerID, folderID uuid.UUID, rawCursor string, limit int, sortName string) (folders []Folder, files []FileView, more bool, next string, err error) {
	folders, files, more, next, err = s.childrenPage(ctx, ownerID, folderID, rawCursor, limit, sortName)
	if rawCursor != "" || !errors.Is(err, ErrCursorStale) {
		return
	}
	// A first page has no previous cursor to invalidate. Under concurrent
	// writes, read its rows and epoch in one bounded database snapshot.
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		reader := NewService(tx)
		var readErr error
		folders, files, more, next, readErr = reader.childrenPage(ctx, ownerID, folderID, "", limit, sortName)
		return readErr
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return
}

func (s *Service) childrenPage(ctx context.Context, ownerID, folderID uuid.UUID, rawCursor string, limit int, sortName string) ([]Folder, []FileView, bool, string, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 {
		return nil, nil, false, "", ErrInvalidInput
	}
	if sortName == "" {
		sortName = "name_asc"
	}
	if sortName != "name_asc" && sortName != "name_desc" {
		return nil, nil, false, "", ErrInvalidInput
	}
	var folder Folder
	if err := s.db.WithContext(ctx).Where("id = ? AND owner_id = ? AND deleted_at IS NULL", folderID, ownerID).Take(&folder).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			err = ErrNotFound
		}
		return nil, nil, false, "", err
	}
	epoch, err := s.currentEpoch(ctx, ownerID)
	if err != nil {
		return nil, nil, false, "", err
	}
	scope := cursorScope(ownerID, folderID)
	var cursor *childCursor
	if rawCursor != "" {
		parsed, parseErr := decodeChildCursor(rawCursor)
		if parseErr != nil || parsed.Scope != scope || parsed.Sort != sortName {
			return nil, nil, false, "", ErrInvalidCursor
		}
		if parsed.Content != epoch.ContentEpoch || parsed.ACL != epoch.ACLEpoch {
			return nil, nil, false, "", ErrCursorStale
		}
		cursor = &parsed
	}

	load := func() (childPage, error) {
		items, queryErr := s.loadChildrenPage(ctx, ownerID, folderID, cursor, limit, sortName)
		if queryErr != nil {
			return childPage{}, queryErr
		}
		current, epochErr := s.currentEpoch(ctx, ownerID)
		if epochErr != nil {
			return childPage{}, epochErr
		}
		if current.ContentEpoch != epoch.ContentEpoch || current.ACLEpoch != epoch.ACLEpoch {
			return childPage{}, ErrCursorStale
		}
		page := childPage{}
		if len(items) > limit {
			page.HasMore = true
			items = items[:limit]
		}
		if page.HasMore && len(items) > 0 {
			last := items[len(items)-1]
			page.Cursor, _ = encodeChildCursor(childCursor{Version: childCursorVersion, Scope: scope, Content: epoch.ContentEpoch, ACL: epoch.ACLEpoch, Kind: last.kind, Name: last.name, ID: last.id, Sort: sortName})
		}
		for _, item := range items {
			if item.folder != nil {
				page.Folders = append(page.Folders, *item.folder)
			} else if item.file != nil {
				page.Files = append(page.Files, *item.file)
			}
		}
		return page, nil
	}

	// Only the first page is cacheable. Cursor pages are already bounded and
	// their token is tied to a specific snapshot, so caching them would add
	// little benefit and complicate invalidation.
	if cursor == nil && s.cache != nil {
		key := "drive:v3:children-page:" + ownerID.String() + ":" + folderID.String() + ":" +
			strconv.FormatInt(epoch.ContentEpoch, 10) + ":" + strconv.FormatInt(epoch.ACLEpoch, 10) + ":" + strconv.Itoa(limit) + ":" + sortName
		var cached childPage
		if cacheErr := s.cache.Load(ctx, key, &cached, func() (any, error) { return load() }); cacheErr == nil {
			current, epochErr := s.currentEpoch(ctx, ownerID)
			if epochErr != nil {
				return nil, nil, false, "", epochErr
			}
			if current.ContentEpoch != epoch.ContentEpoch || current.ACLEpoch != epoch.ACLEpoch {
				return nil, nil, false, "", ErrCursorStale
			}
			return cached.Folders, cached.Files, cached.HasMore, cached.Cursor, nil
		} else if errors.Is(cacheErr, ErrCursorStale) {
			return nil, nil, false, "", cacheErr
		}
	}
	page, err := load()
	if err != nil {
		return nil, nil, false, "", err
	}
	return page.Folders, page.Files, page.HasMore, page.Cursor, nil
}

func (s *Service) currentEpoch(ctx context.Context, ownerID uuid.UUID) (ownerepoch.Epoch, error) {
	var epoch ownerepoch.Epoch
	if err := s.db.WithContext(ctx).Select("content_epoch", "acl_epoch").Where("owner_id = ?", ownerID).Take(&epoch).Error; err != nil {
		return ownerepoch.Epoch{}, err
	}
	return epoch, nil
}

func cursorScope(ownerID, folderID uuid.UUID) string {
	hash := sha256.Sum256([]byte(ownerID.String() + ":" + folderID.String()))
	return hex.EncodeToString(hash[:8])
}

func encodeChildCursor(cursor childCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeChildCursor(raw string) (childCursor, error) {
	if len(raw) == 0 || len(raw) > 4096 {
		return childCursor{}, ErrInvalidCursor
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil {
		return childCursor{}, ErrInvalidCursor
	}
	var cursor childCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.Version != childCursorVersion || cursor.ID == uuid.Nil || cursor.Name == "" || cursor.Scope == "" {
		return childCursor{}, ErrInvalidCursor
	}
	if cursor.Kind != childFolder && cursor.Kind != childFile {
		return childCursor{}, ErrInvalidCursor
	}
	return cursor, nil
}

func (s *Service) loadChildrenPage(ctx context.Context, ownerID, folderID uuid.UUID, cursor *childCursor, limit int, sortName string) ([]childItem, error) {
	items := make([]childItem, 0, (limit+1)*2)
	var folders []Folder
	folderQuery := s.db.WithContext(ctx).Where("owner_id = ? AND parent_id = ? AND deleted_at IS NULL", ownerID, folderID)
	var files []FileView
	fileQuery := s.db.WithContext(ctx).Table("file_entries e").
		Select("e.*, o.size_bytes, o.mime_type, o.sha256, d.status AS ai_status").
		Joins("JOIN file_objects o ON o.id = e.object_id AND o.status = 'ready'").
		Joins("LEFT JOIN ai_documents d ON d.object_id = e.object_id").
		Where("e.owner_id = ? AND e.folder_id = ? AND e.deleted_at IS NULL", ownerID, folderID)
	desc := sortName == "name_desc"
	if cursor != nil {
		if cursor.Kind == childFolder {
			if desc {
				folderQuery = folderQuery.Where("(name_normalized < ? OR (name_normalized = ? AND id < ?))", cursor.Name, cursor.Name, cursor.ID)
			} else {
				folderQuery = folderQuery.Where("(name_normalized > ? OR (name_normalized = ? AND id > ?))", cursor.Name, cursor.Name, cursor.ID)
			}
		} else if cursor.Kind == childFile {
			folderQuery = folderQuery.Where("1 = 0")
			if desc {
				fileQuery = fileQuery.Where("(e.name_normalized < ? OR (e.name_normalized = ? AND e.id < ?))", cursor.Name, cursor.Name, cursor.ID)
			} else {
				fileQuery = fileQuery.Where("(e.name_normalized > ? OR (e.name_normalized = ? AND e.id > ?))", cursor.Name, cursor.Name, cursor.ID)
			}
		}
	}
	folderOrder, fileOrder := "name_normalized ASC, id ASC", "e.name_normalized ASC, e.id ASC"
	if desc {
		folderOrder, fileOrder = "name_normalized DESC, id DESC", "e.name_normalized DESC, e.id DESC"
	}
	if err := folderQuery.Order(folderOrder).Limit(limit + 1).Find(&folders).Error; err != nil {
		return nil, err
	}
	if err := fileQuery.Order(fileOrder).Limit(limit + 1).Scan(&files).Error; err != nil {
		return nil, err
	}
	for i := range folders {
		folder := folders[i]
		items = append(items, childItem{kind: childFolder, name: folder.NameNormalized, id: folder.ID, folder: &folder})
	}
	for i := range files {
		file := files[i]
		items = append(items, childItem{kind: childFile, name: file.NameNormalized, id: file.ID, file: &file})
	}
	// Each query is ordered by PostgreSQL's collation. Concatenation preserves
	// that order within each kind and keeps directories ahead of files.
	return items, nil
}

func (s *Service) loadChildren(ownerID, folderID uuid.UUID) ([]Folder, []FileView, error) {
	var folders []Folder
	err := s.db.Where("owner_id = ? AND parent_id = ? AND deleted_at IS NULL", ownerID, folderID).
		Order("name_normalized ASC").Find(&folders).Error
	if err != nil {
		return nil, nil, err
	}
	var files []FileView
	err = s.db.Raw(`
	        SELECT e.*, o.size_bytes, o.mime_type, o.sha256, d.status AS ai_status
	        FROM file_entries e
	        JOIN file_objects o ON o.id = e.object_id AND o.status = 'ready'
	        LEFT JOIN ai_documents d ON d.object_id = e.object_id
	        WHERE e.owner_id = ? AND e.folder_id = ? AND e.deleted_at IS NULL
	        ORDER BY e.name_normalized ASC`, ownerID, folderID).Scan(&files).Error
	return folders, files, err
}

func (s *Service) clearFolder(ownerID, folderID uuid.UUID) {
	if s.cache != nil {
		_ = s.cache.Delete(context.Background(), s.readCacheKey(ownerID, folderID, "children"))
	}
}

func (s *Service) clearFile(ownerID, fileID uuid.UUID) {
	if s.cache != nil {
		_ = s.cache.Delete(context.Background(), s.readCacheKey(ownerID, fileID, "file"))
	}
}

// Read the committed epoch on every lookup. A writer on another API/worker
// invalidates both cache levels atomically with its business transaction.
func (s *Service) readCacheKey(ownerID, itemID uuid.UUID, kind string) string {
	var epoch ownerepoch.Epoch
	if err := s.db.Select("content_epoch", "acl_epoch").Where("owner_id = ?", ownerID).Take(&epoch).Error; err != nil {
		return ""
	}
	return "drive:v2:" + kind + ":" + ownerID.String() + ":" + itemID.String() + ":" +
		strconv.FormatInt(epoch.ContentEpoch, 10) + ":" + strconv.FormatInt(epoch.ACLEpoch, 10)
}

func (s *Service) FileNameExists(ownerID, folderID uuid.UUID, name string) (bool, error) {
	_, normalized, err := normalizeName(name)
	if err != nil {
		return false, err
	}
	var count int64
	err = s.db.Model(&FileEntry{}).
		Where("owner_id = ? AND folder_id = ? AND name_normalized = ? AND deleted_at IS NULL", ownerID, folderID, normalized).
		Count(&count).Error
	return count > 0, err
}

func (s *Service) Breadcrumb(ownerID, folderID uuid.UUID) ([]Folder, error) {
	var folders []Folder
	err := s.db.Raw(`
        WITH RECURSIVE path AS (
            SELECT f.*, 0 AS depth
            FROM folders f
            WHERE f.id = ? AND f.owner_id = ? AND f.deleted_at IS NULL
            UNION ALL
            SELECT parent.*, child.depth + 1
            FROM folders parent
            JOIN path child ON child.parent_id = parent.id
            WHERE parent.owner_id = ? AND parent.deleted_at IS NULL
        )
        SELECT id, owner_id, parent_id, name, name_normalized, deleted_at, version, created_at, updated_at
        FROM path
        ORDER BY depth DESC`, folderID, ownerID, ownerID).Scan(&folders).Error
	if err != nil || len(folders) == 0 {
		return nil, ErrNotFound
	}
	return folders, nil
}

func (s *Service) Create(ownerID, parentID uuid.UUID, name string) (Folder, error) {
	return s.CreateWithContext(context.Background(), ownerID, parentID, name)
}

func (s *Service) CreateWithContext(ctx context.Context, ownerID, parentID uuid.UUID, name string) (Folder, error) {
	name, normalized, err := normalizeName(name)
	if err != nil {
		return Folder{}, err
	}
	folder := Folder{
		ID:             uuid.Must(uuid.NewV7()),
		OwnerID:        ownerID,
		ParentID:       &parentID,
		Name:           name,
		NameNormalized: normalized,
		Version:        1,
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var epoch ownerepoch.Epoch
		var parent Folder
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where("id = ? AND owner_id = ? AND deleted_at IS NULL", parentID, ownerID).First(&parent).Error; err != nil {
			return ErrNotFound
		}
		if err := tx.Create(&folder).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return ErrConflict
			}
			return err
		}
		var epochErr error
		epoch, epochErr = ownerepoch.BumpContent(tx, ownerID)
		if epochErr != nil {
			return epochErr
		}
		return outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
			EventType: eventcontract.EventFolderCreatedV1, AggregateID: folder.ID, AggregateVersion: folder.Version,
			Producer: "core-api", Payload: eventcontract.FolderCreatedV1{OwnerID: ownerID, FolderID: folder.ID, ParentID: folder.ParentID, ContentEpoch: epoch.ContentEpoch},
		})
	})
	if err != nil {
		return Folder{}, err
	}
	s.clearFolder(ownerID, parentID)
	return folder, nil
}

func (s *Service) Rename(ownerID, folderID uuid.UUID, version int64, name string) (Folder, error) {
	return s.RenameWithContext(context.Background(), ownerID, folderID, version, name)
}

func (s *Service) RenameWithContext(ctx context.Context, ownerID, folderID uuid.UUID, version int64, name string) (Folder, error) {
	name, normalized, err := normalizeName(name)
	if err != nil {
		return Folder{}, err
	}
	var updated Folder
	var epoch ownerepoch.Epoch
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var folder Folder
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND owner_id = ? AND parent_id IS NOT NULL AND deleted_at IS NULL", folderID, ownerID).First(&folder).Error; err != nil {
			return ErrNotFound
		}
		if folder.Version != version {
			return ErrConflict
		}
		oldName := folder.Name
		now := time.Now().UTC()
		if err := tx.Model(&folder).Updates(map[string]any{"name": name, "name_normalized": normalized, "version": gorm.Expr("version + 1"), "updated_at": now}).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return ErrConflict
			}
			return err
		}
		var epochErr error
		epoch, epochErr = ownerepoch.BumpContent(tx, ownerID)
		if epochErr != nil {
			return epochErr
		}
		updated = folder
		updated.Name, updated.NameNormalized, updated.Version, updated.UpdatedAt = name, normalized, version+1, now
		return outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
			EventType: eventcontract.EventFolderRenamedV1, AggregateID: folder.ID, AggregateVersion: version + 1,
			Producer: "core-api", Payload: eventcontract.FolderRenamedV1{OwnerID: ownerID, FolderID: folder.ID, OldName: oldName, NewName: name, ContentEpoch: epoch.ContentEpoch},
		})
	})
	if err == nil && updated.ParentID != nil {
		s.clearFolder(ownerID, *updated.ParentID)
	}
	return updated, err
}

func (s *Service) Move(ownerID, folderID, targetID uuid.UUID, version int64) (Folder, error) {
	return s.MoveWithContext(context.Background(), ownerID, folderID, targetID, version)
}

func (s *Service) MoveWithContext(ctx context.Context, ownerID, folderID, targetID uuid.UUID, version int64) (Folder, error) {
	if folderID == targetID {
		return Folder{}, ErrInvalidInput
	}
	var updated Folder
	var epoch ownerepoch.Epoch
	var oldParentID uuid.UUID
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := dblock.Transaction(tx, "drive:tree:"+ownerID.String()); err != nil {
			return err
		}
		var folder Folder
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND owner_id = ? AND deleted_at IS NULL", folderID, ownerID).First(&folder).Error; err != nil || folder.ParentID == nil {
			return ErrNotFound
		}
		if folder.Version != version {
			return ErrConflict
		}
		var target Folder
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where("id = ? AND owner_id = ? AND deleted_at IS NULL", targetID, ownerID).First(&target).Error; err != nil {
			return ErrNotFound
		}
		var descendant bool
		err := tx.Raw(`
        WITH RECURSIVE descendants AS (
            SELECT id FROM folders WHERE id = ? AND owner_id = ? AND deleted_at IS NULL
            UNION ALL
            SELECT f.id FROM folders f JOIN descendants d ON f.parent_id = d.id
            WHERE f.owner_id = ? AND f.deleted_at IS NULL
        )
        SELECT EXISTS(SELECT 1 FROM descendants WHERE id = ?)`, folderID, ownerID, ownerID, targetID).Scan(&descendant).Error
		if err != nil {
			return err
		}
		if descendant {
			return ErrInvalidInput
		}
		oldParentID = *folder.ParentID
		now := time.Now().UTC()
		if err := tx.Model(&folder).Updates(map[string]any{"parent_id": targetID, "version": gorm.Expr("version + 1"), "updated_at": now}).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return ErrConflict
			}
			return err
		}
		var epochErr error
		epoch, epochErr = ownerepoch.BumpContent(tx, ownerID)
		if epochErr != nil {
			return epochErr
		}
		updated = folder
		updated.ParentID, updated.Version, updated.UpdatedAt = &targetID, version+1, now
		return outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
			EventType: eventcontract.EventFolderMovedV1, AggregateID: folder.ID, AggregateVersion: version + 1,
			Producer: "core-api", Payload: eventcontract.FolderMovedV1{OwnerID: ownerID, FolderID: folder.ID, OldParentID: &oldParentID, NewParentID: &targetID, ContentEpoch: epoch.ContentEpoch},
		})
	})
	if err == nil {
		if oldParentID != uuid.Nil {
			s.clearFolder(ownerID, oldParentID)
		}
		s.clearFolder(ownerID, targetID)
	}
	return updated, err
}

func (s *Service) Delete(ownerID, folderID uuid.UUID, version int64) error {
	return s.DeleteWithContext(context.Background(), ownerID, folderID, version)
}

func (s *Service) DeleteWithContext(ctx context.Context, ownerID, folderID uuid.UUID, version int64) error {
	var epoch ownerepoch.Epoch
	var parentID uuid.UUID
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var folder Folder
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND owner_id = ? AND deleted_at IS NULL", folderID, ownerID).First(&folder).Error; err != nil || folder.ParentID == nil {
			return ErrNotFound
		}
		if folder.Version != version {
			return ErrConflict
		}
		parentID = *folder.ParentID
		var childCount int64
		if err := tx.Model(&Folder{}).Where("parent_id = ? AND deleted_at IS NULL", folderID).Count(&childCount).Error; err != nil {
			return err
		}
		if childCount > 0 {
			return ErrNotEmpty
		}
		if err := tx.Model(&FileEntry{}).Where("folder_id = ? AND deleted_at IS NULL", folderID).Count(&childCount).Error; err != nil {
			return err
		}
		if childCount > 0 {
			return ErrNotEmpty
		}
		now := time.Now().UTC()
		if err := tx.Model(&folder).Updates(map[string]any{"deleted_at": now, "version": gorm.Expr("version + 1"), "updated_at": now}).Error; err != nil {
			return err
		}
		var epochErr error
		epoch, epochErr = ownerepoch.BumpContent(tx, ownerID)
		if epochErr != nil {
			return epochErr
		}
		return outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
			EventType: eventcontract.EventFolderDeletedV1, AggregateID: folder.ID, AggregateVersion: version + 1,
			Producer: "core-api", Payload: eventcontract.FolderDeletedV1{OwnerID: ownerID, FolderID: folder.ID, TombstoneAt: now, ContentEpoch: epoch.ContentEpoch},
		})
	})
	if err == nil {
		s.clearFolder(ownerID, parentID)
	}
	return err
}

func (s *Service) CreateFile(tx *gorm.DB, ownerID, folderID, objectID uuid.UUID, name string) (FileEntry, error) {
	name, normalized, err := normalizeName(name)
	if err != nil {
		return FileEntry{}, err
	}
	var parent Folder
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where("id = ? AND owner_id = ? AND deleted_at IS NULL", folderID, ownerID).First(&parent).Error; err != nil {
		return FileEntry{}, ErrNotFound
	}
	var existing int64
	if err := tx.Model(&FileEntry{}).
		Where("owner_id = ? AND object_id = ? AND deleted_at IS NULL", ownerID, objectID).
		Count(&existing).Error; err != nil {
		return FileEntry{}, err
	}
	if existing > 0 {
		return FileEntry{}, ErrConflict
	}
	entry := FileEntry{
		ID:             uuid.Must(uuid.NewV7()),
		OwnerID:        ownerID,
		FolderID:       folderID,
		ObjectID:       objectID,
		Name:           name,
		NameNormalized: normalized,
		Version:        1,
	}
	if err := tx.Create(&entry).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return FileEntry{}, ErrConflict
		}
		return FileEntry{}, err
	}
	if _, err := ownerepoch.BumpContent(tx, ownerID); err != nil {
		return FileEntry{}, err
	}
	return entry, nil
}

func (s *Service) File(ownerID, fileID uuid.UUID) (FileView, error) {
	if s.cache != nil {
		// Cached metadata cannot authorize access to a deleted or non-ready file.
		var visible int64
		if err := s.db.Table("file_entries e").Joins("JOIN file_objects o ON o.id = e.object_id AND o.status = 'ready'").
			Where("e.id = ? AND e.owner_id = ? AND e.deleted_at IS NULL", fileID, ownerID).Count(&visible).Error; err != nil {
			return FileView{}, err
		}
		if visible == 0 {
			return FileView{}, ErrNotFound
		}
		key := s.readCacheKey(ownerID, fileID, "file")
		var cached FileView
		err := s.cache.Load(context.Background(), key, &cached, func() (any, error) {
			return s.loadFile(ownerID, fileID)
		})
		if err == nil {
			if cached.ID == uuid.Nil {
				return FileView{}, ErrNotFound
			}
			return cached, nil
		}
	}
	return s.loadFile(ownerID, fileID)
}

func (s *Service) loadFile(ownerID, fileID uuid.UUID) (FileView, error) {
	var file FileView
	err := s.db.Raw(`
	        SELECT e.*, o.size_bytes, o.mime_type, o.sha256, d.status AS ai_status
	        FROM file_entries e
	        JOIN file_objects o ON o.id = e.object_id AND o.status = 'ready'
	        LEFT JOIN ai_documents d ON d.object_id = e.object_id
	        WHERE e.id = ? AND e.owner_id = ? AND e.deleted_at IS NULL`, fileID, ownerID).Scan(&file).Error
	if err != nil || file.ID == uuid.Nil {
		return FileView{}, ErrNotFound
	}
	return file, nil
}

func (s *Service) RenameFile(ownerID, fileID uuid.UUID, version int64, name string) (FileView, error) {
	return s.RenameFileWithContext(context.Background(), ownerID, fileID, version, name)
}

func (s *Service) RenameFileWithContext(ctx context.Context, ownerID, fileID uuid.UUID, version int64, name string) (FileView, error) {
	name, normalized, err := normalizeName(name)
	if err != nil {
		return FileView{}, err
	}
	var folderID uuid.UUID
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var epoch ownerepoch.Epoch
		var entry FileEntry
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND owner_id = ? AND deleted_at IS NULL", fileID, ownerID).First(&entry).Error; err != nil {
			return ErrNotFound
		}
		if entry.Version != version {
			return ErrConflict
		}
		folderID = entry.FolderID
		oldName := entry.Name
		now := time.Now().UTC()
		if err := tx.Model(&entry).Updates(map[string]any{"name": name, "name_normalized": normalized, "version": gorm.Expr("version + 1"), "updated_at": now}).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return ErrConflict
			}
			return err
		}
		var epochErr error
		epoch, epochErr = ownerepoch.BumpContent(tx, ownerID)
		if epochErr != nil {
			return epochErr
		}
		return outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
			EventType: eventcontract.EventFileUpdatedV1, AggregateID: entry.ID, AggregateVersion: version + 1,
			Producer: "core-api", Payload: eventcontract.FileUpdatedV1{OwnerID: ownerID, FileEntryID: entry.ID, ObjectID: entry.ObjectID, OldName: oldName, NewName: name, ContentEpoch: epoch.ContentEpoch},
		})
	})
	if err != nil {
		return FileView{}, err
	}
	s.clearFile(ownerID, fileID)
	s.clearFolder(ownerID, folderID)
	return s.File(ownerID, fileID)
}

func (s *Service) MoveFile(ownerID, fileID, targetFolderID uuid.UUID, version int64) (FileView, error) {
	return s.MoveFileWithContext(context.Background(), ownerID, fileID, targetFolderID, version)
}

func (s *Service) MoveFileWithContext(ctx context.Context, ownerID, fileID, targetFolderID uuid.UUID, version int64) (FileView, error) {
	var oldFolderID uuid.UUID
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var epoch ownerepoch.Epoch
		var target Folder
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where("id = ? AND owner_id = ? AND deleted_at IS NULL", targetFolderID, ownerID).First(&target).Error; err != nil {
			return ErrNotFound
		}
		var entry FileEntry
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND owner_id = ? AND deleted_at IS NULL", fileID, ownerID).First(&entry).Error; err != nil {
			return ErrNotFound
		}
		if entry.Version != version {
			return ErrConflict
		}
		oldFolderID = entry.FolderID
		now := time.Now().UTC()
		if err := tx.Model(&entry).Updates(map[string]any{"folder_id": targetFolderID, "version": gorm.Expr("version + 1"), "updated_at": now}).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return ErrConflict
			}
			return err
		}
		var epochErr error
		epoch, epochErr = ownerepoch.BumpContent(tx, ownerID)
		if epochErr != nil {
			return epochErr
		}
		return outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
			EventType: eventcontract.EventFileUpdatedV1, AggregateID: entry.ID, AggregateVersion: version + 1,
			Producer: "core-api", Payload: eventcontract.FileUpdatedV1{OwnerID: ownerID, FileEntryID: entry.ID, ObjectID: entry.ObjectID, OldFolderID: &oldFolderID, NewFolderID: &targetFolderID, ContentEpoch: epoch.ContentEpoch},
		})
	})
	if err != nil {
		return FileView{}, err
	}
	s.clearFile(ownerID, fileID)
	s.clearFolder(ownerID, oldFolderID)
	s.clearFolder(ownerID, targetFolderID)
	return s.File(ownerID, fileID)
}

func (s *Service) folder(ownerID, folderID uuid.UUID) (Folder, error) {
	var folder Folder
	if err := s.db.Where("id = ? AND owner_id = ? AND deleted_at IS NULL", folderID, ownerID).First(&folder).Error; err != nil {
		return Folder{}, ErrNotFound
	}
	return folder, nil
}

func normalizeName(value string) (string, string, error) {
	name := strings.TrimSpace(value)
	if name == "" || len(name) > 255 || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return "", "", ErrInvalidInput
	}
	return name, strings.ToLower(name), nil
}
