package asynccontrol

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// The v2 topology is intentionally disjoint from all legacy work/TTL queues.
// RabbitMQ transports notifications; it owns no business retry timer.
type Broker struct {
	URL      string
	Prefix   string
	Timeout  time.Duration
	gate     chan struct{}
	conn     *amqp.Connection
	ch       *amqp.Channel
	socket   net.Conn
	confirms <-chan amqp.Confirmation
	returns  <-chan amqp.Return
	once     sync.Once
	closed   chan struct{}
}

func NewBroker(url string, prefix ...string) *Broker {
	p := "bx.async.v2"
	if len(prefix) > 0 && prefix[0] != "" {
		p = prefix[0]
	}
	return &Broker{URL: url, Prefix: p, Timeout: 5 * time.Second, gate: make(chan struct{}, 1), closed: make(chan struct{})}
}
func (b *Broker) WorkQueue(queue string) string { return b.Prefix + ".work." + queue }
func (b *Broker) dial(ctx context.Context) (*amqp.Connection, net.Conn, error) {
	var socket net.Conn
	conn, err := amqp.DialConfig(b.URL, amqp.Config{Heartbeat: 10 * time.Second, Locale: "en_US", Dial: func(network, address string) (net.Conn, error) {
		d := net.Dialer{Timeout: b.Timeout}
		c, err := d.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		socket = c
		_ = c.SetDeadline(time.Now().Add(b.Timeout))
		return c, nil
	}})
	if err != nil {
		if socket != nil {
			_ = socket.Close()
		}
		return nil, nil, errors.New("async RabbitMQ connection failed")
	}
	_ = socket.SetDeadline(time.Time{})
	return conn, socket, nil
}

func (b *Broker) declare(ch *amqp.Channel) error {
	if err := ch.ExchangeDeclare(b.Prefix, "direct", true, false, false, false, nil); err != nil {
		return err
	}
	dead := b.Prefix + ".dead"
	if _, err := ch.QueueDeclare(dead, true, false, false, false, amqp.Table{"x-queue-type": "quorum", "x-delivery-limit": int32(20)}); err != nil {
		return err
	}
	if err := ch.QueueBind(dead, dead, b.Prefix, false, nil); err != nil {
		return err
	}
	declare := func(name string) error {
		args := amqp.Table{"x-queue-type": "quorum", "x-delivery-limit": int32(20), "x-dead-letter-exchange": b.Prefix, "x-dead-letter-routing-key": dead, "x-dead-letter-strategy": "at-least-once", "x-overflow": "reject-publish", "x-max-length": int64(10000)}
		if _, err := ch.QueueDeclare(name, true, false, false, false, args); err != nil {
			return err
		}
		return ch.QueueBind(name, name, b.Prefix, false, nil)
	}
	for _, q := range WorkQueues() {
		if err := declare(b.WorkQueue(q)); err != nil {
			return err
		}
	}

	return nil
}

func (b *Broker) reset() {
	if b.socket != nil {
		_ = b.socket.Close()
	}
	b.conn = nil
	b.ch = nil
	b.socket = nil
}

func (b *Broker) publisher(ctx context.Context) error {
	select {
	case <-b.closed:
		return errors.New("broker closed")
	default:
	}
	if b.conn != nil && !b.conn.IsClosed() && b.ch != nil && !b.ch.IsClosed() {
		return nil
	}
	b.reset()
	conn, socket, err := b.dial(ctx)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = socket.Close() })
	defer stop()
	ch, err := conn.Channel()
	if err == nil {
		err = b.declare(ch)
	}
	if err == nil {
		err = ch.Confirm(false)
	}
	if err != nil {
		_ = socket.Close()
		return err
	}
	b.conn = conn
	b.socket = socket
	b.ch = ch
	b.confirms = ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	b.returns = ch.NotifyReturn(make(chan amqp.Return, 1))
	return nil
}

func (b *Broker) Publish(ctx context.Context, queue string, n Notice) (err error) {
	ctx, cancel := context.WithTimeout(ctx, b.Timeout)
	defer cancel()
	select {
	case b.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-b.closed:
		return errors.New("broker closed")
	}
	defer func() { <-b.gate }()
	if err = b.publisher(ctx); err != nil {
		return err
	}
	socket := b.socket
	stop := context.AfterFunc(ctx, func() { _ = socket.Close() })
	defer func() {
		if !stop() || err != nil {
			b.reset()
		}
	}()
	body, _ := json.Marshal(n)
	if err = b.ch.PublishWithContext(ctx, b.Prefix, queue, true, false, amqp.Publishing{ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageId: n.TaskID.String(), Body: body}); err != nil {
		return err
	}
	select {
	case c, ok := <-b.confirms:
		if !ok || !c.Ack {
			return errors.New("async publish not confirmed")
		}
		select {
		case <-b.returns:
			return errors.New("async publish returned")
		default:
			return nil
		}
	case <-b.returns:
		return errors.New("async publish unroutable")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Broker) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, b.Timeout)
	defer cancel()
	select {
	case b.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-b.closed:
		return errors.New("broker closed")
	}
	defer func() { <-b.gate }()
	return b.publisher(ctx)
}

func (b *Broker) Close() {
	b.once.Do(func() { close(b.closed); b.gate <- struct{}{}; b.reset(); <-b.gate })
}

type Delivery struct {
	Notice Notice
	Raw    []byte
	ack    func() error
}

func (d Delivery) Ack() error { return d.ack() }

// ConsumeOnce closes its connection on an unpersisted delivery. The caller
// reconnects with infrastructure backoff; there is no hot nack/requeue loop.
func (b *Broker) ConsumeOnce(ctx context.Context, queue string, concurrency int, handler func(context.Context, Delivery) error) error {
	conn, socket, err := b.dial(ctx)
	if err != nil {
		return err
	}
	defer socket.Close()
	stop := context.AfterFunc(ctx, func() { _ = socket.Close() })
	defer stop()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	if err = b.declare(ch); err != nil {
		return err
	}
	if err = ch.Qos(concurrency, 0, false); err != nil {
		return err
	}
	deliveries, err := ch.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 1)
	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-run.Done():
					return
				case message, ok := <-deliveries:
					if !ok {
						select {
						case errs <- errors.New("async consumer channel closed"):
						default:
						}
						return
					}
					d := Delivery{Raw: message.Body, ack: func() error { return message.Ack(false) }}
					if len(message.Body) <= 1<<20 {
						_ = json.Unmarshal(message.Body, &d.Notice)
					}
					if err := handler(run, d); err != nil {
						select {
						case errs <- err:
						default:
						}
						cancel()
						return
					}
				}
			}
		}()
	}
	select {
	case err = <-errs:
	case <-ctx.Done():
		err = ctx.Err()
	}
	cancel()
	_ = socket.Close()
	wg.Wait()
	return err
}
