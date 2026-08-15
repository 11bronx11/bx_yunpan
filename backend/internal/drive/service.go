package drive

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

var (
	ErrNotFound     = errors.New("drive item not found")
	ErrConflict     = errors.New("drive conflict")
	ErrInvalidInput = errors.New("invalid drive input")
	ErrNotEmpty     = errors.New("folder is not empty")
)

type Service struct {
	db *gorm.DB
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

func (s *Service) ProvisionUser(tx *gorm.DB, userID uuid.UUID) error {
	root := Folder{
		ID:             uuid.Must(uuid.NewV7()),
		OwnerID:        userID,
		Name:           "/",
		NameNormalized: "/",
		Version:        1,
	}
	return tx.Create(&root).Error
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
	name, normalized, err := normalizeName(name)
	if err != nil {
		return Folder{}, err
	}
	if _, err := s.folder(ownerID, parentID); err != nil {
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
	if err := s.db.Create(&folder).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return Folder{}, ErrConflict
		}
		return Folder{}, err
	}
	return folder, nil
}

func (s *Service) Rename(ownerID, folderID uuid.UUID, version int64, name string) (Folder, error) {
	name, normalized, err := normalizeName(name)
	if err != nil {
		return Folder{}, err
	}
	result := s.db.Model(&Folder{}).
		Where("id = ? AND owner_id = ? AND parent_id IS NOT NULL AND deleted_at IS NULL AND version = ?", folderID, ownerID, version).
		Updates(map[string]any{"name": name, "name_normalized": normalized, "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()})
	if errors.Is(result.Error, gorm.ErrDuplicatedKey) {
		return Folder{}, ErrConflict
	}
	if result.Error != nil {
		return Folder{}, result.Error
	}
	if result.RowsAffected != 1 {
		return Folder{}, ErrConflict
	}
	return s.folder(ownerID, folderID)
}

func (s *Service) Move(ownerID, folderID, targetID uuid.UUID, version int64) (Folder, error) {
	if folderID == targetID {
		return Folder{}, ErrInvalidInput
	}
	folder, err := s.folder(ownerID, folderID)
	if err != nil || folder.ParentID == nil {
		return Folder{}, ErrNotFound
	}
	if _, err := s.folder(ownerID, targetID); err != nil {
		return Folder{}, err
	}
	var descendant bool
	err = s.db.Raw(`
        WITH RECURSIVE descendants AS (
            SELECT id FROM folders WHERE id = ? AND owner_id = ? AND deleted_at IS NULL
            UNION ALL
            SELECT f.id FROM folders f JOIN descendants d ON f.parent_id = d.id
            WHERE f.owner_id = ? AND f.deleted_at IS NULL
        )
        SELECT EXISTS(SELECT 1 FROM descendants WHERE id = ?)`, folderID, ownerID, ownerID, targetID).Scan(&descendant).Error
	if err != nil {
		return Folder{}, err
	}
	if descendant {
		return Folder{}, ErrInvalidInput
	}
	result := s.db.Model(&Folder{}).
		Where("id = ? AND owner_id = ? AND deleted_at IS NULL AND version = ?", folderID, ownerID, version).
		Updates(map[string]any{"parent_id": targetID, "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()})
	if errors.Is(result.Error, gorm.ErrDuplicatedKey) {
		return Folder{}, ErrConflict
	}
	if result.Error != nil || result.RowsAffected != 1 {
		return Folder{}, ErrConflict
	}
	return s.folder(ownerID, folderID)
}

func (s *Service) Delete(ownerID, folderID uuid.UUID, version int64) error {
	folder, err := s.folder(ownerID, folderID)
	if err != nil || folder.ParentID == nil {
		return ErrNotFound
	}
	var childCount int64
	if err := s.db.Model(&Folder{}).Where("parent_id = ? AND deleted_at IS NULL", folderID).Count(&childCount).Error; err != nil {
		return err
	}
	if childCount > 0 {
		return ErrNotEmpty
	}
	if err := s.db.Model(&FileEntry{}).Where("folder_id = ? AND deleted_at IS NULL", folderID).Count(&childCount).Error; err != nil {
		return err
	}
	if childCount > 0 {
		return ErrNotEmpty
	}
	now := time.Now().UTC()
	result := s.db.Model(&Folder{}).
		Where("id = ? AND owner_id = ? AND deleted_at IS NULL AND version = ?", folderID, ownerID, version).
		Updates(map[string]any{"deleted_at": now, "version": gorm.Expr("version + 1"), "updated_at": now})
	if result.Error != nil || result.RowsAffected != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Service) CreateFile(tx *gorm.DB, ownerID, folderID, objectID uuid.UUID, name string) (FileEntry, error) {
	name, normalized, err := normalizeName(name)
	if err != nil {
		return FileEntry{}, err
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
	return entry, nil
}

func (s *Service) File(ownerID, fileID uuid.UUID) (FileView, error) {
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
	name, normalized, err := normalizeName(name)
	if err != nil {
		return FileView{}, err
	}
	result := s.db.Model(&FileEntry{}).
		Where("id = ? AND owner_id = ? AND deleted_at IS NULL AND version = ?", fileID, ownerID, version).
		Updates(map[string]any{"name": name, "name_normalized": normalized, "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()})
	if result.Error != nil || result.RowsAffected != 1 {
		return FileView{}, ErrConflict
	}
	return s.File(ownerID, fileID)
}

func (s *Service) MoveFile(ownerID, fileID, targetFolderID uuid.UUID, version int64) (FileView, error) {
	if _, err := s.folder(ownerID, targetFolderID); err != nil {
		return FileView{}, err
	}
	result := s.db.Model(&FileEntry{}).
		Where("id = ? AND owner_id = ? AND deleted_at IS NULL AND version = ?", fileID, ownerID, version).
		Updates(map[string]any{"folder_id": targetFolderID, "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()})
	if result.Error != nil || result.RowsAffected != 1 {
		return FileView{}, ErrConflict
	}
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
