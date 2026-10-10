package cache

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	redis "github.com/redis/go-redis/v9"
)

func TestJSONStoreUsesLRUAndRedis(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	store := NewJSONStore(client, time.Minute, 1)
	var first map[string]string
	if err := store.Load(context.Background(), "first", &first, func() (any, error) { return map[string]string{"v": "1"}, nil }); err != nil {
		t.Fatal(err)
	}
	var second map[string]string
	if err := store.Load(context.Background(), "second", &second, func() (any, error) { return map[string]string{"v": "2"}, nil }); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if hit, err := store.Get(context.Background(), "first", &got); err != nil || !hit || got["v"] != "1" {
		t.Fatalf("redis fallback hit=%v value=%v err=%v", hit, got, err)
	}
	if server.Exists("second") == false {
		t.Fatal("redis value was not written")
	}
	local := NewJSONStore(nil, time.Minute, 1)
	var value map[string]string
	if err := local.Load(context.Background(), "a", &value, func() (any, error) { return map[string]string{"v": "a"}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := local.Load(context.Background(), "b", &value, func() (any, error) { return map[string]string{"v": "b"}, nil }); err != nil {
		t.Fatal(err)
	}
	if hit, _ := local.Get(context.Background(), "a", &value); hit {
		t.Fatal("oldest local value was not evicted")
	}
}

func TestJSONStoreSingleflightLoadsOnce(t *testing.T) {
	store := NewJSONStore(nil, time.Minute, 4)
	var mu sync.Mutex
	loads := 0
	load := func() (any, error) {
		mu.Lock()
		loads++
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		return map[string]int{"ok": 1}, nil
	}
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			var target map[string]int
			if err := store.Load(context.Background(), "same", &target, load); err != nil || target["ok"] != 1 {
				t.Errorf("load target=%v err=%v", target, err)
			}
		}()
	}
	wait.Wait()
	if loads != 1 {
		t.Fatalf("loads=%d, want 1", loads)
	}
	if err := store.Delete(context.Background(), "same"); err != nil {
		t.Fatal(err)
	}
	if err := store.Load(context.Background(), "bad", &targetValue{}, func() (any, error) { return nil, errors.New("source") }); err == nil {
		t.Fatal("source error was swallowed")
	}
}

type targetValue struct{}
