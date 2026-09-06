package httpapi

import "sync/atomic"

// Readiness is a process-local drain gate. It lets shutdown advertise NOT
// READY before the listener is closed so a load balancer stops new requests.
type Readiness struct{ ready atomic.Bool }

func NewReadiness(initial bool) *Readiness {
	gate := &Readiness{}
	gate.ready.Store(initial)
	return gate
}

func (r *Readiness) SetReady(ready bool) {
	if r != nil {
		r.ready.Store(ready)
	}
}

func (r *Readiness) Ready() bool { return r == nil || r.ready.Load() }
