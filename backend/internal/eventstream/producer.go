// Package eventstream defines the long-lived event transport boundary. The
// core transaction only writes PostgreSQL outbox rows; a producer is invoked
// later by a transport-specific relay.
package eventstream

import "context"

type Record struct {
	Topic     string
	Key       string
	EventID   string
	EventType string
	Payload   []byte
}

type Producer interface {
	Publish(context.Context, Record) error
	Health(context.Context) error
}

type NopProducer struct{}

func (NopProducer) Publish(context.Context, Record) error { return nil }
func (NopProducer) Health(context.Context) error          { return nil }
