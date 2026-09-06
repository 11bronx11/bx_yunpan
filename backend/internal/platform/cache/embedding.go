// Package cache contains optional derived-data caches. Cache misses and cache
// outages must never become a source-of-truth failure for a request.
package cache

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	redis "github.com/redis/go-redis/v9"
)

type embeddingEntry struct {
	value     []float32
	expiresAt time.Time
}

// EmbeddingStore is a small two-level cache: a bounded process-local map in
// front of Redis. The Redis client is optional so unit tests and development
// startup can run without a cache instance. Errors are returned to the caller,
// which should fall back to the provider.
type EmbeddingStore struct {
	redis    *redis.Client
	ttl      time.Duration
	maxItems int
	mu       sync.Mutex
	local    map[string]embeddingEntry
}

func NewEmbeddingStore(client *redis.Client, ttl time.Duration, maxItems int) *EmbeddingStore {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if maxItems <= 0 {
		maxItems = 512
	}
	return &EmbeddingStore{redis: client, ttl: ttl, maxItems: maxItems, local: make(map[string]embeddingEntry)}
}

func (s *EmbeddingStore) Get(ctx context.Context, key string) ([]float32, bool, error) {
	if s == nil || key == "" {
		return nil, false, nil
	}
	now := time.Now()
	s.mu.Lock()
	entry, ok := s.local[key]
	if ok && now.Before(entry.expiresAt) {
		value := append([]float32(nil), entry.value...)
		s.mu.Unlock()
		return value, true, nil
	}
	if ok {
		delete(s.local, key)
	}
	s.mu.Unlock()
	if s.redis == nil {
		return nil, false, nil
	}
	raw, err := s.redis.Get(ctx, key).Bytes()
	if err != nil {
		if err == redis.Nil {
			return nil, false, nil
		}
		return nil, false, err
	}
	var value []float32
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false, err
	}
	s.putLocal(key, value, now.Add(s.ttl))
	return append([]float32(nil), value...), true, nil
}

func (s *EmbeddingStore) Set(ctx context.Context, key string, value []float32) error {
	if s == nil || key == "" || len(value) == 0 {
		return nil
	}
	expiresAt := time.Now().Add(s.ttl)
	s.putLocal(key, value, expiresAt)
	if s.redis == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.redis.Set(ctx, key, raw, s.ttl).Err()
}

func (s *EmbeddingStore) putLocal(key string, value []float32, expiresAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.local) >= s.maxItems {
		oldestKey := ""
		var oldest time.Time
		for candidate, entry := range s.local {
			if oldestKey == "" || entry.expiresAt.Before(oldest) {
				oldestKey, oldest = candidate, entry.expiresAt
			}
		}
		if oldestKey != "" {
			delete(s.local, oldestKey)
		}
	}
	s.local[key] = embeddingEntry{value: append([]float32(nil), value...), expiresAt: expiresAt}
}
