package asynccontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/testdb"
)

func TestBackoffDurationExtremes(t *testing.T) {
	for _, jitter := range []float64{-1, 0, 1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		for _, base := range []time.Duration{1, time.Second, math.MaxInt64 / 2, math.MaxInt64} {
			for _, attempt := range []int{0, 1, 2, 1000, math.MaxInt} {
				got := Backoff(attempt, base, time.Duration(math.MaxInt64), jitter)
				if got <= 0 {
					t.Errorf("nonpositive retry: attempt=%d base=%d jitter=%v got=%d", attempt, base, jitter, got)
				}
			}
		}
	}
}

func FuzzBackoffBounds(f *testing.F) {
	f.Add(int64(1), int64(1), int64(1), float64(-1))
	f.Add(int64(1000), int64(math.MaxInt64), int64(math.MaxInt64), float64(1))
	f.Add(int64(2), int64(time.Second), int64(time.Minute), math.NaN())
	f.Fuzz(func(t *testing.T, attempt, base, cap int64, jitter float64) {
		got := Backoff(int(attempt), time.Duration(base), time.Duration(cap), jitter)
		if base <= 0 || cap <= 0 {
			if got != 0 {
				t.Fatal("invalid budget must disable backoff", got)
			}
			return
		}
		if got <= 0 || got > time.Duration(cap) {
			t.Fatalf("backoff outside positive budget: %d, cap %d", got, cap)
		}
	})
}

func FuzzDigestRoundTrip(f *testing.F) {
	for _, seed := range []string{`{"n":9007199254740993}`, `{"n":9007199254740992}`, `{"a":1,"a":2}`, `{"中文":"\u4e2d文"}`, `null`, `[]`, `{} {}`, `1e10000`, `{"nested":[true,false,null]}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 1<<20 {
			t.Skip()
		}
		digest, canonical, err := Digest(input)
		if err != nil {
			return
		}
		again, normalized, err := Digest(canonical)
		if err != nil || digest != again || string(canonical) != string(normalized) {
			t.Fatal("accepted input did not have a stable canonical identity")
		}
		sum := sha256.Sum256(canonical)
		if digest != hex.EncodeToString(sum[:]) {
			t.Fatal("digest does not identify canonical bytes")
		}
	})
}

func TestConcurrentClaimsHaveOneExecution(t *testing.T) {
	s := New(testdb.Control(t))
	ctx := context.Background()
	if _, err := s.Ensure(ctx, spec("concurrent-claim"), "source"); err != nil {
		t.Fatal(err)
	}
	reserved, ok, err := s.Reserve(ctx, "upload")
	if err != nil || !ok {
		t.Fatal("reserve", ok, err)
	}
	var claims atomic.Int32
	var wg sync.WaitGroup
	barrier := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-barrier
			_, claimed, err := s.Claim(ctx, reserved.Notice())
			if err != nil {
				t.Error(err)
			}
			if claimed {
				claims.Add(1)
			}
		}()
	}
	close(barrier)
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatal("valid concurrent claim count", claims.Load())
	}
	var stored Task
	if err := s.DB.First(&stored, "id=?", reserved.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Attempts != 1 || stored.Generation != 1 || stored.State != Running {
		t.Fatal("duplicate notifications consumed an execution", stored.Attempts, stored.Generation, stored.State)
	}
}
