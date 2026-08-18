// Package breaker 提供滑动窗口失败率熔断，带半开探测。
package breaker

import (
	"errors"
	"sync"
	"time"
)

// ErrOpen 表示熔断器处于打开状态，调用被直接拒绝，不会打到下游。
var ErrOpen = errors.New("circuit breaker is open")

// State 是熔断器三态。
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

type Config struct {
	// Window 是失败率统计的滑动窗口长度。
	Window time.Duration
	// Buckets 是窗口内的分桶数，桶越多窗口滑动越平滑。
	Buckets int
	// MinRequests 是窗口内触发熔断所需的最小样本量，避免单次失败即熔断。
	MinRequests int
	// FailureRate 是触发熔断的失败率阈值，取值 (0, 1]。
	FailureRate float64
	// OpenTimeout 是打开状态持续多久后转入半开探测。
	OpenTimeout time.Duration
	// HalfOpenProbes 是半开状态允许的探测请求数，全部成功才恢复关闭。
	HalfOpenProbes int
	// Now 供测试注入时钟。
	Now func() time.Time
}

func (c Config) withDefaults() Config {
	if c.Window <= 0 {
		c.Window = 10 * time.Second
	}
	if c.Buckets <= 0 {
		c.Buckets = 10
	}
	if c.MinRequests <= 0 {
		c.MinRequests = 20
	}
	if c.FailureRate <= 0 || c.FailureRate > 1 {
		c.FailureRate = 0.5
	}
	if c.OpenTimeout <= 0 {
		c.OpenTimeout = 5 * time.Second
	}
	if c.HalfOpenProbes <= 0 {
		c.HalfOpenProbes = 3
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

type bucket struct {
	successes int
	failures  int
}

// Breaker 是并发安全的失败率熔断器。
type Breaker struct {
	config Config

	mutex          sync.Mutex
	state          State
	buckets        []bucket
	cursor         int
	bucketStart    time.Time
	openedAt       time.Time
	probesInFlight int
	probeSuccesses int
}

func New(config Config) *Breaker {
	config = config.withDefaults()
	return &Breaker{
		config:      config,
		state:       StateClosed,
		buckets:     make([]bucket, config.Buckets),
		bucketStart: config.Now(),
	}
}

func (b *Breaker) State() State {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.advance(b.config.Now())
	return b.state
}

// Allow 判断是否放行。放行时返回记录回调，调用方必须在拿到结果后调用它。
func (b *Breaker) Allow() (func(success bool), error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	now := b.config.Now()
	b.advance(now)

	switch b.state {
	case StateOpen:
		return nil, ErrOpen
	case StateHalfOpen:
		if b.probesInFlight >= b.config.HalfOpenProbes {
			return nil, ErrOpen
		}
		b.probesInFlight++
		return b.record, nil
	default:
		return b.record, nil
	}
}

func (b *Breaker) record(success bool) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	now := b.config.Now()
	b.advance(now)

	if b.state == StateHalfOpen {
		if b.probesInFlight > 0 {
			b.probesInFlight--
		}
		if !success {
			// 半开探测失败立刻重新打开，不等窗口凑够样本。
			b.open(now)
			return
		}
		b.probeSuccesses++
		if b.probeSuccesses >= b.config.HalfOpenProbes {
			b.close()
		}
		return
	}

	if success {
		b.buckets[b.cursor].successes++
		return
	}
	b.buckets[b.cursor].failures++

	successes, failures := b.totals()
	total := successes + failures
	if total >= b.config.MinRequests && float64(failures)/float64(total) >= b.config.FailureRate {
		b.open(now)
	}
}

// advance 按经过的时间推进分桶游标，并清空被跨过的旧桶。
func (b *Breaker) advance(now time.Time) {
	if b.state == StateOpen && now.Sub(b.openedAt) >= b.config.OpenTimeout {
		b.state = StateHalfOpen
		b.probesInFlight = 0
		b.probeSuccesses = 0
	}

	bucketWidth := b.config.Window / time.Duration(b.config.Buckets)
	if bucketWidth <= 0 {
		return
	}
	elapsed := now.Sub(b.bucketStart)
	if elapsed < bucketWidth {
		return
	}
	steps := int(elapsed / bucketWidth)
	if steps >= b.config.Buckets {
		b.buckets = make([]bucket, b.config.Buckets)
		b.cursor = 0
		b.bucketStart = now
		return
	}
	for range steps {
		b.cursor = (b.cursor + 1) % b.config.Buckets
		b.buckets[b.cursor] = bucket{}
	}
	b.bucketStart = b.bucketStart.Add(time.Duration(steps) * bucketWidth)
}

func (b *Breaker) totals() (successes, failures int) {
	for _, item := range b.buckets {
		successes += item.successes
		failures += item.failures
	}
	return successes, failures
}

func (b *Breaker) open(now time.Time) {
	b.state = StateOpen
	b.openedAt = now
	b.probesInFlight = 0
	b.probeSuccesses = 0
	b.buckets = make([]bucket, b.config.Buckets)
	b.cursor = 0
	b.bucketStart = now
}

func (b *Breaker) close() {
	b.state = StateClosed
	b.probesInFlight = 0
	b.probeSuccesses = 0
	b.buckets = make([]bucket, b.config.Buckets)
	b.cursor = 0
	b.bucketStart = b.config.Now()
}
