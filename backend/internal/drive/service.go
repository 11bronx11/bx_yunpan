package drive

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/ownerepoch"
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
		if err := tx.Where("id = ? AND owner_id = ? AND deleted_at IS NULL", parentID, ownerID).First(&parent).Error; err != nil {
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
			Producer: "core-api", Payload: eventcontract.FolderRenamedV1{OwnerID: ownerID, FolderID: folder.ID, OldName: folder.Name, NewName: name, ContentEpoch: epoch.ContentEpoch},
		})
	})
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
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var folder Folder
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND owner_id = ? AND deleted_at IS NULL", folderID, ownerID).First(&folder).Error; err != nil || folder.ParentID == nil {
			return ErrNotFound
		}
		if folder.Version != version {
			return ErrConflict
		}
		var target Folder
		if err := tx.Where("id = ? AND owner_id = ? AND deleted_at IS NULL", targetID, ownerID).First(&target).Error; err != nil {
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
		oldParent := folder.ParentID
		updated = folder
		updated.ParentID, updated.Version, updated.UpdatedAt = &targetID, version+1, now
		return outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
			EventType: eventcontract.EventFolderMovedV1, AggregateID: folder.ID, AggregateVersion: version + 1,
			Producer: "core-api", Payload: eventcontract.FolderMovedV1{OwnerID: ownerID, FolderID: folder.ID, OldParentID: oldParent, NewParentID: &targetID, ContentEpoch: epoch.ContentEpoch},
		})
	})
	return updated, err
}

func (s *Service) Delete(ownerID, folderID uuid.UUID, version int64) error {
	return s.DeleteWithContext(context.Background(), ownerID, folderID, version)
}

func (s *Service) DeleteWithContext(ctx context.Context, ownerID, folderID uuid.UUID, version int64) error {
	var epoch ownerepoch.Epoch
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var folder Folder
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND owner_id = ? AND deleted_at IS NULL", folderID, ownerID).First(&folder).Error; err != nil || folder.ParentID == nil {
			return ErrNotFound
		}
		if folder.Version != version {
			return ErrConflict
		}
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
	if _, err := ownerepoch.BumpContent(tx, ownerID); err != nil {
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
	return s.RenameFileWithContext(context.Background(), ownerID, fileID, version, name)
}

func (s *Service) RenameFileWithContext(ctx context.Context, ownerID, fileID uuid.UUID, version int64, name string) (FileView, error) {
	name, normalized, err := normalizeName(name)
	if err != nil {
		return FileView{}, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var epoch ownerepoch.Epoch
		var entry FileEntry
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND owner_id = ? AND deleted_at IS NULL", fileID, ownerID).First(&entry).Error; err != nil {
			return ErrNotFound
		}
		if entry.Version != version {
			return ErrConflict
		}
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
			Producer: "core-api", Payload: eventcontract.FileUpdatedV1{OwnerID: ownerID, FileEntryID: entry.ID, ObjectID: entry.ObjectID, OldName: entry.Name, NewName: name, ContentEpoch: epoch.ContentEpoch},
		})
	})
	if err != nil {
		return FileView{}, err
	}
	return s.File(ownerID, fileID)
}

func (s *Service) MoveFile(ownerID, fileID, targetFolderID uuid.UUID, version int64) (FileView, error) {
	return s.MoveFileWithContext(context.Background(), ownerID, fileID, targetFolderID, version)
}

func (s *Service) MoveFileWithContext(ctx context.Context, ownerID, fileID, targetFolderID uuid.UUID, version int64) (FileView, error) {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var epoch ownerepoch.Epoch
		var target Folder
		if err := tx.Where("id = ? AND owner_id = ? AND deleted_at IS NULL", targetFolderID, ownerID).First(&target).Error; err != nil {
			return ErrNotFound
		}
		var entry FileEntry
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND owner_id = ? AND deleted_at IS NULL", fileID, ownerID).First(&entry).Error; err != nil {
			return ErrNotFound
		}
		if entry.Version != version {
			return ErrConflict
		}
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
		oldFolder := entry.FolderID
		return outbox.AddTyped(ctx, tx, eventcontract.EnvelopeInput{
			EventType: eventcontract.EventFileUpdatedV1, AggregateID: entry.ID, AggregateVersion: version + 1,
			Producer: "core-api", Payload: eventcontract.FileUpdatedV1{OwnerID: ownerID, FileEntryID: entry.ID, ObjectID: entry.ObjectID, OldFolderID: &oldFolder, NewFolderID: &targetFolderID, ContentEpoch: epoch.ContentEpoch},
		})
	})
	if err != nil {
		return FileView{}, err
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
