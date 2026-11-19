package asynccontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/11bronx11/bx_yunpan/backend/internal/testdb"
)

func testBroker(t *testing.T) *Broker {
	t.Helper()
	url := os.Getenv("TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("TEST_RABBITMQ_URL required")
	}
	b := NewBroker(url, "bx.async.test."+uuid.NewString())
	t.Cleanup(func() {
		b.Close()
		conn, err := amqp.Dial(url)
		if err != nil {
			t.Error("test broker cleanup connection")
			return
		}
		defer conn.Close()
		ch, err := conn.Channel()
		if err != nil {
			t.Error(err)
			return
		}
		defer ch.Close()
		for _, q := range WorkQueues() {
			if _, err := ch.QueueDelete(b.WorkQueue(q), false, false, false); err != nil {
				t.Error(err)
			}
		}
		if _, err := ch.QueueDelete(b.Prefix+".dead", false, false, false); err != nil {
			t.Error(err)
		}
		if err := ch.ExchangeDelete(b.Prefix, false, false); err != nil {
			t.Error(err)
		}
	})
	if err := b.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTwoWorkersDuplicateDeliveryAndReceiptReplay(t *testing.T) {
	s := New(testdb.Control(t))
	b := testBroker(t)
	s.Lease = 3 * time.Second
	s.RetryBase = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task, err := s.Ensure(ctx, spec("worker-duplicate"), "source")
	if err != nil {
		t.Fatal(err)
	}
	var computes, applies atomic.Int32
	compute := func(context.Context, Task) (json.RawMessage, error) {
		computes.Add(1)
		return json.RawMessage(`{"upload_id":"stable"}`), nil
	}
	apply := func(context.Context, Task, Result) (string, *time.Time, error) {
		if applies.Add(1) == 1 {
			return "", nil, fmt.Errorf("report outage after effect")
		}
		return Succeeded, nil, nil
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.RunWorker(ctx, b, "upload", 1, compute, func(error) bool { return false }, log, apply)
		}()
	}
	defer func() { cancel(); wg.Wait() }()
	done := make(chan error, 1)
	go func() { done <- s.RunScheduler(ctx, b, "publisher", log) }()
	defer func() { cancel(); <-done }()
	// A duplicate message can race publisher confirmation or another worker.
	if err := b.Publish(ctx, b.WorkQueue("upload"), task.Notice()); err != nil {
		t.Fatal(err)
	}
	for ctx.Err() == nil {
		var cur Task
		s.DB.First(&cur, "id=?", task.ID)
		if cur.State == Done {
			if cur.Outcome != Succeeded || computes.Load() != 1 || applies.Load() != 2 {
				t.Fatal("checkpoint/commit retry changed identity", cur, computes.Load(), applies.Load())
			}
			return
		}
		Pause(ctx, 10*time.Millisecond)
	}
	t.Fatal("workers did not finish")
}

func TestHealthSeparatesLiveFromDependencyReadiness(t *testing.T) {
	s := New(testdb.Control(t))
	biz := testdb.Business(t)
	b := testBroker(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	stop, err := s.Monitor(context.Background(), addr, "control:health-test", biz, b, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	client := &http.Client{Timeout: 5 * time.Second}
	check := func(path string, code int) string {
		t.Helper()
		r, err := client.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		body, _ := io.ReadAll(r.Body)
		if r.StatusCode != code {
			t.Fatalf("%s: %d", path, r.StatusCode)
		}
		return string(body)
	}
	check("/health/live", 200)
	check("/health/ready", 200)
	if err := s.DB.Exec(`INSERT INTO relay_positions(source) VALUES('business-primary')`).Error; err != nil {
		t.Fatal(err)
	}
	metrics := check("/metrics", 200)
	if strings.Contains(metrics, "xmin=\"") || !strings.Contains(metrics, "bx_async_relay_safe_xmin ") {
		t.Fatal("watermark metric missing or unbounded labels")
	}
	b.Close()
	check("/health/live", 200)
	check("/health/ready", 503)
}
