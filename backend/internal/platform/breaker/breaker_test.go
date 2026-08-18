package breaker

import (
	"errors"
	"testing"
	"time"
)

// clock 是可手动推进的测试时钟。
type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestBreaker(now *clock) *Breaker {
	return New(Config{
		Window:         time.Second,
		Buckets:        4,
		MinRequests:    4,
		FailureRate:    0.5,
		OpenTimeout:    time.Second,
		HalfOpenProbes: 2,
		Now:            now.Now,
	})
}

func record(t *testing.T, b *Breaker, success bool) {
	t.Helper()
	done, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	done(success)
}

func TestBreakerOpensOnFailureRate(t *testing.T) {
	now := &clock{now: time.Unix(0, 0)}
	b := newTestBreaker(now)

	for range 4 {
		record(t, b, false)
	}
	if b.State() != StateOpen {
		t.Fatalf("state = %s, want open", b.State())
	}
	if _, err := b.Allow(); !errors.Is(err, ErrOpen) {
		t.Fatalf("Allow after open = %v, want ErrOpen", err)
	}
}

func TestBreakerStaysClosedBelowMinRequests(t *testing.T) {
	now := &clock{now: time.Unix(0, 0)}
	b := newTestBreaker(now)

	// 全部失败但样本量不足，不应熔断。
	for range 3 {
		record(t, b, false)
	}
	if b.State() != StateClosed {
		t.Fatalf("state = %s, want closed", b.State())
	}
}

func TestBreakerHalfOpenRecoversAfterProbes(t *testing.T) {
	now := &clock{now: time.Unix(0, 0)}
	b := newTestBreaker(now)
	for range 4 {
		record(t, b, false)
	}

	now.advance(time.Second)
	if b.State() != StateHalfOpen {
		t.Fatalf("state = %s, want half_open", b.State())
	}
	// 两次探测成功后恢复关闭。
	record(t, b, true)
	record(t, b, true)
	if b.State() != StateClosed {
		t.Fatalf("state = %s, want closed", b.State())
	}
}

func TestBreakerHalfOpenReopensOnProbeFailure(t *testing.T) {
	now := &clock{now: time.Unix(0, 0)}
	b := newTestBreaker(now)
	for range 4 {
		record(t, b, false)
	}
	now.advance(time.Second)

	// 半开探测失败立刻重新打开，不等窗口凑够样本。
	record(t, b, false)
	if b.State() != StateOpen {
		t.Fatalf("state = %s, want open", b.State())
	}
}

func TestBreakerLimitsHalfOpenProbes(t *testing.T) {
	now := &clock{now: time.Unix(0, 0)}
	b := newTestBreaker(now)
	for range 4 {
		record(t, b, false)
	}
	now.advance(time.Second)

	first, err := b.Allow()
	if err != nil {
		t.Fatalf("first probe: %v", err)
	}
	second, err := b.Allow()
	if err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if _, err := b.Allow(); !errors.Is(err, ErrOpen) {
		t.Fatal("third probe should be rejected")
	}
	first(true)
	second(true)
}

func TestBreakerForgetsOldFailuresAfterWindow(t *testing.T) {
	now := &clock{now: time.Unix(0, 0)}
	b := newTestBreaker(now)

	record(t, b, false)
	record(t, b, false)
	// 整个窗口滑过后旧失败被丢弃，再来两次失败不足以凑够样本量。
	now.advance(2 * time.Second)
	record(t, b, false)
	record(t, b, false)
	if b.State() != StateClosed {
		t.Fatalf("state = %s, want closed", b.State())
	}
}
