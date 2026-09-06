package eventstream

import (
	"context"
	"errors"
	"strings"
	"sync"
)

var ErrInvalidRecord = errors.New("eventstream: invalid record")

// Writer is intentionally tiny so the Kafka client remains outside the core
// domain package. A production adapter can wrap franz-go, Sarama, or a managed
// platform client without changing outbox semantics.
type Writer interface {
	Write(context.Context, Record) error
	Ping(context.Context) error
}

type KafkaProducer struct {
	writer Writer
	mu     sync.RWMutex
	closed bool
}

func NewKafkaProducer(writer Writer) *KafkaProducer { return &KafkaProducer{writer: writer} }

func (p *KafkaProducer) Publish(ctx context.Context, record Record) error {
	if p == nil || p.writer == nil || strings.TrimSpace(record.Topic) == "" || strings.TrimSpace(record.Key) == "" || strings.TrimSpace(record.EventID) == "" || len(record.Payload) == 0 {
		return ErrInvalidRecord
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return errors.New("eventstream: producer is closed")
	}
	return p.writer.Write(ctx, record)
}

func (p *KafkaProducer) Health(ctx context.Context) error {
	if p == nil || p.writer == nil {
		return ErrInvalidRecord
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return errors.New("eventstream: producer is closed")
	}
	return p.writer.Ping(ctx)
}

func (p *KafkaProducer) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
}
