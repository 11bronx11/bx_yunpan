package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type generationIndex struct {
	calls      map[string]int
	failTarget string
	failAt     int
}

func (g *generationIndex) write(target string) error {
	g.calls[target]++
	if target == g.failTarget && g.calls[target] == g.failAt {
		return ErrUnavailable
	}
	return nil
}
func (g *generationIndex) UpsertFile(_ context.Context, value FileProjection) error {
	return g.write(value.TargetVersion)
}
func (g *generationIndex) UpsertContent(_ context.Context, value ContentProjection) error {
	return g.write(value.TargetVersion)
}
func (g *generationIndex) TombstoneFile(_ context.Context, target string, _ uuid.UUID, _ int64) error {
	return g.write(target)
}
func (g *generationIndex) TombstoneObjectContent(_ context.Context, target string, _ uuid.UUID, _ int64) error {
	return g.write(target)
}
func (*generationIndex) Health(context.Context) error { return nil }

func projectionTransaction(t *testing.T, check func(*gorm.DB)) {
	t.Helper()
	db, err := gorm.Open(postgres.Open(testPostgresDSN(t)), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	check(tx)
}

func deleteTask() *taskqueue.Task {
	raw, _ := json.Marshal(map[string]any{"event_id": uuid.New(), "event_type": eventcontract.EventFileDeletedV1, "file_entry_id": uuid.New(), "aggregate_version": 2})
	return taskqueue.NewTask("search.project.v1", raw)
}

func TestLiveGenerationsHaveIndependentRetryReceipts(t *testing.T) {
	projectionTransaction(t, func(db *gorm.DB) {
		ctx := context.Background()
		oldTarget, nextTarget := "old-"+uuid.NewString(), "next-"+uuid.NewString()
		p := NewProjectorWithEpoch(db, oldTarget, 1)
		for i, target := range []string{oldTarget, nextTarget} {
			if _, err := p.index.StartVersion(ctx, BackendPostgres, ProjectionContent, target, int64(i+1)); err != nil {
				t.Fatal(err)
			}
		}
		fake := &generationIndex{calls: map[string]int{}, failTarget: nextTarget, failAt: 1}
		p.WithExternalIndex(fake)
		task := deleteTask()
		if err := p.Handler(ctx, task); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("first delivery = %v", err)
		}
		if err := p.Handler(ctx, task); err != nil {
			t.Fatal(err)
		}
		if fake.calls[oldTarget] != 1 || fake.calls[nextTarget] != 2 {
			t.Fatalf("retry lost generation isolation: %v", fake.calls)
		}
		if err := p.Handler(ctx, deleteTask()); err != nil {
			t.Fatal(err)
		}
		if fake.calls[oldTarget] != 2 || fake.calls[nextTarget] != 3 {
			t.Fatalf("live update missed a building generation: %v", fake.calls)
		}
	})
}

func TestLocalReceiptDoesNotSuppressAddedExternalWriter(t *testing.T) {
	projectionTransaction(t, func(db *gorm.DB) {
		p := NewProjector(db, "receipt-"+uuid.NewString())
		task := deleteTask()
		if err := p.Handler(context.Background(), task); err != nil {
			t.Fatal(err)
		}
		fake := &generationIndex{calls: map[string]int{}}
		p.WithExternalIndex(fake)
		if err := p.Handler(context.Background(), task); err != nil {
			t.Fatal(err)
		}
		if fake.calls[p.target] != 1 {
			t.Fatalf("external replay suppressed: %v", fake.calls)
		}
	})
}

