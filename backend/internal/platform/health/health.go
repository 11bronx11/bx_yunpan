package health

import (
	"context"
	"errors"
	"sync"
	"time"
)

type Probe interface {
	Name() string
	Check(context.Context) error
}

type ProbeFunc struct {
	ProbeName string
	Func      func(context.Context) error
}

func (p ProbeFunc) Name() string {
	return p.ProbeName
}

func (p ProbeFunc) Check(ctx context.Context) error {
	return p.Func(ctx)
}

type Result struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

func Run(ctx context.Context, timeout time.Duration, probes []Probe) (Result, error) {
	result := Result{Status: "ok", Checks: make(map[string]string, len(probes))}
	if len(probes) == 0 {
		return result, nil
	}

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		failures []error
	)
	for _, probe := range probes {
		probe := probe
		wg.Add(1)
		go func() {
			defer wg.Done()
			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			err := probe.Check(probeCtx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				result.Checks[probe.Name()] = "unavailable"
				failures = append(failures, err)
				return
			}
			result.Checks[probe.Name()] = "ok"
		}()
	}
	wg.Wait()

	if len(failures) > 0 {
		result.Status = "unavailable"
		return result, errors.Join(failures...)
	}
	return result, nil
}
