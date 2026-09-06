// Package workflow contains the durable state machine used by future remote
// service cutovers. It keeps command deduplication and compensation state in
// PostgreSQL so a retry cannot create an implicit cross-service double write.
package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrInvalid    = errors.New("workflow: invalid input")
	ErrConflict   = errors.New("workflow: command conflicts with existing request")
	ErrTransition = errors.New("workflow: invalid state transition")
	ErrNotFound   = errors.New("workflow: record not found")
)

type Instance struct {
	WorkflowID   uuid.UUID `gorm:"type:uuid;primaryKey"`
	WorkflowType string
	BusinessKey  string
	State        string
	Deadline     *time.Time
	Version      int64
	LastError    *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (Instance) TableName() string { return "workflow_instances" }

type Step struct {
	WorkflowID        uuid.UUID `gorm:"type:uuid;primaryKey"`
	StepName          string    `gorm:"primaryKey"`
	CommandID         uuid.UUID `gorm:"type:uuid;unique"`
	State             string
	Attempt           int
	CompensationState string
	LastError         *string
	StartedAt         *time.Time
	CompletedAt       *time.Time
	UpdatedAt         time.Time
}

func (Step) TableName() string { return "workflow_steps" }

type Command struct {
	CommandID    uuid.UUID `gorm:"type:uuid;primaryKey"`
	OwnerService string
	RequestHash  string
	ResultRef    *string
	Status       string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (Command) TableName() string { return "command_deduplications" }

type Service struct{ db *gorm.DB }

func New(db *gorm.DB) *Service { return &Service{db: db} }

func HashRequest(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func (s *Service) Begin(ctx context.Context, workflowType, businessKey string, deadline *time.Time) (Instance, error) {
	if s == nil || s.db == nil || strings.TrimSpace(workflowType) == "" || strings.TrimSpace(businessKey) == "" {
		return Instance{}, ErrInvalid
	}
	instance := Instance{WorkflowID: uuid.Must(uuid.NewV7()), WorkflowType: workflowType, BusinessKey: businessKey, State: "pending", Deadline: deadline, Version: 1}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "business_key"}}, DoNothing: true}).Create(&instance)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			if err := tx.Where("business_key = ?", businessKey).First(&instance).Error; err != nil {
				return err
			}
			if instance.WorkflowType != workflowType {
				return ErrConflict
			}
			return nil
		}
		return nil
	})
	return instance, err
}

func (s *Service) StartStep(ctx context.Context, workflowID uuid.UUID, stepName string, commandID uuid.UUID, ownerService, requestHash string) (Step, error) {
	if s == nil || s.db == nil || workflowID == uuid.Nil || commandID == uuid.Nil || strings.TrimSpace(stepName) == "" || strings.TrimSpace(ownerService) == "" || strings.TrimSpace(requestHash) == "" {
		return Step{}, ErrInvalid
	}
	now := time.Now().UTC()
	step := Step{WorkflowID: workflowID, StepName: stepName, CommandID: commandID, State: "running", Attempt: 1, CompensationState: "not_required", StartedAt: &now, UpdatedAt: now}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var instance Instance
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("workflow_id = ?", workflowID).First(&instance).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if err := transitionInstance(tx, &instance, "running", nil); err != nil {
			return err
		}
		var existing Command
		err := tx.Where("command_id = ?", commandID).First(&existing).Error
		if err == nil {
			if existing.OwnerService != ownerService || existing.RequestHash != requestHash {
				return ErrConflict
			}
			if err := tx.Where("workflow_id = ? AND step_name = ?", workflowID, stepName).First(&step).Error; err != nil {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := tx.Create(&Command{CommandID: commandID, OwnerService: ownerService, RequestHash: requestHash, Status: "pending", CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "workflow_id"}, {Name: "step_name"}}, DoNothing: true}).Create(&step).Error; err != nil {
			return err
		}
		return nil
	})
	return step, err
}

func (s *Service) CompleteCommand(ctx context.Context, commandID uuid.UUID, resultRef string) error {
	if s == nil || s.db == nil || commandID == uuid.Nil {
		return ErrInvalid
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var command Command
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("command_id = ?", commandID).First(&command).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if command.Status == "succeeded" {
			return nil
		}
		if command.Status != "pending" {
			return ErrTransition
		}
		if err := tx.Model(&command).Updates(map[string]any{"status": "succeeded", "result_ref": resultRef, "updated_at": now}).Error; err != nil {
			return err
		}
		var step Step
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("command_id = ?", commandID).First(&step).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if err := transitionStep(tx, &step, "succeeded", nil); err != nil {
			return err
		}
		var remaining int64
		if err := tx.Model(&Step{}).Where("workflow_id = ? AND state NOT IN ?", step.WorkflowID, []string{"succeeded", "compensated"}).Count(&remaining).Error; err != nil {
			return err
		}
		if remaining == 0 {
			var instance Instance
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("workflow_id = ?", step.WorkflowID).First(&instance).Error; err != nil {
				return err
			}
			return transitionInstance(tx, &instance, "succeeded", nil)
		}
		return nil
	})
}

