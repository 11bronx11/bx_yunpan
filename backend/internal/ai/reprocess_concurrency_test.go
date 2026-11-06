package ai

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/drive"
	"github.com/google/uuid"
)

func TestRequestReprocessConcurrentReplayReturnsOneTask(t *testing.T) {
	db := versionDB(t)
	owner, folder, object, file := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id, username, username_normalized, password_hash) VALUES (?, ?, ?, 'test')`, []any{owner, owner.String(), owner.String()}},
		{`INSERT INTO folders(id, owner_id, name, name_normalized) VALUES (?, ?, '/', '/')`, []any{folder, owner}},
		{`INSERT INTO file_objects(id, sha256, size_bytes, mime_type, bucket, object_key, status, reference_count) VALUES (?, ?, 1, 'text/plain', 'test', ?, 'ready', 1)`, []any{object, strings.ReplaceAll(object.String(), "-", "") + strings.ReplaceAll(object.String(), "-", ""), object.String()}},
		{`INSERT INTO file_entries(id, owner_id, folder_id, object_id, name, name_normalized) VALUES (?, ?, ?, ?, 'replay.txt', 'replay.txt')`, []any{file, owner, folder, object}},
	} {
		if err := db.Exec(statement.query, statement.args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM outbox_events WHERE aggregate_id IN (SELECT id FROM async_tasks WHERE owner_id=?)`, owner)
		db.Exec(`DELETE FROM async_tasks WHERE owner_id=?`, owner)
		db.Exec(`DELETE FROM file_entries WHERE id=?`, file)
		db.Exec(`DELETE FROM file_objects WHERE id=?`, object)
		db.Exec(`DELETE FROM folders WHERE id=?`, folder)
		db.Exec(`DELETE FROM users WHERE id=?`, owner)
	})
	service := &Service{db: db, drive: drive.NewService(db)}
	type result struct {
		task Task
		err  error
	}
	const clients = 8
	results := make(chan result, clients)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			task, err := service.RequestReprocess(context.Background(), owner, file, "same-device-request")
			results <- result{task, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var id uuid.UUID
	for got := range results {
		if got.err != nil {
			t.Fatal(got.err)
		}
		if id == uuid.Nil {
			id = got.task.ID
		}
		if got.task.ID != id || got.task.Status != "pending" {
			t.Fatalf("replayed task = %#v, want pending %s", got.task, id)
		}
	}
	var tasks, events int64
	db.Table("async_tasks").Where("owner_id=?", owner).Count(&tasks)
	db.Table("outbox_events").Where("aggregate_id=?", id).Count(&events)
	if tasks != 1 || events != 1 {
		t.Fatalf("tasks=%d events=%d; want one of each", tasks, events)
	}
	if err := db.Model(&Task{}).Where("id=?", id).Update("status", "succeeded").Error; err != nil {
		t.Fatal(err)
	}
	replay, err := service.RequestReprocess(context.Background(), owner, file, "same-device-request")
	if err != nil || replay.ID != id || replay.Status != "succeeded" {
		t.Fatalf("completed replay = %#v, err=%v", replay, err)
	}
}
