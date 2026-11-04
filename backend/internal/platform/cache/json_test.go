package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	redis "github.com/redis/go-redis/v9"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/breaker"
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
	redisTTL := server.TTL("second")
	if redisTTL < 48*time.Second || redisTTL > 72*time.Second {
		t.Fatalf("Redis TTL=%s is outside the 48s..72s jitter range", redisTTL)
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

func TestJSONStoreSharesConcurrentMissAcrossStores(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	stores := []*JSONStore{
		NewJSONStore(client, time.Minute, 8),
		NewJSONStore(client, time.Minute, 8),
	}
	var loads atomic.Int32
	var holdFirst sync.Once
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	load := func() (any, error) {
		loads.Add(1)
		holdFirst.Do(func() {
			close(firstStarted)
			<-releaseFirst
		})
		return map[string]int{"value": 9}, nil
	}
	var wait sync.WaitGroup
	startLoad := func(store *JSONStore) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			var got map[string]int
			if err := store.Load(context.Background(), "same-key", &got, load); err != nil || got["value"] != 9 {
				t.Errorf("value=%v error=%v", got, err)
			}
		}()
	}
	startLoad(stores[0])
	<-firstStarted
	startLoad(stores[1])
	time.Sleep(5 * time.Millisecond)
	close(releaseFirst)
	wait.Wait()
	if got := loads.Load(); got != 1 {
		t.Fatalf("source calls=%d, want one across stores", got)
	}
}

func TestJSONStoreLeaseReleaseIsTokenFenced(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewJSONStore(client, time.Minute, 8)
	key := leaseKey("same-key")
	if err := client.Set(context.Background(), key, "new-owner", cacheLeaseTTL).Err(); err != nil {
		t.Fatal(err)
	}
	store.releaseLease("same-key", "old-owner")
	if got, err := client.Get(context.Background(), key).Result(); err != nil || got != "new-owner" {
		t.Fatalf("stale release changed lease: value=%q error=%v", got, err)
	}
	store.releaseLease("same-key", "new-owner")
	if server.Exists(key) {
		t.Fatal("current lease owner could not release lease")
	}
}

func TestJSONStoreTTLJitterRange(t *testing.T) {
	store := NewJSONStore(nil, time.Minute, 8)
	minTTL, maxTTL := time.Duration(1<<63-1), time.Duration(0)
	for range 32 {
		ttl := store.jitteredTTL()
		if ttl < 48*time.Second || ttl > 72*time.Second {
			t.Fatalf("jittered TTL %s is outside the 48s..72s range", ttl)
		}
		if ttl < minTTL {
			minTTL = ttl
		}
		if ttl > maxTTL {
			maxTTL = ttl
		}
	}
	if maxTTL-minTTL < time.Second {
		t.Fatalf("TTL samples did not spread: min=%s max=%s", minTTL, maxTTL)
	}
}

func TestJSONStoreRedisCircuitFallsBackAndRecovers(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	registry := prometheus.NewRegistry()
	store := NewJSONStore(client, time.Minute, 8, NewMetrics(registry))
	now := time.Now()
	store.circuit = breaker.New(breaker.Config{
		Window: time.Second, Buckets: 2, MinRequests: 2,
		FailureRate: 0.5, OpenTimeout: time.Second, HalfOpenProbes: 1,
		Now: func() time.Time { return now },
	})
	server.SetError("cache unavailable")
	for i := range 2 {
		var got map[string]int
		key := fmt.Sprintf("failed-%d", i)
		if err := store.Load(context.Background(), key, &got, func() (any, error) {
			return map[string]int{"value": i}, nil
		}); err != nil || got["value"] != i {
			t.Fatalf("fallback %d: value=%v error=%v", i, got, err)
		}
	}
	if state := store.circuit.State(); state != breaker.StateOpen {
		t.Fatalf("circuit state=%s, want open", state)
	}
	var openFallback map[string]int
	if err := store.Load(context.Background(), "open-fallback", &openFallback, func() (any, error) {
		return map[string]int{"value": 2}, nil
	}); err != nil || openFallback["value"] != 2 {
		t.Fatalf("open-circuit fallback: value=%v error=%v", openFallback, err)
	}
	if got := metricCounter(t, registry, "yunpan_cache_redis_circuit_rejections_total"); got != 1 {
		t.Fatalf("circuit rejections=%v, want 1", got)
	}
	server.SetError("")
	now = now.Add(2 * time.Second)
	var recovered map[string]int
	if err := store.Load(context.Background(), "recovered", &recovered, func() (any, error) {
		return map[string]int{"value": 3}, nil
	}); err != nil || recovered["value"] != 3 {
		t.Fatalf("half-open fallback: value=%v error=%v", recovered, err)
	}
	if state := store.circuit.State(); state != breaker.StateClosed {
		t.Fatalf("circuit state=%s, want closed", state)
	}
	if got := metricCounter(t, registry, "yunpan_cache_redis_circuit_rejections_total"); got != 1 {
		t.Fatalf("circuit rejections=%v after recovery, want 1", got)
	}
}

func TestJSONStoreMarksSourceErrors(t *testing.T) {
	store := NewJSONStore(nil, time.Minute, 8)
	loadErr := errors.New("postgres unavailable")
	loads := 0
	var target map[string]int
	err := store.Load(context.Background(), "source-error", &target, func() (any, error) {
		loads++
		return nil, loadErr
	})
	cause, ok := SourceLoadCause(err)
	if !ok || cause != loadErr || loads != 1 {
		t.Fatalf("source error=%v cause=%v marked=%v loads=%d", err, cause, ok, loads)
	}
}

func metricCounter(t *testing.T, registry *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == name {
			for _, metric := range family.Metric {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

type targetValue struct{}
