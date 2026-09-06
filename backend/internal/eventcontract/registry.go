package eventcontract

import "sort"

const (
	EventFileCreatedV1             = "file.created.v1"
	EventFileUpdatedV1             = "file.updated.v1"
	EventFileDeletedV1             = "file.deleted.v1"
	EventFolderCreatedV1           = "folder.created.v1"
	EventFolderRenamedV1           = "folder.renamed.v1"
	EventFolderMovedV1             = "folder.moved.v1"
	EventFolderDeletedV1           = "folder.deleted.v1"
	EventShareCreatedV1            = "share.created.v1"
	EventShareRevokedV1            = "share.revoked.v1"
	EventShareImportedV1           = "share.imported.v1"
	EventObjectVerifyRequestedV1   = "object.verify_requested.v1"
	EventObjectReadyV1             = "object.ready.v1"
	EventObjectGCRequestedV1       = "object.gc_requested.v1"
	EventAIIndexRequestedV1        = "ai.index_requested.v1"
	EventAIReprocessRequestedV1    = "ai.reprocess_requested.v1"
	EventStorageOperationRecoverV1 = "storage.operation_recover.v1"
	EventMediaThumbnailRequestedV1 = "media.thumbnail_requested.v1"
)

// Definition describes the immutable part of a registered contract.
type Definition struct {
	EventType     string
	MessageKind   MessageKind
	EventVersion  int
	AggregateType string
	NewPayload    func() any
}

var definitions = map[string]Definition{
	EventFileCreatedV1:             {EventFileCreatedV1, MessageKindDomainEvent, 1, "file_entry", func() any { return &FileCreatedV1{} }},
	EventFileUpdatedV1:             {EventFileUpdatedV1, MessageKindDomainEvent, 1, "file_entry", func() any { return &FileUpdatedV1{} }},
	EventFileDeletedV1:             {EventFileDeletedV1, MessageKindDomainEvent, 1, "file_entry", func() any { return &FileDeletedV1{} }},
	EventFolderCreatedV1:           {EventFolderCreatedV1, MessageKindDomainEvent, 1, "folder", func() any { return &FolderCreatedV1{} }},
	EventFolderRenamedV1:           {EventFolderRenamedV1, MessageKindDomainEvent, 1, "folder", func() any { return &FolderRenamedV1{} }},
	EventFolderMovedV1:             {EventFolderMovedV1, MessageKindDomainEvent, 1, "folder", func() any { return &FolderMovedV1{} }},
	EventFolderDeletedV1:           {EventFolderDeletedV1, MessageKindDomainEvent, 1, "folder", func() any { return &FolderDeletedV1{} }},
	EventShareCreatedV1:            {EventShareCreatedV1, MessageKindDomainEvent, 1, "share", func() any { return &ShareCreatedV1{} }},
	EventShareRevokedV1:            {EventShareRevokedV1, MessageKindDomainEvent, 1, "share", func() any { return &ShareRevokedV1{} }},
	EventShareImportedV1:           {EventShareImportedV1, MessageKindDomainEvent, 1, "share", func() any { return &ShareImportedV1{} }},
	EventObjectVerifyRequestedV1:   {EventObjectVerifyRequestedV1, MessageKindCommand, 1, "upload_session", func() any { return &ObjectVerifyRequestedV1{} }},
	EventObjectReadyV1:             {EventObjectReadyV1, MessageKindDomainEvent, 1, "file_object", func() any { return &ObjectReadyV1{} }},
	EventObjectGCRequestedV1:       {EventObjectGCRequestedV1, MessageKindCommand, 1, "file_object", func() any { return &ObjectGCRequestedV1{} }},
	EventAIIndexRequestedV1:        {EventAIIndexRequestedV1, MessageKindCommand, 1, "file_object", func() any { return &AIIndexRequestedV1{} }},
	EventAIReprocessRequestedV1:    {EventAIReprocessRequestedV1, MessageKindCommand, 1, "ai_task", func() any { return &AIReprocessRequestedV1{} }},
	EventStorageOperationRecoverV1: {EventStorageOperationRecoverV1, MessageKindCommand, 1, "storage_operation", func() any { return &StorageOperationRecoverV1{} }},
	EventMediaThumbnailRequestedV1: {EventMediaThumbnailRequestedV1, MessageKindCommand, 1, "file_object", func() any { return &MediaThumbnailRequestedV1{} }},
}

// Lookup returns a copy of the definition for an event type.
func Lookup(eventType string) (Definition, bool) {
	definition, ok := definitions[eventType]
	return definition, ok
}

// Definitions returns a copy suitable for inspection by startup checks.
func Definitions() map[string]Definition {
	result := make(map[string]Definition, len(definitions))
	for eventType, definition := range definitions {
		result[eventType] = definition
	}
	return result
}

// EventTypes returns all registered types in deterministic order.
func EventTypes() []string {
	result := make([]string, 0, len(definitions))
	for eventType := range definitions {
		result = append(result, eventType)
	}
	sort.Strings(result)
	return result
}
