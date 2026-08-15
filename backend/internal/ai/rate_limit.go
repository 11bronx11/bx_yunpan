package ai

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	redis "github.com/redis/go-redis/v9"
)

type RequestLimiter interface {
	Allow(context.Context, uuid.UUID, string) (bool, error)
}

type RateLimits struct {
	SearchPerMinute    int
	AskPerMinute       int
	ReprocessPerMinute int
}

type redisRequestLimiter struct {
	client *redis.Client
	limits RateLimits
}

var incrementWindow = redis.NewScript(`
local count = redis.call("INCR", KEYS[1])
if count == 1 then
  redis.call("EXPIRE", KEYS[1], ARGV[1])
end
return count
`)

func NewRequestLimiter(client *redis.Client, limits RateLimits) RequestLimiter {
	return &redisRequestLimiter{client: client, limits: limits}
}

func (l *redisRequestLimiter) Allow(ctx context.Context, ownerID uuid.UUID, scope string) (bool, error) {
	limit := l.limit(scope)
	if limit <= 0 {
		return true, nil
	}
	window := time.Now().UTC().Unix() / 60
	key := fmt.Sprintf("ai:rate:%s:%s:%d", scope, ownerID, window)
	count, err := incrementWindow.Run(ctx, l.client, []string{key}, 120).Int64()
	if err != nil {
		return true, err
	}
	return count <= int64(limit), nil
}

func (l *redisRequestLimiter) limit(scope string) int {
	switch scope {
	case "search":
		return l.limits.SearchPerMinute
	case "ask":
		return l.limits.AskPerMinute
	case "reprocess":
		return l.limits.ReprocessPerMinute
	default:
		return 0
	}
}
