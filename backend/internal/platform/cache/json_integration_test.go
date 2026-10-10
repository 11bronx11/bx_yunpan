package cache

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	redis "github.com/redis/go-redis/v9"
)

func TestJSONStoreRealRedisFallbackBudget(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" || os.Getenv("TEST_REDIS_FAULTS") != "1" {
		t.Skip("requires dedicated TEST_REDIS_ADDR and TEST_REDIS_FAULTS=1")
	}
	ctx := context.Background()
	client := NewRedisClient(addr, "", 0)
	control := redis.NewClient(&redis.Options{Addr: addr, ReadTimeout: 2 * time.Second, MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close(); _ = control.Close() })
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	key := "hc:cache:" + uuid.NewString()
	t.Cleanup(func() { _ = client.Del(ctx, key).Err() })
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)
	store := NewJSONStore(client, time.Minute, 1, m)
	var got map[string]int
	load := func() (any, error) { return map[string]int{"value": 7}, nil }
	if err := store.Load(ctx, key, &got, load); err != nil {
		t.Fatal(err)
	}
	otherReplica := NewJSONStore(client, time.Minute, 1, m)
	if hit, err := otherReplica.Get(ctx, key, &got); err != nil || !hit || got["value"] != 7 {
		t.Fatalf("real L2 read: value=%v hit=%v error=%v", got, hit, err)
	}
	if err := control.Do(ctx, "CLIENT", "PAUSE", 1000, "ALL").Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Do(ctx, "CLIENT", "UNPAUSE").Err() })
	faultStore := NewJSONStore(client, time.Minute, 1, m)
	started := time.Now()
	if err := faultStore.Load(ctx, key, &got, load); err != nil || got["value"] != 7 {
		t.Fatalf("Redis stall fallback: value=%v error=%v", got, err)
	}
	elapsed := time.Since(started)
	t.Logf("Redis stalled for 1s; cache fallback took %s (50ms I/O budget)", elapsed)
	// Allow scheduler noise, while rejecting stacked timeouts or default 3s I/O.
	if elapsed > 150*time.Millisecond {
		t.Fatalf("Redis fallback exceeded budget tolerance: %s", elapsed)
	}
	if cacheCounter(t, registry, "yunpan_cache_redis_errors_total", "get") != 1 || cacheCounter(t, registry, "yunpan_cache_source_loads_total", "success") != 2 {
		t.Fatal("fallback metrics do not describe the observed source calls")
	}
	if err := control.Do(ctx, "CLIENT", "UNPAUSE").Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	closedStore := NewJSONStore(client, time.Minute, 1, m)
	if err := closedStore.Load(ctx, key, &got, load); err != nil {
		t.Fatalf("unavailable Redis prevented source read: %v", err)
	}
}

func cacheCounter(t *testing.T, registry *prometheus.Registry, name, label string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == name {
			for _, metric := range family.Metric {
				if len(metric.Label) == 1 && metric.Label[0].GetValue() == label {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}
