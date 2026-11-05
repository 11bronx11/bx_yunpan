// Package storageops persists external object-storage work independently from
// business transactions. It is intentionally small: callers own the concrete
// S3/MinIO operation while this package owns idempotency, leases, and retry
// state.
package storageops

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	MultipartCreate   = "multipart.create"
	MultipartComplete = "multipart.complete"
	MultipartAbort    = "multipart.abort"
	CanonicalEnsure   = "canonical.ensure"
	StagingDelete     = "staging.delete"
	ObjectDelete      = "object.delete"
	VariantDelete     = "variant.delete"
	StatePending      = "pending"
	StateRunning      = "running"
	StateSucceeded    = "succeeded"
	StateFailed       = "failed"
	StateDead         = "dead"
	// This budget belongs to the durable operation, not a broker message.
	// Rebuilding a lost or exhausted message must never reset it.
	MaxAttempts = 12
)

var ErrInvalidOperation = errors.New("invalid storage operation")
var ErrExhausted = errors.New("storage operation retry budget exhausted")

type Operation struct {
	ID              uuid.UUID `gorm:"type:uuid;primaryKey"`
	OperationType   string
	IdempotencyKey  string
	UploadSessionID *uuid.UUID `gorm:"type:uuid"`
	ObjectID        *uuid.UUID `gorm:"type:uuid"`
	SourceBucket    string
	SourceKey       string
	TargetBucket    string
	TargetKey       string
	StorageUploadID string
	State           string
	Attempt         int
	AvailableAt     time.Time
	LeaseUntil      *time.Time
	LeaseToken      *uuid.UUID `gorm:"type:uuid"`
	LastError       *string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	CompletedAt     *time.Time
}

func (o Operation) LastErrorValue() error {
	if o.State == StateDead {
		return errors.Join(ErrExhausted, errorMessage(o.LastError))
	}
	return errorMessage(o.LastError)
}

func errorMessage(message *string) error {
	if message == nil || strings.TrimSpace(*message) == "" {
		return errors.New("storage operation failed")
	}
	return errors.New(*message)
}

func (Operation) TableName() string { return "storage_operations" }

type Input struct {
	ID              uuid.UUID
	OperationType   string
	IdempotencyKey  string
	UploadSessionID *uuid.UUID
	ObjectID        *uuid.UUID
	SourceBucket    string
	SourceKey       string
	TargetBucket    string
	TargetKey       string
	StorageUploadID string
}

func Enqueue(tx *gorm.DB, input Input) (Operation, error) {
	if tx == nil || strings.TrimSpace(input.OperationType) == "" || strings.TrimSpace(input.IdempotencyKey) == "" {
		return Operation{}, ErrInvalidOperation
	}
	if input.ID == uuid.Nil {
		input.ID = uuid.Must(uuid.NewV7())
	}
	now := time.Now().UTC()
	op := Operation{
		ID: input.ID, OperationType: input.OperationType, IdempotencyKey: input.IdempotencyKey,
		UploadSessionID: input.UploadSessionID, ObjectID: input.ObjectID,
		SourceBucket: input.SourceBucket, SourceKey: input.SourceKey,
		TargetBucket: input.TargetBucket, TargetKey: input.TargetKey, StorageUploadID: input.StorageUploadID,
		State: StatePending, AvailableAt: now, CreatedAt: now, UpdatedAt: now,
	}
	result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "idempotency_key"}}, DoNothing: true}).Create(&op)
	if result.Error != nil {
		return Operation{}, result.Error
	}
	if result.RowsAffected == 0 {
		var existing Operation
		if err := tx.Where("idempotency_key = ?", input.IdempotencyKey).First(&existing).Error; err != nil {
			return Operation{}, err
		}
		op = existing
	}
	return op, nil
}

func MarkSucceeded(db *gorm.DB, id uuid.UUID) error {
	return markSucceeded(db, id, uuid.Nil)
}

// MarkSucceededOwned only permits the current lease owner to complete an
// operation. The token prevents a worker whose lease expired during an
// external call from overwriting the state written by a replacement worker.
func MarkSucceededOwned(db *gorm.DB, id, token uuid.UUID) error {
	if token == uuid.Nil {
		return ErrInvalidOperation
	}
	return markSucceeded(db, id, token)
}

