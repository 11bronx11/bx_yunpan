package asynccontrol

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
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
	b := NewBroker(url, 32, "bx.async.test."+uuid.NewString())
	if err := b.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
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
		for _, q := range []string{"upload", "object", "media", "gc", "ai", "search"} {
			if _, err := ch.QueueDelete(b.WorkQueue(q), false, false, false); err != nil {
				t.Error(err)
			}
		}
		for i := 0; i < 32; i++ {
			if _, err := ch.QueueDelete(b.ResultQueue(i), false, false, false); err != nil {
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
	return b
}

func TestTwoCommittersDistributeAndTakeOverResultShards(t *testing.T) {
	s := New(testdb.Control(t))
	s.Lease = 3 * time.Second
	b := testBroker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	aCtx, stopA := context.WithCancel(ctx)
	defer stopA()
	bCtx, stopB := context.WithCancel(ctx)
	defer stopB()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	doneA, doneB := make(chan error, 1), make(chan error, 1)
	consumed := make(chan int, 2)
	go func() {
		doneA <- s.RunCommitters(aCtx, b, "a", func(_ context.Context, n Notice) error { consumed <- n.Shard; return nil }, logger)
	}()
	go func() {
		doneB <- s.RunCommitters(bCtx, b, "b", func(_ context.Context, n Notice) error { consumed <- n.Shard; return nil }, logger)
	}()
	wait := func(wantA, wantB int) {
		t.Helper()
		for ctx.Err() == nil {
			var a, b int64
			e1 := s.DB.Table("module_leases").Where("name LIKE 'committer:%' AND holder='a' AND lease_until>clock_timestamp()").Count(&a).Error
			e2 := s.DB.Table("module_leases").Where("name LIKE 'committer:%' AND holder='b' AND lease_until>clock_timestamp()").Count(&b).Error
			if e1 != nil || e2 != nil {
				t.Fatal(e1, e2)
			}
			if a == int64(wantA) && b == int64(wantB) {
				return
			}
			Pause(ctx, 50*time.Millisecond)
		}
		t.Fatal("committer shard allocation timed out")
	}
	wait(16, 16)
	stopA()
	<-doneA
	wait(0, 32)
	id := uuid.New()
	if err := b.Publish(ctx, b.ResultQueue(0), Notice{TaskID: uuid.New(), ResultID: &id, Version: 1, Shard: 0, SemanticKey: "takeover-probe", InputDigest: "probe", ShardKey: "probe", LayoutVersion: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case shard := <-consumed:
		if shard != 0 {
			t.Fatal(shard)
		}
	case <-ctx.Done():
		t.Fatal("surviving committer did not consume")
	}
	stopB()
	<-doneB
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
	stop, err := s.Monitor(context.Background(), addr, "health-test", biz, b, slog.Default())
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
