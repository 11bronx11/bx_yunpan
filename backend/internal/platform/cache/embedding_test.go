package cache

import (
	"context"
	"testing"
	"time"
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
