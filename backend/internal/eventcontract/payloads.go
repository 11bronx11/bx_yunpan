package eventcontract

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The payload structs intentionally contain only business data.  Transport
// metadata belongs in Envelope.Trace and must not leak into these types.
type FileCreatedV1 struct {
	OwnerID      uuid.UUID `json:"owner_id"`
	FileEntryID  uuid.UUID `json:"file_entry_id"`
	ObjectID     uuid.UUID `json:"object_id"`
	ContentEpoch int64     `json:"content_epoch,omitempty"`
}

type FileUpdatedV1 struct {
	OwnerID      uuid.UUID  `json:"owner_id"`
	FileEntryID  uuid.UUID  `json:"file_entry_id"`
	ObjectID     uuid.UUID  `json:"object_id"`
	OldName      string     `json:"old_name,omitempty"`
	NewName      string     `json:"new_name,omitempty"`
	OldFolderID  *uuid.UUID `json:"old_folder_id,omitempty"`
	NewFolderID  *uuid.UUID `json:"new_folder_id,omitempty"`
	ContentEpoch int64      `json:"content_epoch,omitempty"`
}

type FileDeletedV1 struct {
	OwnerID      uuid.UUID `json:"owner_id"`
	FileEntryID  uuid.UUID `json:"file_entry_id"`
	ObjectID     uuid.UUID `json:"object_id"`
	TombstoneAt  time.Time `json:"tombstone_at"`
	ContentEpoch int64     `json:"content_epoch,omitempty"`
}

type FolderCreatedV1 struct {
	OwnerID      uuid.UUID  `json:"owner_id"`
	FolderID     uuid.UUID  `json:"folder_id"`
	ParentID     *uuid.UUID `json:"parent_id,omitempty"`
	ContentEpoch int64      `json:"content_epoch,omitempty"`
}

type FolderRenamedV1 struct {
	OwnerID      uuid.UUID `json:"owner_id"`
	FolderID     uuid.UUID `json:"folder_id"`
	OldName      string    `json:"old_name"`
	NewName      string    `json:"new_name"`
	ContentEpoch int64     `json:"content_epoch,omitempty"`
}

type FolderMovedV1 struct {
	OwnerID      uuid.UUID  `json:"owner_id"`
	FolderID     uuid.UUID  `json:"folder_id"`
	OldParentID  *uuid.UUID `json:"old_parent_id,omitempty"`
	NewParentID  *uuid.UUID `json:"new_parent_id,omitempty"`
	ContentEpoch int64      `json:"content_epoch,omitempty"`
}

type FolderDeletedV1 struct {
	OwnerID      uuid.UUID `json:"owner_id"`
	FolderID     uuid.UUID `json:"folder_id"`
	TombstoneAt  time.Time `json:"tombstone_at"`
	ContentEpoch int64     `json:"content_epoch,omitempty"`
}