// FailCommand records a retryable command failure and moves its step to
// failed. The workflow remains running until an operator or coordinator asks
// for compensation, which keeps transient remote errors recoverable.
func (s *Service) FailCommand(ctx context.Context, commandID uuid.UUID, message string) error {
	if s == nil || s.db == nil || commandID == uuid.Nil || strings.TrimSpace(message) == "" {
		return ErrInvalid
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var command Command
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("command_id = ?", commandID).First(&command).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if command.Status == "failed" {
			return nil
		}
		if command.Status != "pending" {
			return ErrTransition
		}
		if err := tx.Model(&command).Updates(map[string]any{"status": "failed", "updated_at": now}).Error; err != nil {
			return err
		}
		var step Step
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("command_id = ?", commandID).First(&step).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		return transitionStep(tx, &step, "failed", &message)
	})
}

// BeginCompensation marks a failed workflow and all completed steps that have
// a compensation contract as compensating. The caller performs the actual
// side effects and acknowledges each step with CompleteCompensation.
func (s *Service) BeginCompensation(ctx context.Context, workflowID uuid.UUID, reason string) error {
	if s == nil || s.db == nil || workflowID == uuid.Nil || strings.TrimSpace(reason) == "" {
		return ErrInvalid
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var instance Instance
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("workflow_id = ?", workflowID).First(&instance).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if err := transitionInstance(tx, &instance, "compensating", &reason); err != nil {
			return err
		}
		result := tx.Model(&Step{}).Where("workflow_id = ? AND state = 'succeeded' AND compensation_state <> 'completed'", workflowID).Updates(map[string]any{"state": "compensating", "compensation_state": "pending", "last_error": reason, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return transitionInstance(tx, &instance, "failed", nil)
		}
		return nil
	})
}

func (s *Service) CompleteCompensation(ctx context.Context, workflowID uuid.UUID, stepName string) error {
	if s == nil || s.db == nil || workflowID == uuid.Nil || strings.TrimSpace(stepName) == "" {
		return ErrInvalid
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var step Step
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("workflow_id = ? AND step_name = ?", workflowID, stepName).First(&step).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if err := transitionStep(tx, &step, "compensated", nil); err != nil {
			return err
		}
		var remaining int64
		if err := tx.Model(&Step{}).Where("workflow_id = ? AND state = 'compensating'", workflowID).Count(&remaining).Error; err != nil {
			return err
		}
		if remaining != 0 {
			return nil
		}
		var instance Instance
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("workflow_id = ?", workflowID).First(&instance).Error; err != nil {
			return err
		}
		return transitionInstance(tx, &instance, "failed", nil)
	})
}

func transitionInstance(tx *gorm.DB, instance *Instance, to string, message *string) error {
	if instance == nil {
		return ErrTransition
	}
	if instance.State == to {
		return nil
	}
	if !allowedInstanceTransition(instance.State, to) {
		return ErrTransition
	}
	updates := map[string]any{"state": to, "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()}
	if message != nil {
		updates["last_error"] = *message
	}
	if err := tx.Model(instance).Updates(updates).Error; err != nil {
		return err
	}
	instance.State = to
	instance.Version++
	return nil
}

func transitionStep(tx *gorm.DB, step *Step, to string, message *string) error {
	if step == nil {
		return ErrTransition
	}
	if step.State == to {
		return nil
	}
	if !allowedStepTransition(step.State, to) {
		return ErrTransition
	}
	updates := map[string]any{"state": to, "updated_at": time.Now().UTC()}
	if to == "succeeded" || to == "compensated" {
		updates["completed_at"] = time.Now().UTC()
	}
	if to == "compensated" {
		updates["compensation_state"] = "completed"
	}
	if message != nil {
		updates["last_error"] = *message
	}
	if err := tx.Model(step).Updates(updates).Error; err != nil {
		return err
	}
	step.State = to
	if to == "succeeded" || to == "compensated" {
		now := time.Now().UTC()
		step.CompletedAt = &now
	}
	return nil
}

func allowedInstanceTransition(from, to string) bool {
	switch from {
	case "pending":
		return to == "running" || to == "failed" || to == "manual_intervention"
	case "running":
		return to == "running" || to == "succeeded" || to == "compensating" || to == "failed" || to == "manual_intervention"
	case "compensating":
		return to == "failed" || to == "manual_intervention"
	case "failed":
		return to == "compensating" || to == "manual_intervention"
	default:
		return false
	}
}

func allowedStepTransition(from, to string) bool {
	switch from {
	case "pending", "running":
		return to == "succeeded" || to == "failed" || to == "compensating" || to == "manual_intervention"
	case "compensating":
		return to == "compensated" || to == "failed" || to == "manual_intervention"
	case "failed":
		return to == "running" || to == "compensating" || to == "manual_intervention"
	default:
		return false
	}
}

func ptr[T any](value T) *T { return &value }
