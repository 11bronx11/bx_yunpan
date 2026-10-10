package taskqueue

import (
	"context"
	"sort"
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"
)

type queuedDelivery struct {
	queue string
	value amqp.Delivery
}
type weightedQueue struct {
	name            string
	weight, current int
	pending         []amqp.Delivery
}
type deliveryScheduler struct {
	mu      sync.Mutex
	queues  []weightedQueue
	changed chan struct{}
}

func newDeliveryScheduler(weights map[string]int) *deliveryScheduler {
	s := &deliveryScheduler{changed: make(chan struct{})}
	names := make([]string, 0, len(weights))
	for q := range weights {
		names = append(names, q)
	}
	sort.Strings(names)
	for _, q := range names {
		w := weights[q]
		if w <= 0 {
			w = 1
		}
		s.queues = append(s.queues, weightedQueue{name: q, weight: w})
	}
	return s
}
func (s *deliveryScheduler) put(q string, d amqp.Delivery) {
	s.mu.Lock()
	for i := range s.queues {
		if s.queues[i].name == q {
			s.queues[i].pending = append(s.queues[i].pending, d)
			break
		}
	}
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

// Smooth weighted round robin over runnable queues. Empty queues consume no
// slots and carry no credit into a later burst. Prefetch is independent of weight.
func (s *deliveryScheduler) take(ctx context.Context) (queuedDelivery, bool) {
	for {
		if ctx.Err() != nil {
			return queuedDelivery{}, false
		}
		s.mu.Lock()
		selected := -1
		total := 0
		for i := range s.queues {
			q := &s.queues[i]
			if len(q.pending) == 0 {
				q.current = 0
				continue
			}
			q.current += q.weight
			total += q.weight
			if selected < 0 || q.current > s.queues[selected].current {
				selected = i
			}
		}
		if selected >= 0 {
			q := &s.queues[selected]
			q.current -= total
			d := q.pending[0]
			q.pending[0] = amqp.Delivery{}
			q.pending = q.pending[1:]
			s.mu.Unlock()
			return queuedDelivery{q.name, d}, true
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return queuedDelivery{}, false
		case <-changed:
		}
	}
}
