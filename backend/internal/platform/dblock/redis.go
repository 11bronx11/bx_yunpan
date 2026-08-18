package dblock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	redis "github.com/redis/go-redis/v9"
)

// ErrNotAcquired 表示锁被他人持有，调用方应当放弃本轮而不是重试到超时。
var ErrNotAcquired = errors.New("dblock: lock not acquired")

// releaseScript 用 Lua 保证「校验 token + 删除」是原子的。
//
// 不校验 token 直接 DEL 会误删他人的锁：本实例的锁可能已因超时过期、
// 键被另一个实例重新持有，此时 DEL 删掉的是别人正在用的锁。
var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)

// renewScript 同理：只有仍持有该 token 才续期，避免给别人的锁延命。
var renewScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0
`)

// RedisLock 是跨事务边界的互斥锁。
//
// 与同包的 Transaction（pg_advisory_xact_lock）分工明确：
//   - advisory lock 在事务边界内、随事务提交或回滚自动释放，不存在
//     「锁过期了业务还在跑」的窗口，也没有误删问题，语义更强。上传会话
//     创建、同名文件写入串行化继续用它。
//   - RedisLock 用于跨事务边界、需要跨进程互斥的场景，例如 Outbox
//     dispatcher 多副本选主——那里没有一个覆盖全过程的数据库事务可依附。
//
// 没有采用 Redlock：它假设多节点时钟漂移可控、进程不会长时间 GC pause，
// 这两条在实践中都不牢靠。这里锁失效的后果只是重复投递，而下游任务有
// 幂等保护（Asynq TaskID 去重 + 业务侧 dedupe），因此选了单实例简单方案。
// 若将来需要真正安全的互斥，应换成带 fencing token 的方案或 etcd lease。
type RedisLock struct {
	client redis.UniversalClient
	key    string
	ttl    time.Duration
	token  string
}

// NewRedisLock 构造一把未持有的锁。ttl 是锁的过期时间，持锁期间可用
// Watchdog 续期。
func NewRedisLock(client redis.UniversalClient, key string, ttl time.Duration) *RedisLock {
	return &RedisLock{client: client, key: key, ttl: ttl}
}

// Acquire 尝试获取锁：SET key token NX PX ttl。
// 未抢到返回 ErrNotAcquired，调用方据此区分「别人持有」与「Redis 故障」。
func (l *RedisLock) Acquire(ctx context.Context) error {
	token, err := newToken()
	if err != nil {
		return err
	}
	ok, err := l.client.SetArgs(ctx, l.key, token, redis.SetArgs{Mode: "NX", TTL: l.ttl}).Result()
	if errors.Is(err, redis.Nil) {
		return ErrNotAcquired
	}
	if err != nil {
		return err
	}
	if ok != "OK" {
		return ErrNotAcquired
	}
	l.token = token
	return nil
}

// Renew 续期。返回 ErrNotAcquired 表示锁已不属于本实例——此时调用方
// 必须停止临界区工作，不能假装还持有。
func (l *RedisLock) Renew(ctx context.Context) error {
	if l.token == "" {
		return ErrNotAcquired
	}
	result, err := renewScript.Run(ctx, l.client, []string{l.key}, l.token, l.ttl.Milliseconds()).Int64()
	if err != nil {
		return err
	}
	if result == 0 {
		return ErrNotAcquired
	}
	return nil
}

// Release 释放锁。token 不匹配时什么都不做，返回 nil：这不是错误，
// 只说明锁早已过期并被他人接手。
func (l *RedisLock) Release(ctx context.Context) error {
	if l.token == "" {
		return nil
	}
	token := l.token
	l.token = ""
	return releaseScript.Run(ctx, l.client, []string{l.key}, token).Err()
}

// Held 报告本实例是否认为自己持有锁（不查 Redis）。
func (l *RedisLock) Held() bool { return l.token != "" }

// Watchdog 在后台按 ttl/3 的间隔续期，直到 ctx 结束或续期失败。
// 失败时关闭返回的 channel，通知调用方立刻退出临界区。
//
// 间隔取 ttl/3 而不是 ttl/2：留出至少两次重试机会，一次网络抖动不会
// 直接丢锁。
func (l *RedisLock) Watchdog(ctx context.Context, logger *slog.Logger) <-chan struct{} {
	lost := make(chan struct{})
	interval := l.ttl / 3
	if interval <= 0 {
		interval = time.Second
	}
	go func() {
		defer close(lost)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := l.Renew(ctx); err != nil {
					if ctx.Err() == nil && logger != nil {
						logger.WarnContext(ctx, "renew distributed lock", "key", l.key, "error", err)
					}
					return
				}
			}
		}
	}()
	return lost
}

func newToken() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
