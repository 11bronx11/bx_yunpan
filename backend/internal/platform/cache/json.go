package cache

import (
	"container/list"
	"context"
	"encoding/json"
	"sync"
	"time"

	redis "github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

type jsonEntry struct {
	key       string
	value     []byte
	expiresAt time.Time
}

type jsonItem struct {
	entry jsonEntry
}

// JSONStore 用于缓存可重建的业务读模型，不承载权限和写入事实。
type JSONStore struct {
	redis    redis.UniversalClient
	ttl      time.Duration
	maxItems int
	mu       sync.Mutex
	local    map[string]*list.Element
	lru      *list.List
	group    singleflight.Group
	metrics  *Metrics
}

const redisTimeout = 50 * time.Millisecond

func NewJSONStore(client redis.UniversalClient, ttl time.Duration, maxItems int, metrics ...*Metrics) *JSONStore {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if maxItems <= 0 {
		maxItems = 512
	}
	store := &JSONStore{redis: client, ttl: ttl, maxItems: maxItems, local: make(map[string]*list.Element), lru: list.New()}
	if len(metrics) > 0 {
		store.metrics = metrics[0]
	}
	return store
}

func (s *JSONStore) Get(ctx context.Context, key string, target any) (bool, error) {
	if s == nil || key == "" {
		return false, nil
	}
	raw, ok := s.getRaw(ctx, key)
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return false, err
	}
	return true, nil
}

// Load 合并同一 key 的并发回源，避免缓存刚过期时打爆数据库。
func (s *JSONStore) Load(ctx context.Context, key string, target any, load func() (any, error)) error {
	if s == nil || key == "" {
		value, err := load()
		if err != nil {
			return err
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, target)
	}
	if raw, hit := s.getLocal(key); hit {
		s.metrics.lookup("l1_hit")
		return json.Unmarshal(raw, target)
	}
	value, err, shared := s.group.Do(key, func() (any, error) {
		// Share one Redis budget across the read and best-effort cache fill.
		redisCtx, cancel := context.WithTimeout(ctx, redisTimeout)
		defer cancel()
		if raw, hit := s.getRaw(redisCtx, key); hit {
			return raw, nil
		}
		loaded, loadErr := load()
		s.metrics.source(loadErr)
		if loadErr != nil {
			return nil, loadErr
		}
		raw, marshalErr := json.Marshal(loaded)
		if marshalErr != nil {
			return nil, marshalErr
		}
		s.putLocal(key, raw, time.Now().Add(s.ttl))
		if s.redis != nil && redisCtx.Err() == nil {
			s.metrics.redisError("set", s.redis.Set(redisCtx, key, raw, s.ttl).Err())
		}
		return raw, nil
	})
	if shared && s.metrics != nil {
		s.metrics.shared.Inc()
	}
	if err != nil {
		return err
	}
	if value == nil {
		return nil
	}
	raw, ok := value.([]byte)
	if !ok || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, target)
}

func (s *JSONStore) Delete(ctx context.Context, key string) error {
	if s == nil || key == "" {
		return nil
	}
	s.mu.Lock()
	if element := s.local[key]; element != nil {
		delete(s.local, key)
		s.lru.Remove(element)
	}
	s.mu.Unlock()
	if s.redis == nil {
		return nil
	}
	redisCtx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()
	err := s.redis.Del(redisCtx, key).Err()
	s.metrics.redisError("delete", err)
	return err
}

func (s *JSONStore) getRaw(ctx context.Context, key string) ([]byte, bool) {
	if raw, ok := s.getLocal(key); ok {
		s.metrics.lookup("l1_hit")
		return raw, true
	}
	if s.redis == nil {
		s.metrics.lookup("miss")
		return nil, false
	}
	redisCtx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()
	raw, err := s.redis.Get(redisCtx, key).Bytes()
	if err != nil {
		// Redis 只是加速层，故障时回源数据库。
		s.metrics.lookup("miss")
		if err != redis.Nil {
			s.metrics.redisError("get", err)
		}
		return nil, false
	}
	s.putLocal(key, raw, time.Now().Add(s.ttl))
	s.metrics.lookup("l2_hit")
	return raw, true
}

func (s *JSONStore) getLocal(key string) ([]byte, bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	element := s.local[key]
	if element == nil {
		return nil, false
	}
	entry := element.Value.(jsonItem).entry
	if !now.Before(entry.expiresAt) {
		delete(s.local, key)
		s.lru.Remove(element)
		return nil, false
	}
	s.lru.MoveToFront(element)
	return append([]byte(nil), entry.value...), true
}

func (s *JSONStore) putLocal(key string, raw []byte, expiresAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if element := s.local[key]; element != nil {
		element.Value = jsonItem{entry: jsonEntry{key: key, value: append([]byte(nil), raw...), expiresAt: expiresAt}}
		s.lru.MoveToFront(element)
		return
	}
	element := s.lru.PushFront(jsonItem{entry: jsonEntry{key: key, value: append([]byte(nil), raw...), expiresAt: expiresAt}})
	s.local[key] = element
	for len(s.local) > s.maxItems {
		oldest := s.lru.Back()
		if oldest == nil {
			break
		}
		delete(s.local, oldest.Value.(jsonItem).entry.key)
		s.lru.Remove(oldest)
		if s.metrics != nil {
			s.metrics.evictions.Inc()
		}
	}
}
