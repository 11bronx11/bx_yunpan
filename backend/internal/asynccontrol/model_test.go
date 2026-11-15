package asynccontrol

import (
	"testing"
	"time"
)

func TestCanonicalIdentity(t *testing.T) {
	a, _, err := Digest([]byte(`{"b":2,"a":{"x":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := Digest([]byte("{\"a\": {\"x\":1}, \"b\":2}"))
	if err != nil || a != b {
		t.Fatal("JSON key order changed identity")
	}
	c, _, _ := Digest([]byte(`{"a":{"x":2},"b":2}`))
	if c == a {
		t.Fatal("changed input retained digest")
	}
	if _, _, err := Digest([]byte(`{} {}`)); err == nil {
		t.Fatal("accepted trailing JSON")
	}
}

func TestRetryBounds(t *testing.T) {
	for i := 1; i <= 1000; i++ {
		d := Backoff(i, 5*time.Second, 5*time.Minute, 0)
		if d < 5*time.Second || d > 5*time.Minute {
			t.Fatal(i, d)
		}
	}
	if Backoff(2, 5*time.Second, 5*time.Minute, 0) != 10*time.Second {
		t.Fatal("not exponential")
	}
	if Backoff(1, 5*time.Second, 5*time.Minute, -1) != 4*time.Second {
		t.Fatal("jitter")
	}
	for _, state := range []string{Succeeded, Superseded, Rejected, Failed, Quarantined, NeedsAttention} {
		if !Terminal(state) {
			t.Fatal(state)
		}
	}
	if Terminal(Exhausted) || Terminal(Converging) || Terminal(Computed) {
		t.Fatal("unfinished effects treated terminal")
	}
}

func TestShardContract(t *testing.T) {
	for i := 0; i < 100; i++ {
		if Shard("owner:stable", 32) != Shard("owner:stable", 32) {
			t.Fatal("unstable shard")
		}
	}
	r := CoreRegistry()
	if _, err := r.Validate(Spec{Kind: "arbitrary.sql", SemanticKey: "x"}); err == nil {
		t.Fatal("unknown contract")
	}
}
