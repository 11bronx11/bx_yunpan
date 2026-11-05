// Package taskqueue implements durable, at-least-once RabbitMQ task delivery.
// Business handlers must remain idempotent: a process can die after committing
// its side effect and before acknowledging its delivery.
package taskqueue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
)

var SkipRetry = errors.New("taskqueue: skip retry")
var ErrClosed = errors.New("taskqueue: broker closed")

type Config struct {
	URL            string
	Exchange       string
	Prefetch       int
	Concurrency    int
	PublishTimeout time.Duration
	MaxRetry       int
	TaskTimeout    time.Duration
	StopTimeout    time.Duration
}

type Task struct {
	ID         string
	Kind       string
	Queue      string
	Body       []byte
	RetryCount int
	MaxRetry   int
	Timeout    time.Duration
	leaseToken uuid.UUID
}

func NewTask(kind string, body []byte) *Task { return &Task{Kind: kind, Body: body} }
func (t *Task) Type() string                 { return t.Kind }
func (t *Task) Payload() []byte              { return t.Body }

type Handler interface {
	ProcessTask(context.Context, *Task) error
}
type HandlerFunc func(context.Context, *Task) error

func (h HandlerFunc) ProcessTask(ctx context.Context, t *Task) error { return h(ctx, t) }

type MiddlewareFunc func(Handler) Handler
type Publisher interface {
	Publish(context.Context, Task) error
}

// FailureRecorder persists exhausted/poison tasks before the original is ACKed.
type FailureRecorder func(context.Context, Task, error) error

type publisher struct {
	conn     *amqp.Connection
	socket   net.Conn
	ch       *amqp.Channel
	confirms <-chan amqp.Confirmation
	returns  <-chan amqp.Return
}
type Broker struct {
	cfg             Config
	gate            chan struct{}
	pub             *publisher // protected by gate
	done            chan struct{}
	once            sync.Once
	FailureRecorder FailureRecorder
	ExecutionStore  *Store
}

var queueNames = []string{"object", "media", "maintenance", "upload", "ai", "search"}
var retryDelays = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, time.Minute, 5 * time.Minute}

