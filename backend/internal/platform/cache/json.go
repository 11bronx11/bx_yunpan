package cache

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"
	redis "github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/breaker"
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
	circuit  *breaker.Breaker
}

const (
	redisTimeout       = 50 * time.Millisecond
	ttlJitterRatio     = 0.2
	cacheLeaseTTL      = 5 * time.Second
	cacheLeaseWait     = 40 * time.Millisecond
	cacheLeasePollBase = 5 * time.Millisecond
)

var releaseCacheLease = redis.NewScript(`
	if redis.call("GET", KEYS[1]) == ARGV[1] then
		return redis.call("DEL", KEYS[1])
	end
	return 0
`)

func NewJSONStore(client redis.UniversalClient, ttl time.Duration, maxItems int, metrics ...*Metrics) *JSONStore {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if maxItems <= 0 {
		maxItems = 512
	}
	store := &JSONStore{
		redis: client, ttl: ttl, maxItems: maxItems,
		local: make(map[string]*list.Element), lru: list.New(),
		circuit: breaker.New(breaker.Config{}),
	}
	if len(metrics) > 0 {
		store.metrics = metrics[0]
	}
	store.metrics.setBreakerState(breaker.StateClosed)
	return store
}

func (s *JSONStore) Get(ctx context.Context, key string, target any) (bool, error) {
	if s == nil || key == "" {
		return false, nil
	}
	raw, ok, _ := s.getRaw(ctx, key)
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
			return &sourceLoadError{cause: err}
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
		redisCtx, cancel := context.WithTimeout(ctx, redisTimeout)
		raw, hit, redisErr := s.getRaw(redisCtx, key)
		if hit {
			cancel()
			return raw, nil
		}

		writeShared := false
		if s.redis != nil && redisErr == nil && redisCtx.Err() == nil {
			token, acquired, contended, lockErr := s.acquireLease(redisCtx, key)
			if lockErr == nil && acquired {
				defer s.releaseLease(key, token)
				if cached, found, readErr := s.getRaw(redisCtx, key); found {
					cancel()
					return cached, nil
				} else if readErr == nil && redisCtx.Err() == nil {
					writeShared = true
				}
			} else if lockErr == nil && contended {
				if cached, found := s.waitForLeaseResult(redisCtx, key); found {
					cancel()
					return cached, nil
				}
			}
		}
		cancel()
		return s.loadSource(ctx, key, load, writeShared)
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

func (s *JSONStore) loadSource(ctx context.Context, key string, load func() (any, error), writeShared bool) ([]byte, error) {
	loaded, loadErr := load()
	s.metrics.source(loadErr)
	if loadErr != nil {
		return nil, &sourceLoadError{cause: loadErr}
	}
	raw, marshalErr := json.Marshal(loaded)
	if marshalErr != nil {
		return nil, marshalErr
	}
	ttl := s.jitteredTTL()
	s.putLocal(key, raw, time.Now().Add(ttl))
	if writeShared {
		s.writeRedis(ctx, key, raw, ttl)
	}
	return raw, nil
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
	done, allowed := s.allowRedis()
	if !allowed {
		return nil
	}
	err := s.redis.Del(redisCtx, key).Err()
	done(err == nil)
	s.metrics.redisError("delete", err)
	return err
}

func (s *JSONStore) getRaw(ctx context.Context, key string) ([]byte, bool, error) {
	if raw, ok := s.getLocal(key); ok {
		s.metrics.lookup("l1_hit")
		return raw, true, nil
	}
	if s.redis == nil {
		s.metrics.lookup("miss")
		return nil, false, nil
	}
	redisCtx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()
	done, allowed := s.allowRedis()
	if !allowed {
		s.metrics.lookup("miss")
		return nil, false, breaker.ErrOpen
	}
	raw, remaining, hit, err := s.readRedis(redisCtx, key)
	done(err == nil)
	if err != nil {
		s.metrics.redisError("get", err)
	}
	if !hit {
		s.metrics.lookup("miss")
		return nil, false, err
	}
	if remaining <= 0 {
		remaining = s.jitteredTTL()
	}
	s.putLocal(key, raw, time.Now().Add(remaining))
	s.metrics.lookup("l2_hit")
	return raw, true, err
}

func (s *JSONStore) readRedis(ctx context.Context, key string) ([]byte, time.Duration, bool, error) {
	var valueCmd *redis.StringCmd
	var ttlCmd *redis.DurationCmd
	_, execErr := s.redis.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		valueCmd = pipe.Get(ctx, key)
		ttlCmd = pipe.PTTL(ctx, key)
		return nil
	})
	if valueCmd == nil || ttlCmd == nil {
		return nil, 0, false, execErr
	}
	raw, err := valueCmd.Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	if execErr != nil {
		return raw, s.jitteredTTL(), true, execErr
	}
	remaining, err := ttlCmd.Result()
	if err != nil {
		return raw, s.jitteredTTL(), true, err
	}
	if remaining == -2*time.Millisecond {
		return nil, 0, false, nil
	}
	if remaining <= 0 {
		remaining = s.jitteredTTL()
	} else if remaining > time.Millisecond {
		remaining -= time.Millisecond
	}
	return raw, remaining, true, nil
}

func (s *JSONStore) acquireLease(ctx context.Context, key string) (string, bool, bool, error) {
	done, allowed := s.allowRedis()
	if !allowed {
		return "", false, false, breaker.ErrOpen
	}
	token := uuid.NewString()
	acquired, err := s.redis.SetNX(ctx, leaseKey(key), token, cacheLeaseTTL).Result()
	done(err == nil)
	if err != nil {
		s.metrics.redisError("lock", err)
		return "", false, false, err
	}
	return token, acquired, !acquired, nil
}

func (s *JSONStore) waitForLeaseResult(ctx context.Context, key string) ([]byte, bool) {
	deadline := time.Now().Add(cacheLeaseWait)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 || ctx.Err() != nil {
			return nil, false
		}
		delay := cacheLeasePollBase + time.Duration(rand.Int63n(int64(cacheLeasePollBase)))
		if delay > remaining {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, false
		case <-timer.C:
		}
		if raw, hit, _ := s.getRaw(ctx, key); hit {
			return raw, true
		}
	}
}

func (s *JSONStore) releaseLease(key, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), redisTimeout)
	defer cancel()
	_, err := releaseCacheLease.Run(ctx, s.redis, []string{leaseKey(key)}, token).Result()
	s.metrics.redisError("unlock", err)
}

func (s *JSONStore) writeRedis(parent context.Context, key string, raw []byte, ttl time.Duration) {
	if s.redis == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, redisTimeout)
	defer cancel()
	done, allowed := s.allowRedis()
	if !allowed {
		return
	}
	err := s.redis.Set(ctx, key, raw, ttl).Err()
	done(err == nil)
	s.metrics.redisError("set", err)
}

func (s *JSONStore) allowRedis() (func(bool), bool) {
	done, err := s.circuit.Allow()
	if err != nil {
		s.metrics.breakerRejectedCall()
		s.metrics.setBreakerState(s.circuit.State())
		return nil, false
	}
	s.metrics.setBreakerState(s.circuit.State())
	return func(success bool) {
		done(success)
		s.metrics.setBreakerState(s.circuit.State())
	}, true
}

func (s *JSONStore) jitteredTTL() time.Duration {
	spread := time.Duration(float64(s.ttl) * ttlJitterRatio)
	if spread <= 0 {
		return s.ttl
	}
	return s.ttl - spread + time.Duration(rand.Int63n(int64(2*spread)+1))
}

func leaseKey(key string) string { return key + ":fill-lease" }

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