func TestBackfillResumesAfterExternalFailureAndRejectsEarlyActivation(t *testing.T) {
	projectionTransaction(t, func(db *gorm.DB) {
		ctx := context.Background()
		owner, folder, object := uuid.New(), uuid.New(), uuid.New()
		for _, statement := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO users(id, username, username_normalized, password_hash) VALUES (?, ?, ?, 'test')`, []any{owner, owner.String(), owner.String()}},
			{`INSERT INTO folders(id, owner_id, name, name_normalized) VALUES (?, ?, '/', '/')`, []any{folder, owner}},
			{`INSERT INTO file_objects(id, sha256, size_bytes, mime_type, bucket, object_key, status) VALUES (?, ?, 10, 'text/plain', 'test', ?, 'ready')`, []any{object, uuid.NewString(), object.String()}},
		} {
			if err := db.Exec(statement.sql, statement.args...).Error; err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 3; i++ {
			id := uuid.New()
			if i > 0 {
				object = uuid.New()
				if err := db.Exec(`INSERT INTO file_objects(id, sha256, size_bytes, mime_type, bucket, object_key, status) VALUES (?, ?, 10, 'text/plain', 'test', ?, 'ready')`, object, uuid.NewString(), object.String()).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Exec(`INSERT INTO file_entries(id, owner_id, folder_id, object_id, name, name_normalized) VALUES (?, ?, ?, ?, ?, ?)`, id, owner, folder, object, id.String(), id.String()).Error; err != nil {
				t.Fatal(err)
			}
		}
		target := "resume-" + uuid.NewString()
		fake := &generationIndex{calls: map[string]int{}, failTarget: target, failAt: 2}
		p := NewProjectorWithEpoch(db, target, 2).WithExternalIndex(fake)
		options := BackfillOptions{BatchSize: 1}
		if _, err := p.Backfill(ctx, options); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("backfill failure=%v", err)
		}
		var version IndexVersion
		if err := db.Where("backend = ? AND projection_type = ? AND target_version = ?", BackendPostgres, ProjectionFile, target).Take(&version).Error; err != nil {
			t.Fatal(err)
		}
		if version.State != "building" || version.SnapshotCursor == nil || version.SnapshotWatermark == nil || *version.SnapshotWatermark != 1 {
			t.Fatalf("lost resume cursor: %+v", version)
		}
		if err := p.ActivateTarget(ctx, target); !errors.Is(err, ErrIndexVersionConflict) {
			t.Fatalf("incomplete target activated: %v", err)
		}
		if _, err := p.index.StartVersion(ctx, BackendPostgres, ProjectionFile, target, 3); !errors.Is(err, ErrIndexVersionConflict) {
			t.Fatalf("epoch changed under same target: %v", err)
		}
		report, err := p.Backfill(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		if report.FileCount != 3 || fake.calls[target] != 4 {
			t.Fatalf("did not resume from committed page: %+v calls=%v", report, fake.calls)
		}
		if _, err := NewProjectorWithEpoch(db, target, 2).Backfill(ctx, options); !errors.Is(err, ErrIndexVersionConflict) {
			t.Fatalf("changed writer set reused an existing cursor: %v", err)
		}
		checkpoint := ProjectionCheckpoint{EventID: uuid.New(), ConsumerName: p.consumerName(), State: "failed", Attempt: 1}
		if err := db.Create(&checkpoint).Error; err != nil {
			t.Fatal(err)
		}
		status, err := p.Status(ctx)
		if err != nil || status.Checkpoints["failed"] != 1 {
			t.Fatalf("publication status=%+v err=%v", status, err)
		}
		if err := p.ActivateTarget(ctx, target); !errors.Is(err, ErrIndexVersionConflict) {
			t.Fatalf("unfinished publication activated: %v", err)
		}
		if err := db.Delete(&checkpoint).Error; err != nil {
			t.Fatal(err)
		}
		if err := p.ActivateTarget(ctx, target); err != nil {
			t.Fatal(err)
		}
		var active int64
		if err := db.Model(&IndexVersion{}).Where("target_version = ? AND state = 'active'", target).Count(&active).Error; err != nil || active != 6 {
			t.Fatalf("incomplete activation: active=%d err=%v", active, err)
		}
	})
}

func TestTargetActivationRollsBackAllBackendsOnIncompleteMember(t *testing.T) {
	projectionTransaction(t, func(db *gorm.DB) {
		ctx := context.Background()
		p := NewProjectorWithEpoch(db, "atomic-"+uuid.NewString(), 3).WithExternalIndex(&generationIndex{})
		versions, err := p.ensureBackfillVersions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range versions.all {
			checksum, count := fmt.Sprintf("sha256:%d", i), int64(0)
			if i != len(versions.all)-1 {
				if err := p.index.UpdateVersionProgress(ctx, v.ID, &count, &checksum); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := p.ActivateTarget(ctx, p.target); !errors.Is(err, ErrIndexVersionConflict) {
			t.Fatalf("activation=%v", err)
		}
		var active int64
		if err := db.Model(&IndexVersion{}).Where("target_version = ? AND state = 'active'", p.target).Count(&active).Error; err != nil || active != 0 {
			t.Fatalf("partial activation: %d %v", active, err)
		}
	})
}

func TestRollbackTargetIsAtomicAcrossBackends(t *testing.T) {
	projectionTransaction(t, func(db *gorm.DB) {
		ctx := context.Background()
		p := NewProjectorWithEpoch(db, "rollback-old-"+uuid.NewString(), 10).WithExternalIndex(&generationIndex{})
		old, err := p.ensureBackfillVersions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		next := *p
		next.target, next.indexEpoch = "rollback-next-"+uuid.NewString(), 11
		newVersions, err := next.ensureBackfillVersions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		checksum, count := "sha256:test", int64(0)
		for _, versions := range []backfillVersions{old, newVersions} {
			for _, version := range versions.all {
				if err := p.index.UpdateVersionProgress(ctx, version.ID, &count, &checksum); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := p.ActivateTarget(ctx, p.target); err != nil {
			t.Fatal(err)
		}
		if err := next.ActivateTarget(ctx, next.target); err != nil {
			t.Fatal(err)
		}
		broken := old.all[len(old.all)-1]
		if err := db.Model(&IndexVersion{}).Where("id = ?", broken.ID).Update("state", "failed").Error; err != nil {
			t.Fatal(err)
		}
		if err := p.RollbackTarget(ctx, p.target); !errors.Is(err, ErrIndexVersionConflict) {
			t.Fatalf("rollback=%v", err)
		}
		var active int64
		if err := db.Model(&IndexVersion{}).Where("target_version = ? AND state = 'active'", next.target).Count(&active).Error; err != nil || active != 6 {
			t.Fatalf("partial rollback: active=%d err=%v", active, err)
		}
		if err := db.Model(&IndexVersion{}).Where("id = ?", broken.ID).Update("state", "retired").Error; err != nil {
			t.Fatal(err)
		}
		if err := p.RollbackTarget(ctx, p.target); err != nil {
			t.Fatal(err)
		}
		if err := db.Model(&IndexVersion{}).Where("target_version = ? AND state = 'active'", p.target).Count(&active).Error; err != nil || active != 6 {
			t.Fatalf("rollback active=%d err=%v", active, err)
		}
	})
}
