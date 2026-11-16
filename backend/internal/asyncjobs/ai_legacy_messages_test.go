package asyncjobs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/testdb"
	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
)

// G05: actual legacy AI/search messages in a newly owned vhost. A failed
// durable transfer must leave broker messages available; unrelated core work
// must never be drained. This does not claim the post-COMMIT/pre-ACK kill window.
func TestLegacyAIMessagesRequireDurableTransferBeforeACK(t *testing.T) {
	base, management := os.Getenv("TEST_RABBITMQ_URL"), os.Getenv("TEST_RABBITMQ_MANAGEMENT_URL")
	if base == "" || management == "" {
		t.Skip("dedicated test RabbitMQ required")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal("invalid test broker URL")
	}
	password, _ := u.User.Password()
	vhost := "ai-deep-legacy-" + uuid.NewString()
	manage := func(method, path, body string) {
		t.Helper()
		req, _ := http.NewRequest(method, management+path, strings.NewReader(body))
		req.SetBasicAuth(u.User.Username(), password)
		req.Header.Set("Content-Type", "application/json")
		res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal("test vhost unavailable")
		}
		defer res.Body.Close()
		if res.StatusCode >= 300 {
			t.Fatal("test vhost management status", res.StatusCode)
		}
	}
	manage("PUT", "/api/vhosts/"+vhost, `{}`)
	t.Cleanup(func() { manage("DELETE", "/api/vhosts/"+vhost, "") })
	manage("PUT", "/api/permissions/"+vhost+"/"+url.PathEscape(u.User.Username()), `{"configure":".*","write":".*","read":".*"}`)
	u.Path = "/" + vhost
	conn, err := amqp.Dial(u.String())
	if err != nil {
		t.Fatal("test MQ unavailable")
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	prefix := "bx.ai.legacy.fixture"
	queues := []string{"ai.retry.1000", "ai.dlq", "ai", "search", prefix + ".work.ai", prefix + ".work.upload"}
	for _, q := range queues {
		if _, err := ch.QueueDeclare(q, true, false, false, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	for n, q := range queues {
		body, _ := json.Marshal(map[string]any{"event_id": uuid.New(), "object_id": uuid.New(), "task_type": "ai.index.v1", "fixture": n})
		if q == "search" {
			body = []byte("legacy-invalid-json")
		}
		if err := ch.PublishWithContext(context.Background(), "", q, false, false, amqp.Publishing{MessageId: uuid.NewString() + ":ai.index.v1@ai", Type: "ai.index.v1", Body: body, DeliveryMode: amqp.Persistent, Headers: amqp.Table{"x-retry-count": int32(3)}}); err != nil {
			t.Fatal(err)
		}
	}
	store := asynccontrol.New(testdb.Control(t))
	if err := store.DB.Exec(`CREATE FUNCTION reject_transfer() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture transfer failure'; END $$; CREATE TRIGGER reject_transfer BEFORE INSERT ON quarantines FOR EACH ROW EXECUTE FUNCTION reject_transfer()`).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := TransferLegacyAIMessages(ctx, u.String(), store, prefix); err == nil {
		t.Fatal("failed durable transfer acknowledged")
	}
	if err := store.DB.Exec("DROP TRIGGER reject_transfer ON quarantines").Error; err != nil {
		t.Fatal(err)
	}
	if err := TransferLegacyAIMessages(ctx, u.String(), store, prefix); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := store.DB.Table("quarantines").Where("identity LIKE 'legacy-mq:%'").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Fatal("legacy message accounting mismatch", count)
	}
	for _, q := range queues {
		s, err := ch.QueueInspect(q)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if q == prefix+".work.upload" {
			want = 1
		}
		if s.Messages != want {
			t.Fatal("lost or untransferred message", q, s.Messages)
		}
	}
	if err := TransferLegacyAIMessages(ctx, u.String(), store, prefix); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.Table("quarantines").Count(&count).Error; err != nil || count != 5 {
		t.Fatal("duplicate transfer", count, err)
	}
}
