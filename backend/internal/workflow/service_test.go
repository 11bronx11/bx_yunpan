package workflow

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestHashRequestIsStable(t *testing.T) {
	if HashRequest([]byte("payload")) != HashRequest([]byte("payload")) {
		t.Fatal("request hash is not stable")
	}
	if HashRequest([]byte("payload")) == HashRequest([]byte("other")) {
		t.Fatal("different requests share a hash")
	}
}

func TestInstanceTransitionsRejectTerminalReopen(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		ok       bool
	}{
		{"pending", "running", true},
		{"running", "running", true},
		{"running", "compensating", true},
		{"compensating", "failed", true},
		{"succeeded", "running", false},
		{"manual_intervention", "failed", false},
	} {
		if got := allowedInstanceTransition(tc.from, tc.to); got != tc.ok {
			t.Fatalf("instance transition %s -> %s = %v, want %v", tc.from, tc.to, got, tc.ok)
		}
	}
}

func TestStepTransitionsIncludeCompensation(t *testing.T) {
	if !allowedStepTransition("running", "failed") || !allowedStepTransition("failed", "running") {
		t.Fatal("failed step should be retryable")
	}
	if !allowedStepTransition("compensating", "compensated") {
		t.Fatal("compensating step should be completable")
	}
	if allowedStepTransition("compensated", "running") {
		t.Fatal("compensated step must be terminal")
	}
}

func TestPersistentFailureCompensationConvergesAndCannotReopen(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	service := New(db)
	businessKey := "local-staging-workflow-" + uuid.NewString()
	instance, err := service.Begin(ctx, "local.failure.recovery", businessKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM command_deduplications WHERE command_id IN (SELECT command_id FROM workflow_steps WHERE workflow_id = ?)", instance.WorkflowID).Error
		_ = db.Where("workflow_id = ?", instance.WorkflowID).Delete(&Step{}).Error
		_ = db.Where("workflow_id = ?", instance.WorkflowID).Delete(&Instance{}).Error
	})
	commandID := uuid.New()
	step, err := service.StartStep(ctx, instance.WorkflowID, "reserve", commandID, "local-core", HashRequest([]byte("reserve")))
	if err != nil {
		t.Fatal(err)
	}
	if step.State != "running" {
		t.Fatalf("step state after start = %q", step.State)
	}
	// Replaying the same command is a no-op and returns the existing step.
	replayed, err := service.StartStep(ctx, instance.WorkflowID, "reserve", commandID, "local-core", HashRequest([]byte("reserve")))
	if err != nil || replayed.CommandID != commandID {
		t.Fatalf("replayed command = %#v, err=%v", replayed, err)
	}
	if err := service.FailCommand(ctx, commandID, "simulated remote outage"); err != nil {
		t.Fatal(err)
	}
	// A completed prior step is the compensable side effect when a later
	// command fails.
	completedCommandID := uuid.New()
	if _, err := service.StartStep(ctx, instance.WorkflowID, "prepare", completedCommandID, "local-core", HashRequest([]byte("prepare"))); err != nil {
		t.Fatal(err)
	}
	if err := service.CompleteCommand(ctx, completedCommandID, "reservation-ref"); err != nil {
		t.Fatal(err)
	}
	if err := service.BeginCompensation(ctx, instance.WorkflowID, "rollback local reservation"); err != nil {
		t.Fatal(err)
	}
	if err := service.CompleteCompensation(ctx, instance.WorkflowID, "prepare"); err != nil {
		t.Fatal(err)
	}
	var final Instance
	if err := db.First(&final, "workflow_id = ?", instance.WorkflowID).Error; err != nil {
		t.Fatal(err)
	}
	if final.State != "failed" {
		t.Fatalf("final workflow state = %q", final.State)
	}
	if err := service.CompleteCompensation(ctx, instance.WorkflowID, "prepare"); err != nil {
		t.Fatalf("idempotent terminal compensation result = %v", err)
	}
	if _, err := service.StartStep(ctx, instance.WorkflowID, "reopen", uuid.New(), "local-core", HashRequest([]byte("reopen"))); !errors.Is(err, ErrTransition) {
		t.Fatalf("terminal workflow reopen result = %v", err)
	}
}
