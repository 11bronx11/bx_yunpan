package taskqueue

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type acknowledger struct{ ack, nack atomic.Int32 }

func (a *acknowledger) Ack(uint64, bool) error        { a.ack.Add(1); return nil }
func (a *acknowledger) Nack(uint64, bool, bool) error { a.nack.Add(1); return nil }
func (a *acknowledger) Reject(uint64, bool) error     { a.nack.Add(1); return nil }
func TestHandlerMetadataAndCancellation(t *testing.T) {
	b := New(Config{})
	defer b.Close()
	a := &acknowledger{}
	d := amqp.Delivery{Acknowledger: a, MessageId: "id", Type: "verify", Headers: amqp.Table{"x-retry-count": int32(2), "x-max-retry": int32(2)}}
	h := map[string]HandlerFunc{"verify": func(ctx context.Context, _ *Task) error {
		n, ok := GetRetryCount(ctx)
		if !ok || n != 2 {
			t.Errorf("retry=%d %v", n, ok)
		}
		n, ok = GetMaxRetry(ctx)
		if !ok || n != 2 {
			t.Errorf("max=%d %v", n, ok)
		}
		id, ok := GetTaskID(ctx)
		if !ok || id != "id" {
			t.Errorf("id=%s %v", id, ok)
		}
		return nil
	}}
	if err := b.handleDelivery(context.Background(), "object", d, h, slog.Default()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.handleDelivery(ctx, "object", d, h, slog.Default()); err != nil {
		t.Fatal(err)
	}
	if a.ack.Load() != 1 || a.nack.Load() != 1 {
		t.Fatalf("ack=%d nack=%d", a.ack.Load(), a.nack.Load())
	}
}
func TestPanicIsRetryableAndUnknownIsTerminal(t *testing.T) {
	if err := invoke(context.Background(), &Task{}, func(context.Context, *Task) error { panic("boom") }); err == nil || errors.Is(err, SkipRetry) {
		t.Fatalf("panic=%v", err)
	}
	if err := invoke(context.Background(), &Task{Kind: "unknown"}, nil); !errors.Is(err, SkipRetry) {
		t.Fatalf("unknown=%v", err)
	}
}
func TestPublishBoundsStalledHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, e := ln.Accept()
		if e == nil {
			defer c.Close()
			_, _ = io.Copy(io.Discard, c)
		}
	}()
	b := New(Config{URL: "amqp://guest:guest@" + ln.Addr().String() + "/", PublishTimeout: 100 * time.Millisecond})
	defer b.Close()
	start := time.Now()
	err = b.Publish(context.Background(), Task{ID: "id", Kind: "kind", Queue: "object"})
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("bounded publish err=%v duration=%s", err, time.Since(start))
	}
	<-done
}
func TestRejectsUnknownQueueBeforeConnecting(t *testing.T) {
	b := New(Config{})
	defer b.Close()
	if err := b.Publish(context.Background(), Task{ID: "id", Kind: "kind", Queue: "typo"}); err == nil {
		t.Fatal("accepted unknown queue")
	}
}

func TestLegacyDecodePreservesRetryAndTimeout(t *testing.T) {
	// protobuf: type="kind", payload="{}", id="id", queue="ai",
	// max_retry=8, retried=5, timeout=30 seconds.
	wire := []byte{10, 4, 'k', 'i', 'n', 'd', 18, 2, '{', '}', 26, 2, 'i', 'd', 34, 2, 'a', 'i', 40, 8, 48, 5, 64, 30}
	task, err := DecodeLegacyTask(wire)
	if err != nil {
		t.Fatal(err)
	}
	if task.Kind != "kind" || task.ID != "id" || task.Queue != "ai" || task.RetryCount != 5 || task.MaxRetry != 8 || task.Timeout != 30*time.Second || string(task.Body) != "{}" {
		t.Fatalf("legacy metadata changed: %+v", task)
	}
	if _, err := DecodeLegacyTask(wire[:len(wire)-1]); err == nil {
		t.Fatal("accepted truncated protobuf")
	}
}

func TestRetryPublishFailureNeverACKsOriginal(t *testing.T) {
	b := New(Config{URL: "amqp://guest:guest@127.0.0.1:1/", PublishTimeout: 100 * time.Millisecond, MaxRetry: 1})
	defer b.Close()
	a := &acknowledger{}
	d := amqp.Delivery{Acknowledger: a, MessageId: "retry-confirm-window", Type: "test", Headers: amqp.Table{"x-max-retry": int32(1)}}
	err := b.handleDelivery(context.Background(), "object", d, map[string]HandlerFunc{"test": func(context.Context, *Task) error { return errors.New("temporary") }}, slog.Default())
	if err == nil || a.ack.Load() != 0 || a.nack.Load() != 1 {
		t.Fatalf("error=%v ack=%d nack=%d", err, a.ack.Load(), a.nack.Load())
	}
}
func TestFailureAuditFailureNeverACKsOriginal(t *testing.T) {
	b := New(Config{})
	defer b.Close()
	b.FailureRecorder = func(context.Context, Task, error) error { return errors.New("database unavailable") }
	a := &acknowledger{}
	d := amqp.Delivery{Acknowledger: a, MessageId: "audit-window", Type: "unknown"}
	err := b.handleDelivery(context.Background(), "object", d, nil, slog.Default())
	if err == nil || a.ack.Load() != 0 || a.nack.Load() != 1 {
		t.Fatalf("error=%v ack=%d nack=%d", err, a.ack.Load(), a.nack.Load())
	}
}
