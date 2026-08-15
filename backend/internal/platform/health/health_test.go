package health

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunReportsUnavailableProbe(t *testing.T) {
	probes := []Probe{
		ProbeFunc{ProbeName: "postgres", Func: func(context.Context) error { return nil }},
		ProbeFunc{ProbeName: "redis", Func: func(context.Context) error { return errors.New("offline") }},
	}

	result, err := Run(context.Background(), time.Second, probes)
	if err == nil {
		t.Fatal("expected readiness failure")
	}
	if result.Status != "unavailable" || result.Checks["redis"] != "unavailable" {
		t.Fatalf("unexpected result: %#v", result)
	}
}