type ShareCreatedV1 struct {
	OwnerID     uuid.UUID  `json:"owner_id"`
	ShareID     uuid.UUID  `json:"share_id"`
	FileEntryID uuid.UUID  `json:"file_entry_id"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	ACLEpoch    int64      `json:"acl_epoch,omitempty"`
}

type ShareRevokedV1 struct {
	OwnerID   uuid.UUID `json:"owner_id"`
	ShareID   uuid.UUID `json:"share_id"`
	RevokedAt time.Time `json:"revoked_at"`
	ACLEpoch  int64     `json:"acl_epoch,omitempty"`
}

type ShareUpdatedV1 struct {
	OwnerID   uuid.UUID  `json:"owner_id"`
	ShareID   uuid.UUID  `json:"share_id"`
	ExpiresAt *time.Time `json:"expires_at"`
	ACLEpoch  int64      `json:"acl_epoch,omitempty"`
}

type ShareImportedV1 struct {
	OwnerID      uuid.UUID `json:"owner_id"`
	ShareID      uuid.UUID `json:"share_id"`
	FileEntryID  uuid.UUID `json:"file_entry_id"`
	ObjectID     uuid.UUID `json:"object_id"`
	ContentEpoch int64     `json:"content_epoch,omitempty"`
}

type ObjectVerifyRequestedV1 struct {
	UploadID uuid.UUID `json:"upload_id"`
}

type ObjectReadyV1 struct {
	ObjectID uuid.UUID  `json:"object_id"`
	OwnerID  *uuid.UUID `json:"owner_id,omitempty"`
}

type ObjectGCRequestedV1 struct {
	ObjectID uuid.UUID `json:"object_id"`
}

type AIIndexRequestedV1 struct {
	ObjectID             uuid.UUID  `json:"object_id"`
	RunID                *uuid.UUID `json:"run_id,omitempty"`
	ContentHash          string     `json:"content_hash,omitempty"`
	DocumentVersionID    *uuid.UUID `json:"document_version_id,omitempty"`
	PipelineVersion      string     `json:"pipeline_version,omitempty"`
	ModelVersion         string     `json:"model_version,omitempty"`
	ParserProfileVersion string     `json:"parser_profile_version,omitempty"`
	TraceID              string     `json:"trace_id,omitempty"`
}

// AIIndexedV1 is published with the active-version switch, after chunks exist.
// It triggers search projection without requesting another AI indexing run.
type AIIndexedV1 struct {
	ObjectID          uuid.UUID `json:"object_id"`
	DocumentVersionID uuid.UUID `json:"document_version_id"`
}

type AIReprocessRequestedV1 struct {
	TaskID               uuid.UUID  `json:"task_id"`
	ObjectID             uuid.UUID  `json:"object_id"`
	RunID                *uuid.UUID `json:"run_id,omitempty"`
	ContentHash          string     `json:"content_hash,omitempty"`
	DocumentVersionID    *uuid.UUID `json:"document_version_id,omitempty"`
	PipelineVersion      string     `json:"pipeline_version,omitempty"`
	ModelVersion         string     `json:"model_version,omitempty"`
	ParserProfileVersion string     `json:"parser_profile_version,omitempty"`
	TraceID              string     `json:"trace_id,omitempty"`
}

// StorageOperationRecoverV1 is a durable maintenance nudge for an operation
// whose published task may have disappeared with the broker. The operation
// row remains the authority; consumers re-read it and apply lease fencing.
type StorageOperationRecoverV1 struct {
	OperationID   uuid.UUID `json:"operation_id"`
	OperationType string    `json:"operation_type"`
	RecoveryKey   string    `json:"recovery_key"`
}

// UploadOperationRecoverV1 nudges the upload worker to resume a durable
// multipart or upload-owned storage operation.
type UploadOperationRecoverV1 struct {
	OperationID   uuid.UUID `json:"operation_id"`
	OperationType string    `json:"operation_type"`
	RecoveryKey   string    `json:"recovery_key"`
}

type MediaThumbnailRequestedV1 struct {
	ObjectID uuid.UUID `json:"object_id"`
}

func requireUUID(name string, value uuid.UUID) error {
	if value == uuid.Nil {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

func requireTime(name string, value time.Time) error {
	if value.IsZero() {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

func requireNames(oldName, newName string) error {
	if strings.TrimSpace(oldName) == "" || strings.TrimSpace(newName) == "" {
		return errors.New("old_name and new_name are required")
	}
	return nil
}

func (p FileCreatedV1) Validate() error {
	if err := requireUUID("owner_id", p.OwnerID); err != nil {
		return err
	}
	if err := requireUUID("file_entry_id", p.FileEntryID); err != nil {
		return err
	}
	return requireUUID("object_id", p.ObjectID)
}

func (p FileUpdatedV1) Validate() error {
	if err := requireUUID("owner_id", p.OwnerID); err != nil {
		return err
	}
	if err := requireUUID("file_entry_id", p.FileEntryID); err != nil {
		return err
	}
	if err := requireUUID("object_id", p.ObjectID); err != nil {
		return err
	}
	if strings.TrimSpace(p.OldName) == "" && strings.TrimSpace(p.NewName) == "" && p.OldFolderID == nil && p.NewFolderID == nil {
		return errors.New("file update must carry name or folder changes")
	}
	return nil
}

func (p FileDeletedV1) Validate() error {
	if err := requireUUID("owner_id", p.OwnerID); err != nil {
		return err
	}
	if err := requireUUID("file_entry_id", p.FileEntryID); err != nil {
		return err
	}
	if err := requireUUID("object_id", p.ObjectID); err != nil {
		return err
	}
	return requireTime("tombstone_at", p.TombstoneAt)
}

func (p FolderCreatedV1) Validate() error {
	if err := requireUUID("owner_id", p.OwnerID); err != nil {
		return err
	}
	return requireUUID("folder_id", p.FolderID)
}

func (p FolderRenamedV1) Validate() error {
	if err := requireUUID("owner_id", p.OwnerID); err != nil {
		return err
	}
	if err := requireUUID("folder_id", p.FolderID); err != nil {
		return err
	}
	return requireNames(p.OldName, p.NewName)
}

func (p FolderMovedV1) Validate() error {
	if err := requireUUID("owner_id", p.OwnerID); err != nil {
		return err
	}
	if err := requireUUID("folder_id", p.FolderID); err != nil {
		return err
	}
	if p.OldParentID == nil && p.NewParentID == nil {
		return errors.New("folder move must carry parent changes")
	}
	return nil
}

func (p FolderDeletedV1) Validate() error {
	if err := requireUUID("owner_id", p.OwnerID); err != nil {
		return err
	}
	if err := requireUUID("folder_id", p.FolderID); err != nil {
		return err
	}
	return requireTime("tombstone_at", p.TombstoneAt)
}

func (p ShareCreatedV1) Validate() error {
	if err := requireUUID("owner_id", p.OwnerID); err != nil {
		return err
	}
	if err := requireUUID("share_id", p.ShareID); err != nil {
		return err
	}
	return requireUUID("file_entry_id", p.FileEntryID)
}

func (p ShareRevokedV1) Validate() error {
	if err := requireUUID("owner_id", p.OwnerID); err != nil {
		return err
	}
	if err := requireUUID("share_id", p.ShareID); err != nil {
		return err
	}
	return requireTime("revoked_at", p.RevokedAt)
}

func (p ShareImportedV1) Validate() error {
	if err := requireUUID("owner_id", p.OwnerID); err != nil {
		return err
	}
	if err := requireUUID("share_id", p.ShareID); err != nil {
		return err
	}
	if err := requireUUID("file_entry_id", p.FileEntryID); err != nil {
		return err
	}
	return requireUUID("object_id", p.ObjectID)
}

func (p ObjectVerifyRequestedV1) Validate() error { return requireUUID("upload_id", p.UploadID) }

func (p ObjectReadyV1) Validate() error { return requireUUID("object_id", p.ObjectID) }

func (p ObjectGCRequestedV1) Validate() error { return requireUUID("object_id", p.ObjectID) }

func (p AIIndexRequestedV1) Validate() error { return requireUUID("object_id", p.ObjectID) }

func (p AIIndexedV1) Validate() error {
	if err := requireUUID("object_id", p.ObjectID); err != nil {
		return err
	}
	return requireUUID("document_version_id", p.DocumentVersionID)
}

func (p AIReprocessRequestedV1) Validate() error {
	if err := requireUUID("task_id", p.TaskID); err != nil {
		return err
	}
	return requireUUID("object_id", p.ObjectID)
}

func (p StorageOperationRecoverV1) Validate() error {
	if err := requireUUID("operation_id", p.OperationID); err != nil {
		return err
	}
	if strings.TrimSpace(p.OperationType) == "" {
		return errors.New("operation_type is required")
	}
	if strings.TrimSpace(p.RecoveryKey) == "" {
		return errors.New("recovery_key is required")
	}
	return nil
}

func (p UploadOperationRecoverV1) Validate() error {
	return (StorageOperationRecoverV1)(p).Validate()
}

func (p MediaThumbnailRequestedV1) Validate() error { return requireUUID("object_id", p.ObjectID) }
