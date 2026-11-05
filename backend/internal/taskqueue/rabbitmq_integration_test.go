package taskqueue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
)

type rabbitFixture struct{ url, management, user, password, vhost string }

func (f rabbitFixture) request(t *testing.T, method, path string, body []byte) []byte {
	t.Helper()
	req, err := http.NewRequest(method, f.management+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(f.user, f.password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("management %s %s status=%d", method, path, resp.StatusCode)
	}
	return data
}
func fixture(t *testing.T) rabbitFixture {
	t.Helper()
	raw := os.Getenv("TEST_RABBITMQ_URL")
	management := os.Getenv("TEST_RABBITMQ_MANAGEMENT_URL")
	if raw == "" || management == "" {
		t.Skip("TEST_RABBITMQ_URL and TEST_RABBITMQ_MANAGEMENT_URL required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	f := rabbitFixture{management: management, user: u.User.Username(), password: password, vhost: "test-" + uuid.NewString()}
	f.request(t, "PUT", "/api/vhosts/"+f.vhost, []byte(`{}`))
	f.request(t, "PUT", "/api/permissions/"+f.vhost+"/"+url.PathEscape(f.user), []byte(`{"configure":".*","write":".*","read":".*"}`))
	u.Path = "/" + f.vhost
	f.url = u.String()
	t.Cleanup(func() { f.request(t, "DELETE", "/api/vhosts/"+f.vhost, nil) })
	return f
}
func (f rabbitFixture) broker(t *testing.T, n int) *Broker {
	t.Helper()
	b := New(Config{URL: f.url, Concurrency: n, Prefetch: 16, TaskTimeout: time.Second, PublishTimeout: 5 * time.Second})
	t.Cleanup(func() { _ = b.Close() })
	return b
}
func (f rabbitFixture) channel(t *testing.T) *amqp.Channel {
	t.Helper()
	c, e := amqp.Dial(f.url)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = c.Close() })
	ch, e := c.Channel()
	if e != nil {
		t.Fatal(e)
	}
	return ch
}
func startConsumer(t *testing.T, b *Broker, q map[string]int, h map[string]HandlerFunc) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Consume(ctx, q, h, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case e := <-done:
				if e != nil {
					t.Error(e)
				}
			case <-time.After(10 * time.Second):
				t.Error("consumer did not join")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}
func await(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(15 * time.Second):
		t.Fatal("timed out awaiting task")
		return ""
	}
}
func waitDelivery(t *testing.T, ch *amqp.Channel, q string) amqp.Delivery {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		d, ok, e := ch.Get(q, true)
		if e != nil {
			t.Fatal(e)
		}
		if ok {
			return d
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no delivery in %s", q)
	return amqp.Delivery{}
}
func TestRabbitMQTransport(t *testing.T) {
	f := fixture(t)
	b := f.broker(t, 4)
	task := func(id, q string) Task {
		return Task{ID: id, Kind: "test", Queue: q, Body: []byte(`{"ok":true}`), MaxRetry: 2, Timeout: time.Second}
	}
	t.Run("confirmed_persistent_all_queues", func(t *testing.T) {
		for i := 0; i < 20*len(queueNames); i++ {
			q := queueNames[i%len(queueNames)]
			if e := b.Publish(context.Background(), task(fmt.Sprint(i), q)); e != nil {
				t.Fatal(e)
			}
		}
		ch := f.channel(t)
		for _, q := range queueNames {
			for i := 0; i < 20; i++ {
				d := waitDelivery(t, ch, q)
				if d.DeliveryMode != amqp.Persistent {
					t.Fatal("non-persistent")
				}
			}
		}
	})
	t.Run("mandatory_unroutable", func(t *testing.T) {
		if e := b.publish(context.Background(), "does-not-exist", task("unroutable", "object"), ""); e == nil {
			t.Fatal("returned message accepted")
		}
		if e := b.Publish(context.Background(), task("after-return", "object")); e != nil {
			t.Fatal(e)
		}
		if d := waitDelivery(t, f.channel(t), "object"); d.MessageId != "after-return" {
			t.Fatal(d.MessageId)
		}
	})
	t.Run("retry_metadata_and_fixed_delay", func(t *testing.T) {
		got := make(chan string, 2)
		var calls atomic.Int32
		start := time.Now()
		stop := startConsumer(t, b, map[string]int{"object": 1}, map[string]HandlerFunc{"test": func(ctx context.Context, t *Task) error {
			calls.Add(1)
			n, _ := GetRetryCount(ctx)
			if n == 0 {
				return errors.New("temporary")
			}
			got <- t.ID
			return nil
		}})
		if e := b.Publish(context.Background(), task("retry", "object")); e != nil {
			t.Fatal(e)
		}
		await(t, got)
		stop()
		if calls.Load() != 2 || time.Since(start) < time.Second {
			t.Fatalf("retry calls=%d elapsed=%s", calls.Load(), time.Since(start))
		}
	})
	t.Run("poison_panic_timeout_exhaustion", func(t *testing.T) {
		var recorded atomic.Int32
		b.FailureRecorder = func(context.Context, Task, error) error { recorded.Add(1); return nil }
		stop := startConsumer(t, b, map[string]int{"object": 1}, map[string]HandlerFunc{
			"panic":   func(context.Context, *Task) error { panic("injected") },
			"timeout": func(ctx context.Context, _ *Task) error { <-ctx.Done(); return ctx.Err() },
			"failure": func(context.Context, *Task) error { return errors.New("permanent") },
		})
		for _, kind := range []string{"unknown", "panic", "timeout", "failure"} {
			msg := task(kind, "object")
			msg.Kind = kind
			msg.MaxRetry = 0
			msg.Timeout = 50 * time.Millisecond
			if e := b.Publish(context.Background(), msg); e != nil {
				t.Fatal(e)
			}
		}
		ch := f.channel(t)
		for i := 0; i < 4; i++ {
			d := waitDelivery(t, ch, "object.dlq")
			if d.Headers["x-last-error"] == nil {
				t.Fatal("missing failure reason")
			}
		}
		stop()
		b.FailureRecorder = nil
		if recorded.Load() != 4 {
			t.Fatal(recorded.Load())
		}
	})
	t.Run("bounded_concurrency_and_shutdown_redelivery", func(t *testing.T) {
		entered := make(chan string, 10)
		var active, peak atomic.Int32
		stop := startConsumer(t, b, map[string]int{"object": 4}, map[string]HandlerFunc{"test": func(ctx context.Context, msg *Task) error {
			n := active.Add(1)
			defer active.Add(-1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			entered <- msg.ID
			<-ctx.Done()
			return ctx.Err()
		}})
		for i := 0; i < 8; i++ {
			msg := task(fmt.Sprint("cancel-", i), "object")
			msg.Timeout = time.Minute
			if e := b.Publish(context.Background(), msg); e != nil {
				t.Fatal(e)
			}
		}
		for i := 0; i < 4; i++ {
			await(t, entered)
		}
		stop()
		if peak.Load() != 4 || active.Load() != 0 {
			t.Fatalf("peak=%d active=%d", peak.Load(), active.Load())
		}
		ch := f.channel(t)
		seen := map[string]bool{}
		for i := 0; i < 8; i++ {
			d := waitDelivery(t, ch, "object")
			n, _ := headerInt(d.Headers["x-retry-count"])
			if n != 0 {
				t.Fatal("shutdown spent retry")
			}
			seen[d.MessageId] = true
		}
		if len(seen) != 8 {
			t.Fatal("lost redelivery")
		}
	})
	t.Run("reconnect_after_connection_loss", func(t *testing.T) {
		got := make(chan string, 10)
		stop := startConsumer(t, b, map[string]int{"media": 4}, map[string]HandlerFunc{"test": func(_ context.Context, msg *Task) error { got <- msg.ID; return nil }})
		defer stop()
		if e := b.Publish(context.Background(), task("before", "media")); e != nil {
			t.Fatal(e)
		}
		await(t, got)
		var connections []struct{ Name, Vhost string }
		if e := json.Unmarshal(f.request(t, "GET", "/api/connections", nil), &connections); e != nil {
			t.Fatal(e)
		}
		for _, c := range connections {
			if c.Vhost == f.vhost {
				f.request(t, "DELETE", "/api/connections/"+url.PathEscape(c.Name), nil)
			}
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			e := b.Publish(context.Background(), task("after", "media"))
			if e == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal(e)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if got := await(t, got); got != "after" {
			t.Fatal(got)
		}
	})
	t.Run("broker_restart_preserves_confirmed_tasks", func(t *testing.T) {
		container := os.Getenv("TEST_RABBITMQ_CONTAINER")
		if container == "" {
			t.Skip("explicit isolated TEST_RABBITMQ_CONTAINER required")
		}
		for i := 0; i < 20; i++ {
			if e := b.Publish(context.Background(), task(fmt.Sprint("persist-", i), "search")); e != nil {
				t.Fatal(e)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if out, e := exec.CommandContext(ctx, "docker", "restart", container).CombinedOutput(); e != nil {
			t.Fatalf("restart: %v %s", e, out)
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			if e := b.Health(context.Background()); e == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("broker restart timeout")
			}
			time.Sleep(200 * time.Millisecond)
		}
		seen := map[string]bool{}
		ch := f.channel(t)
		for i := 0; i < 20; i++ {
			seen[waitDelivery(t, ch, "search").MessageId] = true
		}
		if len(seen) != 20 {
			t.Fatal("confirmed task lost across restart")
		}
		// AMQP can accept connections before retry queue leaders and their
		// at-least-once DLX workers have recovered. Verify that path before
		// tearing down the vhost; deleting it during restoration can crash
		// RabbitMQ 4.1's DLX supervisor and poison subsequent isolated tests.
		probe := task("restart-retry-probe", "object")
		for {
			if err := b.publish(context.Background(), retryQueue("object", retryDelays[0]), probe, "restart probe"); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("retry topology did not recover after restart")
			}
			time.Sleep(100 * time.Millisecond)
		}
		if d := waitDelivery(t, ch, "object"); d.MessageId != probe.ID {
			t.Fatalf("retry recovery probe=%s", d.MessageId)
		}
	})
}
func TestRabbitMQStability(t *testing.T) {
	duration := os.Getenv("TEST_STABILITY_DURATION")
	if duration == "" {
		t.Skip("TEST_STABILITY_DURATION required")
	}
	d, e := time.ParseDuration(duration)
	if e != nil {
		t.Fatal(e)
	}
	f := fixture(t)
	b := f.broker(t, 8)
	var mu sync.Mutex
	seen := map[string]int{}
	var count int
	stop := startConsumer(t, b, map[string]int{"object": 2, "media": 4, "maintenance": 1, "upload": 2, "ai": 4, "search": 2}, map[string]HandlerFunc{"soak": func(_ context.Context, t *Task) error { mu.Lock(); seen[t.ID]++; mu.Unlock(); return nil }})
	defer stop()
	start := time.Now()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for time.Since(start) < d {
		<-ticker.C
		msg := Task{ID: fmt.Sprint(count), Kind: "soak", Queue: queueNames[count%len(queueNames)], Body: []byte(`{}`), MaxRetry: 1}
		if e := b.Publish(context.Background(), msg); e != nil {
			t.Fatal(e)
		}
		count++
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n == count {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sent=%d unique_consumed=%d", count, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	duplicates := 0
	for _, n := range seen {
		duplicates += n - 1
	}
	t.Logf("duration=%s confirmed=%d unique_consumed=%d duplicates=%d queues=%d concurrency=8", time.Since(start), count, len(seen), duplicates, len(queueNames))
}

func TestRabbitMQCrashConsumerHelper(t *testing.T) {
	raw := os.Getenv("RABBIT_CRASH_CHILD_URL")
	if raw == "" {
		t.Skip("subprocess helper")
	}
	broker := New(Config{URL: raw, Concurrency: 1, TaskTimeout: time.Minute})
	defer broker.Close()
	err := broker.Consume(context.Background(), map[string]int{"object": 1}, map[string]HandlerFunc{"crash": func(ctx context.Context, _ *Task) error {
		if err := os.WriteFile(os.Getenv("RABBIT_CRASH_READY_FILE"), []byte("processing"), 0600); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
}

func TestRabbitMQKilledConsumerRedelivers(t *testing.T) {
	f := fixture(t)
	broker := f.broker(t, 1)
	task := Task{ID: uuid.NewString(), Kind: "crash", Queue: "object", Body: []byte(`{}`), MaxRetry: 3, Timeout: time.Minute}
	if err := broker.Publish(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	ready := t.TempDir() + "/ready"
	child := exec.Command(os.Args[0], "-test.run=^TestRabbitMQCrashConsumerHelper$")
	child.Env = append(os.Environ(), "RABBIT_CRASH_CHILD_URL="+f.url, "RABBIT_CRASH_READY_FILE="+ready)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not start processing")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("child unexpectedly exited cleanly")
	}
	got := make(chan string, 1)
	stop := startConsumer(t, broker, map[string]int{"object": 1}, map[string]HandlerFunc{"crash": func(ctx context.Context, msg *Task) error {
		retry, ok := GetRetryCount(ctx)
		if !ok || retry != 0 {
			t.Errorf("process crash spent retry: %d %v", retry, ok)
		}
		got <- msg.ID
		return nil
	}})
	if id := await(t, got); id != task.ID {
		t.Fatal("redelivery identity changed")
	}
	stop()
}