func QueueNames() []string { return append([]string(nil), queueNames...) }
func validQueue(q string) bool {
	for _, name := range queueNames {
		if q == name {
			return true
		}
	}
	return false
}
func New(cfg Config) *Broker {
	if cfg.Exchange == "" {
		cfg.Exchange = "yunpan.tasks"
	}
	if cfg.Prefetch <= 0 {
		cfg.Prefetch = 16
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.PublishTimeout <= 0 {
		cfg.PublishTimeout = 5 * time.Second
	}
	if cfg.TaskTimeout <= 0 {
		cfg.TaskTimeout = 30 * time.Minute
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = 10 * time.Second
	}
	return &Broker{cfg: cfg, gate: make(chan struct{}, 1), done: make(chan struct{})}
}
func (b *Broker) Close() error {
	b.once.Do(func() { close(b.done) })
	b.gate <- struct{}{}
	defer func() { <-b.gate }()
	b.resetPublisher()
	return nil
}
func (b *Broker) resetPublisher() {
	if b.pub != nil {
		_ = b.pub.socket.Close()
		b.pub = nil
	}
}

// Closing the underlying socket also bounds AMQP handshake/RPC/write calls,
// whose library APIs do not themselves honor context cancellation.
func dial(ctx context.Context, url string) (*amqp.Connection, net.Conn, error) {
	var socket net.Conn
	conn, err := amqp.DialConfig(url, amqp.Config{Heartbeat: 10 * time.Second, Dial: func(network, addr string) (net.Conn, error) {
		c, e := (&net.Dialer{}).DialContext(ctx, network, addr)
		if e != nil {
			return nil, e
		}
		socket = c
		if d, ok := ctx.Deadline(); ok {
			_ = c.SetDeadline(d)
		}
		return c, nil
	}})
	if err != nil {
		if socket != nil {
			_ = socket.Close()
		}
		return nil, nil, fmt.Errorf("rabbitmq connect: %w", err)
	}
	if ctx.Err() != nil {
		_ = socket.Close()
		return nil, nil, ctx.Err()
	}
	return conn, socket, nil
}
func (b *Broker) openPublisher(ctx context.Context) error {
	if b.pub != nil && !b.pub.conn.IsClosed() && !b.pub.ch.IsClosed() {
		return nil
	}
	b.resetPublisher()
	conn, socket, err := dial(ctx, b.cfg.URL)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = socket.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { _ = socket.Close() })
	defer stop()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	if err = declareTopology(ch, b.cfg.Exchange); err != nil {
		return err
	}
	if err = ch.Confirm(false); err != nil {
		return err
	}
	b.pub = &publisher{conn: conn, socket: socket, ch: ch, confirms: ch.NotifyPublish(make(chan amqp.Confirmation, 1)), returns: ch.NotifyReturn(make(chan amqp.Return, 1))}
	_ = socket.SetDeadline(time.Time{})
	ok = true
	return nil
}
func declareTopology(ch *amqp.Channel, exchange string) error {
	if err := ch.ExchangeDeclare(exchange, "direct", true, false, false, false, nil); err != nil {
		return err
	}
	for _, q := range queueNames {
		// Unlimited broker redeliveries: process crashes do not silently exhaust
		// RabbitMQ 4's default delivery limit. Application retries are bounded below.
		if _, err := ch.QueueDeclare(q, true, false, false, false, amqp.Table{"x-queue-type": "quorum", "x-delivery-limit": int32(-1)}); err != nil {
			return err
		}
		if err := ch.QueueBind(q, q, exchange, false, nil); err != nil {
			return err
		}
		if _, err := ch.QueueDeclare(q+".dlq", true, false, false, false, amqp.Table{"x-queue-type": "quorum", "x-delivery-limit": int32(-1)}); err != nil {
			return err
		}
		if err := ch.QueueBind(q+".dlq", q+".dlq", exchange, false, nil); err != nil {
			return err
		}
		// Fixed TTL per queue avoids per-message TTL head-of-line delays. Quorum
		// at-least-once dead lettering retains retries while the target is unavailable.
		for _, delay := range retryDelays {
			name := retryQueue(q, delay)
			args := amqp.Table{"x-queue-type": "quorum", "x-message-ttl": delay.Milliseconds(), "x-dead-letter-exchange": exchange, "x-dead-letter-routing-key": q, "x-dead-letter-strategy": "at-least-once", "x-overflow": "reject-publish"}
			if _, err := ch.QueueDeclare(name, true, false, false, false, args); err != nil {
				return err
			}
			if err := ch.QueueBind(name, name, exchange, false, nil); err != nil {
				return err
			}
		}
	}
	return nil
}
func retryQueue(q string, d time.Duration) string {
	return q + ".retry." + strconv.FormatInt(d.Milliseconds(), 10)
}
func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > len(retryDelays) {
		attempt = len(retryDelays)
	}
	return retryDelays[attempt-1]
}
func (b *Broker) Publish(ctx context.Context, t Task) error {
	if t.ID == "" || t.Kind == "" || !validQueue(t.Queue) {
		return errors.New("taskqueue: id, type and known queue required")
	}
	if t.Timeout <= 0 {
		t.Timeout = b.cfg.TaskTimeout
	}
	if t.MaxRetry < 0 {
		t.MaxRetry = b.cfg.MaxRetry
	}
	return b.publish(ctx, t.Queue, t, "")
}
func (b *Broker) publish(ctx context.Context, key string, t Task, failure string) (err error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.PublishTimeout)
	defer cancel()
	select {
	case b.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-b.done:
		return ErrClosed
	}
	defer func() { <-b.gate }()
	select {
	case <-b.done:
		return ErrClosed
	default:
	}
	if err = b.openPublisher(ctx); err != nil {
		return err
	}
	p := b.pub
	stop := context.AfterFunc(ctx, func() { _ = p.socket.Close() })
	defer func() {
		if !stop() || err != nil {
			b.resetPublisher()
		}
	}()
	headers := amqp.Table{"x-retry-count": int32(t.RetryCount), "x-max-retry": int32(t.MaxRetry), "x-timeout-ms": t.Timeout.Milliseconds()}
	if failure != "" {
		headers["x-last-error"] = failure
	}
	msg := amqp.Publishing{ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageId: t.ID, Type: t.Kind, Body: t.Body, Headers: headers, Timestamp: time.Now().UTC()}
	if err = p.ch.PublishWithContext(ctx, b.cfg.Exchange, key, true, false, msg); err != nil {
		return err
	}
	select {
	case confirmation, ok := <-p.confirms:
		if !ok || !confirmation.Ack {
			return errors.New("rabbitmq confirm closed or nacked")
		}
		// AMQP sends basic.return before basic.ack. The library queues return
		// notifications before the confirm; drain it even when select chose ACK.
		select {
		case ret, ok := <-p.returns:
			if !ok {
				return errors.New("rabbitmq return channel closed")
			}
			return fmt.Errorf("rabbitmq returned message: %s", ret.ReplyText)
		default:
			return nil
		}
	case ret, ok := <-p.returns:
		if !ok {
			return errors.New("rabbitmq return channel closed")
		}
		return fmt.Errorf("rabbitmq returned message: %s", ret.ReplyText)
	case <-ctx.Done():
		return ctx.Err()
	case <-b.done:
		return ErrClosed
	}
}
func (b *Broker) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.PublishTimeout)
	defer cancel()
	conn, socket, err := dial(ctx, b.cfg.URL)
	if err != nil {
		return err
	}
	_ = conn
	return socket.Close()
}
func (b *Broker) Consume(ctx context.Context, queues map[string]int, handlers map[string]HandlerFunc, logger *slog.Logger) error {
	if len(queues) == 0 {
		return errors.New("taskqueue: no queues")
	}
	for q := range queues {
		if !validQueue(q) {
			return fmt.Errorf("unknown queue %q", q)
		}
	}
	if logger == nil {
		logger = slog.Default()
	}
	for {
		err := b.consumeOnce(ctx, queues, handlers, logger)
		select {
		case <-ctx.Done():
			return nil
		case <-b.done:
			return nil
		default:
		}
		logger.WarnContext(ctx, "rabbitmq consumer reconnecting", "error", err)
		select {
		case <-ctx.Done():
			return nil
		case <-b.done:
			return nil
		case <-time.After(time.Second):
		}
	}
}
func (b *Broker) consumeOnce(parent context.Context, queues map[string]int, handlers map[string]HandlerFunc, logger *slog.Logger) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	connectCtx, stopConnect := context.WithTimeout(ctx, b.cfg.PublishTimeout)
	conn, socket, err := dial(connectCtx, b.cfg.URL)
	if err != nil {
		stopConnect()
		return err
	}
	stopSetup := context.AfterFunc(connectCtx, func() { _ = socket.Close() })
	defer func() { stopSetup(); stopConnect(); _ = socket.Close() }()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	if err = declareTopology(ch, b.cfg.Exchange); err != nil {
		return err
	}
	scheduler := newDeliveryScheduler(queues)
	var wg sync.WaitGroup
	// Every exit cancels and joins handlers before bootstrap closes DB/storage.
	defer func() { cancel(); _ = socket.Close(); wg.Wait() }()
	errs := make(chan error, len(queues)+b.cfg.Concurrency)
	for q := range queues {
		channel, e := conn.Channel()
		if e != nil {
			return e
		}
		prefetch := b.cfg.Prefetch
		if e = channel.Qos(prefetch, 0, false); e != nil {
			return e
		}
		stream, e := channel.Consume(q, "", false, false, false, false, nil)
		if e != nil {
			return e
		}
		wg.Add(1)
		go func(q string, stream <-chan amqp.Delivery) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case d, ok := <-stream:
					if !ok {
						errs <- errors.New("rabbitmq consumer channel closed")
						return
					}
					scheduler.put(q, d)
				}
			}
		}(q, stream)
	}
	if !stopSetup() {
		return connectCtx.Err()
	}
	stopConnect()
	_ = socket.SetDeadline(time.Time{})
	for i := 0; i < b.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				job, ok := scheduler.take(ctx)
				if !ok {
					return
				}
				if e := b.handleDelivery(ctx, job.queue, job.value, handlers, logger); e != nil {
					errs <- e
					return
				}
			}
		}()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.done:
		return ErrClosed
	case e := <-errs:
		return e
	}
}
func invoke(ctx context.Context, t *Task, h HandlerFunc) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("task handler panic: %v", p)
		}
	}()
	if h == nil {
		return fmt.Errorf("unknown task type %q: %w", t.Kind, SkipRetry)
	}
	return h(ctx, t)
}
func (b *Broker) handleDelivery(ctx context.Context, q string, d amqp.Delivery, handlers map[string]HandlerFunc, logger *slog.Logger) error {
	t := Task{ID: d.MessageId, Kind: d.Type, Queue: q, Body: d.Body, MaxRetry: b.cfg.MaxRetry, Timeout: b.cfg.TaskTimeout}
	if n, ok := headerInt(d.Headers["x-retry-count"]); ok {
		t.RetryCount = n
	}
	if n, ok := headerInt(d.Headers["x-max-retry"]); ok {
		t.MaxRetry = n
	}
	if n, ok := headerInt(d.Headers["x-timeout-ms"]); ok && n > 0 {
		t.Timeout = time.Duration(n) * time.Millisecond
	}
	ctx = context.WithValue(ctx, deliveryContextKey{}, ctx)
	var err error
	if t.ID == "" || t.Kind == "" || t.RetryCount < 0 || t.MaxRetry < 0 {
		err = fmt.Errorf("invalid task envelope: %w", SkipRetry)
	} else if b.ExecutionStore != nil {
		handler := b.ExecutionStore.Middleware()(HandlerFunc(func(ctx context.Context, t *Task) error { return invoke(ctx, t, handlers[t.Kind]) }))
		err = handler.ProcessTask(ctx, &t)
	} else {
		taskCtx, cancel := context.WithTimeout(WithMetadata(ctx, t), t.Timeout)
		err = invokeBounded(taskCtx, &t, handlers[t.Kind], b.cfg.StopTimeout)
		if err == nil && taskCtx.Err() != nil {
			err = taskCtx.Err()
		}
		cancel()
	}
	if ctx.Err() != nil {
		return d.Nack(false, true)
	} // shutdown does not spend retries
	if err == nil {
		return d.Ack(false)
	}
	if errors.Is(err, ErrTaskStorage) || errors.Is(err, ErrLeaseLost) {
		_ = d.Nack(false, true)
		return err
	}
	if errors.Is(err, ErrTaskBusy) {
		if e := b.publish(ctx, retryQueue(q, retryDelays[0]), t, err.Error()); e != nil {
			_ = d.Nack(false, true)
			return e
		}
		return d.Ack(false)
	}
	if errors.Is(err, ErrTaskConflict) {
		if e := b.publish(ctx, q+".dlq", t, err.Error()); e != nil {
			_ = d.Nack(false, true)
			return e
		}
		return d.Ack(false)
	}
	if errors.Is(err, ErrTaskDead) {
		if e := b.publish(ctx, q+".dlq", t, err.Error()); e != nil {
			_ = d.Nack(false, true)
			return e
		}
		return d.Ack(false)
	}
	key := retryQueue(q, retryDelay(t.RetryCount+1))
	terminal := errors.Is(err, SkipRetry) || t.RetryCount >= t.MaxRetry
	if terminal {
		if b.FailureRecorder != nil {
			if e := b.FailureRecorder(ctx, t, err); e != nil {
				_ = d.Nack(false, true)
				return e
			}
		}
		key = q + ".dlq"
	} else {
		t.RetryCount++
	}
	if e := b.publish(ctx, key, t, err.Error()); e != nil {
		_ = d.Nack(false, true)
		return e
	}
	if terminal {
		logger.ErrorContext(ctx, "task exhausted; persisted in dead letter queue", "task_id", t.ID, "task_type", t.Kind, "error", err)
	}
	return d.Ack(false)
}
func headerInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	default:
		return 0, false
	}
}
