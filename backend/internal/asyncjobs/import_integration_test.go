package asyncjobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/testdb"
)

func TestLegacyImportPreservesFanoutBudgetsAndConclusions(t *testing.T) {
	ctx := context.Background()
	biz := testdb.Business(t)
	s := asynccontrol.New(testdb.Control(t))
	obj := objectstore.Object{ID: uuid.New(), SHA256: fmt.Sprintf("%064d", 1), SizeBytes: 12, MimeType: "image/png", Bucket: "test", ObjectKey: "objects/test", Status: "ready", ReferenceCount: 1, Version: 1}
	if err := biz.Create(&obj).Error; err != nil {
		t.Fatal(err)
	}
	var events []outbox.Event
	// Different retained events for the same content must map to one task.
	for i := 0; i < 2; i++ {
		if err := outbox.Add(biz, "object", obj.ID, outbox.EventObjectReady, map[string]any{"object_id": obj.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if err := biz.Order("seq").Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	var known string
	for i, e := range events {
		for _, dest := range []string{"media.thumbnail.v1@media", "ai.index.v1@ai"} {
			attempts := i + 1
			if dest == "ai.index.v1@ai" {
				attempts = 7
			}
			id := e.ID.String() + ":" + dest
			if dest == "media.thumbnail.v1@media" {
				known = id
			}
			if err := biz.Exec(`INSERT INTO outbox_deliveries(event_id,transport,destination,attempt) VALUES(?,'rabbitmq',?,?)`, e.ID, dest, attempts).Error; err != nil {
				t.Fatal(err)
			}
			if err := biz.Exec(`INSERT INTO task_executions(task_id,payload_sha256,task_type,queue,status,available_at,retry_count,max_retry,timeout_ms) VALUES(?,'fixture','fixture','media','retry',clock_timestamp(),?,9,1000)`, id, attempts-1).Error; err != nil {
				t.Fatal(err)
			}
			if dest == "media.thumbnail.v1@media" && i == 0 {
				if err := biz.Exec(`INSERT INTO task_results VALUES(?,'succeeded',clock_timestamp())`, id).Error; err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	planner := &Planner{Read: biz, AI: true}
	if err := ImportLegacy(ctx, biz, s, planner, "import-test"); err != nil {
		t.Fatal(err)
	}
	var tasks []asynccontrol.Task
	if err := s.DB.Order("kind").Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("fanout count %d", len(tasks))
	}
	for _, task := range tasks {
		switch task.Kind {
		case "thumbnail":
			if task.Attempts != 3 || task.Dispatches != 3 || task.MaxAttempts != 10 || task.State != asynccontrol.Succeeded {
				t.Fatalf("thumbnail: %+v", task)
			}
		case "ai.index":
			if task.Attempts != 14 || task.Dispatches != 14 || task.State != asynccontrol.Ready {
				t.Fatalf("AI budget: %+v", task)
			}
		}
		if task.Deadline.After(events[1].CreatedAt.Add(24*time.Hour + time.Second)) {
			t.Fatal("legacy deadline refreshed")
		}
	}
	if err := s.Recover(ctx, 10); err != nil {
		t.Fatal(err)
	}
	var ai asynccontrol.Task
	if err := s.DB.Where("kind='ai.index'").First(&ai).Error; err != nil {
		t.Fatal(err)
	}
	if ai.State != asynccontrol.NeedsAttention || ai.Attempts != 14 {
		t.Fatal("exhausted work reopened", ai.State, ai.Attempts)
	}
	if err := ImportLegacy(ctx, biz, s, planner, "again"); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := s.DB.Table("tasks").Count(&count).Error; err != nil || count != 2 {
		t.Fatal("import not idempotent", count, err)
	}
	// The cutover consumer is exercised against the dedicated test vhost only.
	url := os.Getenv("TEST_RABBITMQ_URL")
	if url == "" {
		t.Fatal("TEST_RABBITMQ_URL required for legacy cutover")
	}
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatal("test MQ unavailable")
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	// An old queue name is intentional: this fixture runs only in the dedicated vhost.
	prefix := "legacy-test-" + uuid.NewString() + "."
	queue := prefix + "media"
	_, err = ch.QueueDeclare(queue, true, false, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.QueueDelete(queue, false, false, false)
	for _, id := range []string{known, "unknown-" + uuid.NewString()} {
		b, _ := json.Marshal(map[string]string{"fixture": "legacy"})
		if err := ch.PublishWithContext(ctx, "", queue, false, false, amqp.Publishing{MessageId: id, Body: b, Headers: amqp.Table{"x-retry-count": int32(2)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := TransferLegacyMessages(ctx, url, s, prefix); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Table("quarantines").Where("identity LIKE 'legacy-mq:%'").Count(&count).Error; err != nil || count != 2 {
		t.Fatal("missing durable transfers", count, err)
	}
	state, err := ch.QueueInspect(queue)
	if err != nil || state.Messages != 0 {
		t.Fatal("messages not transferred", state, err)
	}
	// Durable records survive reruns; no new work or budget is granted.
	if err := TransferLegacyMessages(ctx, url, s, prefix); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Table("tasks").Count(&count).Error; err != nil || count != 2 {
		t.Fatal(count, err)
	}
}