func markSucceeded(db *gorm.DB, id, token uuid.UUID) error {
	if db == nil || id == uuid.Nil {
		return ErrInvalidOperation
	}
	now := time.Now().UTC()
	query := db.Model(&Operation{}).Where("id = ?", id)
	if token != uuid.Nil {
		query = query.Where("state = ? AND lease_token = ?", StateRunning, token)
	}
	result := query.Updates(map[string]any{
		"state": StateSucceeded, "completed_at": now, "updated_at": now, "lease_until": nil, "last_error": nil,
		"lease_token": nil,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 || token == uuid.Nil {
		return nil
	}
	var current Operation
	if err := db.Where("id = ?", id).First(&current).Error; err != nil {
		return err
	}
	if current.State == StateSucceeded {
		return nil
	}
	return errors.New("storage operation lease lost")
}

func MarkFailed(db *gorm.DB, id uuid.UUID, cause error, retryAfter time.Duration) error {
	return markFailed(db, id, uuid.Nil, cause, retryAfter)
}

// MarkFailedOwned is the lease-fenced counterpart of MarkFailed.
func MarkFailedOwned(db *gorm.DB, id, token uuid.UUID, cause error, retryAfter time.Duration) error {
	if token == uuid.Nil {
		return ErrInvalidOperation
	}
	return markFailed(db, id, token, cause, retryAfter)
}

func markFailed(db *gorm.DB, id, token uuid.UUID, cause error, retryAfter time.Duration) error {
	if db == nil || id == uuid.Nil {
		return ErrInvalidOperation
	}
	message := "operation failed"
	if cause != nil {
		message = cause.Error()
	}
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	now := time.Now().UTC()
	query := db.Model(&Operation{}).Where("id = ?", id)
	if token != uuid.Nil {
		query = query.Where("state = ? AND lease_token = ?", StateRunning, token)
	}
	var updated Operation
	result := query.Model(&updated).Clauses(clause.Returning{}).Updates(map[string]any{
		"state":        gorm.Expr("CASE WHEN attempt >= ? THEN ? ELSE ? END", MaxAttempts, StateDead, StateFailed),
		"completed_at": gorm.Expr("CASE WHEN attempt >= ? THEN ?::timestamptz ELSE NULL END", MaxAttempts, now),
		"available_at": gorm.Expr("?::timestamptz + GREATEST(? * interval '1 millisecond', CASE WHEN attempt <= 1 THEN interval '1 second' WHEN attempt = 2 THEN interval '5 seconds' WHEN attempt = 3 THEN interval '30 seconds' WHEN attempt = 4 THEN interval '1 minute' ELSE interval '5 minutes' END)", now, retryAfter.Milliseconds()), "updated_at": now,
		"lease_until": nil, "lease_token": nil, "last_error": message,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 && updated.State == StateDead {
		logStopped(updated)
		return ErrExhausted
	}
	if result.RowsAffected == 1 || token == uuid.Nil {
		return nil
	}
	var current Operation
	if err := db.Where("id = ?", id).First(&current).Error; err != nil {
		return err
	}
	return errors.New("storage operation lease lost")
}

func Get(db *gorm.DB, id uuid.UUID) (Operation, error) {
	var operation Operation
	if db == nil || id == uuid.Nil {
		return operation, ErrInvalidOperation
	}
	err := db.Where("id = ?", id).First(&operation).Error
	return operation, err
}

// TryStart gives one caller ownership of an operation's external side effect.
// Other callers must wait for the owner to publish succeeded/failed, avoiding
// duplicate multipart completion calls after the database transaction ends.
func TryStart(db *gorm.DB, id uuid.UUID, lease time.Duration) (bool, error) {
	started, _, err := TryStartWithToken(db, id, lease)
	return started, err
}

// TryStartWithToken starts an operation and returns the fencing token owned by
// this caller. A false result means another worker currently owns it.
func TryStartWithToken(db *gorm.DB, id uuid.UUID, lease time.Duration) (bool, uuid.UUID, error) {
	if db == nil || id == uuid.Nil {
		return false, uuid.Nil, ErrInvalidOperation
	}
	if lease <= 0 {
		lease = 5 * time.Minute
	}
	if err := stopExhausted(db.Where("id = ?", id), 1); err != nil {
		return false, uuid.Nil, err
	}
	now := time.Now().UTC()
	token := uuid.Must(uuid.NewV7())
	result := db.Model(&Operation{}).Where("attempt < ? AND id = ? AND ((state IN ? AND available_at <= ?) OR (state = ? AND (lease_until IS NULL OR lease_until <= ?)))", MaxAttempts, id, []string{StatePending, StateFailed}, now, StateRunning, now).
		Updates(map[string]any{"state": StateRunning, "lease_until": now.Add(lease), "lease_token": token, "attempt": gorm.Expr("attempt + 1"), "updated_at": now})
	return result.RowsAffected == 1, token, result.Error
}

// Claim atomically leases retryable operations. A lease that expired while a
// process was down is eligible again; no Redis lock is required for safety.
func Claim(ctx context.Context, db *gorm.DB, limit int, lease time.Duration) ([]Operation, error) {
	return ClaimTypes(ctx, db, limit, lease, MultipartCreate, MultipartComplete, MultipartAbort, CanonicalEnsure, StagingDelete, ObjectDelete, VariantDelete)
}

func ClaimTypes(ctx context.Context, db *gorm.DB, limit int, lease time.Duration, operationTypes ...string) ([]Operation, error) {
	if db == nil || limit <= 0 {
		return nil, ErrInvalidOperation
	}
	if len(operationTypes) == 0 {
		return nil, ErrInvalidOperation
	}
	if lease <= 0 {
		lease = time.Minute
	}
	var operations []Operation
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := stopExhausted(tx.Where("operation_type IN ?", operationTypes), limit); err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("attempt < ?", MaxAttempts).
			Where("operation_type IN ? AND state IN ? AND available_at <= ? AND (lease_until IS NULL OR lease_until <= ?)", operationTypes, []string{StatePending, StateFailed, StateRunning}, now, now).
			Order("available_at ASC, created_at ASC").Limit(limit).Find(&operations).Error; err != nil {
			return err
		}
		leaseUntil := now.Add(lease)
		for index := range operations {
			token := uuid.Must(uuid.NewV7())
			if err := tx.Model(&Operation{}).Where("id = ?", operations[index].ID).Updates(map[string]any{
				"state": StateRunning, "lease_until": leaseUntil, "lease_token": token, "attempt": gorm.Expr("attempt + 1"), "updated_at": now,
			}).Error; err != nil {
				return err
			}
			operations[index].State = StateRunning
			operations[index].LeaseUntil = &leaseUntil
			operations[index].LeaseToken = &token
			operations[index].Attempt++
		}
		return nil
	})
	return operations, err
}

// StopExhausted also retires a final attempt whose owner crashed. Live leases
// are never stopped, so a slow owner can still commit its successful result.
func StopExhausted(ctx context.Context, db *gorm.DB, limit int) error {
	return stopExhausted(db.WithContext(ctx), limit)
}

func stopExhausted(db *gorm.DB, limit int) error {
	var rows []Operation
	eligible := db.Session(&gorm.Session{}).Model(&Operation{}).Select("id").
		Where("attempt >= ? AND (state IN ? OR (state = ? AND (lease_until IS NULL OR lease_until <= clock_timestamp())))", MaxAttempts, []string{StatePending, StateFailed}, StateRunning).
		Order("updated_at, id").Limit(limit).Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
	if err := db.Session(&gorm.Session{NewDB: true}).Model(&rows).Where("id IN (?)", eligible).Clauses(clause.Returning{}).Updates(map[string]any{
		"state": StateDead, "lease_until": nil, "lease_token": nil,
		"completed_at": gorm.Expr("clock_timestamp()"), "updated_at": gorm.Expr("clock_timestamp()"),
		"last_error": gorm.Expr("COALESCE(last_error, ?)", ErrExhausted.Error()),
	}).Error; err != nil {
		return err
	}
	for _, row := range rows {
		logStopped(row)
	}
	return nil
}

func logStopped(row Operation) {
	slog.Error("storage operation stopped; manual repair required", "operation_id", row.ID, "operation_type", row.OperationType, "attempt", row.Attempt, "error", row.LastErrorValue())
}
