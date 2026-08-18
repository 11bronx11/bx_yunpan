package dblock

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	redis "github.com/redis/go-redis/v9"
)

func newTestLock(t *testing.T, key string, ttl time.Duration) (*miniredis.Miniredis, *RedisLock) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return server, NewRedisLock(client, key, ttl)
}

func TestAcquireIsExclusive(t *testing.T) {
	server, first := newTestLock(t, "lock:exclusive", time.Minute)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	second := NewRedisLock(client, "lock:exclusive", time.Minute)

	if err := first.Acquire(context.Background()); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := second.Acquire(context.Background()); err != ErrNotAcquired {
		t.Fatalf("second acquire = %v, want ErrNotAcquired", err)
	}
	if second.Held() {
		t.Fatal("second lock reports held after losing the race")
	}
}

// 释放必须校验 token：本实例的锁过期后可能已被他人接手，无脑 DEL 会删掉
// 别人正在用的锁。
func TestReleaseDoesNotDeleteAnotherHoldersLock(t *testing.T) {
	server, lock := newTestLock(t, "lock:token", time.Minute)
	if err := lock.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// 模拟锁已过期并被另一个实例重新持有。
	if err := server.Set("lock:token", "another-holder-token"); err != nil {
		t.Fatalf("overwrite lock: %v", err)
	}
	if err := lock.Release(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}
	value, err := server.Get("lock:token")
	if err != nil {
		t.Fatalf("lock key was deleted: %v", err)
	}
	if value != "another-holder-token" {
		t.Fatalf("lock value = %q, want another-holder-token", value)
	}
}

func TestReleaseFreesOwnLock(t *testing.T) {
	server, lock := newTestLock(t, "lock:release", time.Minute)
	if err := lock.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := lock.Release(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}
	if server.Exists("lock:release") {
		t.Fatal("lock key still exists after release")
	}
	if lock.Held() {
		t.Fatal("lock reports held after release")
	}
	// 释放后可以重新获取。
	if err := lock.Acquire(context.Background()); err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
}

func TestLockExpiresAfterTTL(t *testing.T) {
	server, lock := newTestLock(t, "lock:ttl", 5*time.Second)
	if err := lock.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	server.FastForward(6 * time.Second)
	if server.Exists("lock:ttl") {
		t.Fatal("lock key survived its TTL")
	}
	// 过期后续期必须失败：调用方据此知道自己已经不再持锁。
	if err := lock.Renew(context.Background()); err != ErrNotAcquired {
		t.Fatalf("renew after expiry = %v, want ErrNotAcquired", err)
	}
}

func TestRenewExtendsTTL(t *testing.T) {
	server, lock := newTestLock(t, "lock:renew", 5*time.Second)
	if err := lock.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	server.FastForward(4 * time.Second)
	if err := lock.Renew(context.Background()); err != nil {
		t.Fatalf("renew: %v", err)
	}
	// 续期后再走过原 TTL 的剩余时间，锁应仍在。
	server.FastForward(2 * time.Second)
	if !server.Exists("lock:renew") {
		t.Fatal("lock key expired despite renewal")
	}
}

func TestRenewFailsForAnotherHolder(t *testing.T) {
	server, lock := newTestLock(t, "lock:renew-other", time.Minute)
	if err := lock.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := server.Set("lock:renew-other", "another-holder-token"); err != nil {
		t.Fatalf("overwrite lock: %v", err)
	}
	if err := lock.Renew(context.Background()); err != ErrNotAcquired {
		t.Fatalf("renew = %v, want ErrNotAcquired", err)
	}
}

// watchdog 丢锁后必须关闭 channel，否则 dispatcher 会在已经不持锁的情况下
// 继续投递。
func TestWatchdogSignalsLostLock(t *testing.T) {
	server, lock := newTestLock(t, "lock:watchdog", 300*time.Millisecond)
	if err := lock.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lost := lock.Watchdog(ctx, nil)

	if err := server.Set("lock:watchdog", "another-holder-token"); err != nil {
		t.Fatalf("overwrite lock: %v", err)
	}
	select {
	case <-lost:
	case <-time.After(3 * time.Second):
		t.Fatal("watchdog did not report the lost lock")
	}
}
