package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	redis "github.com/redis/go-redis/v9"
)

func TestEmbeddingStoreUsesBoundedLocalCache(t *testing.T) {
	store := NewEmbeddingStore(nil, time.Minute, 1)
	if err := store.Set(context.Background(), "first", []float32{1, 2}); err != nil {
		t.Fatal(err)
	}
	value, ok, err := store.Get(context.Background(), "first")
	if err != nil || !ok || len(value) != 2 || value[0] != 1 {
		t.Fatalf("first cache read = %#v, ok=%v, err=%v", value, ok, err)
	}
	if err := store.Set(context.Background(), "second", []float32{3}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get(context.Background(), "first"); err != nil || ok {
		t.Fatalf("bounded cache retained evicted value: ok=%v err=%v", ok, err)
	}
}

func TestEmbeddingStoreExpiresLocalValue(t *testing.T) {
	store := NewEmbeddingStore(nil, time.Millisecond, 2)
	if err := store.Set(context.Background(), "key", []float32{1}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Millisecond)
	if _, ok, err := store.Get(context.Background(), "key"); err != nil || ok {
		t.Fatalf("expired cache read = ok:%v err:%v", ok, err)
	}
}

func TestEmbeddingStorePreservesRedisExpiry(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	writer := NewEmbeddingStore(client, 5*time.Second, 8)
	if err := writer.Set(ctx, "query", []float32{1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	server.FastForward(4950 * time.Millisecond)
	reader := NewEmbeddingStore(client, 5*time.Second, 8)
	value, hit, err := reader.Get(ctx, "query")
	if err != nil || !hit || len(value) != 3 || value[0] != 1 {
		t.Fatalf("Redis hit: value=%v hit=%v err=%v", value, hit, err)
	}
	// Redis had 50ms left at the local fill; advance both clocks past expiry.
	time.Sleep(100 * time.Millisecond)
	server.FastForward(100 * time.Millisecond)
	if server.Exists("query") {
		t.Fatal("Redis key did not expire")
	}
	if _, hit, err := reader.Get(ctx, "query"); err != nil || hit {
		t.Fatalf("local cache outlived Redis: hit=%v err=%v", hit, err)
	}
}
