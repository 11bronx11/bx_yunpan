package outbox

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/lib/pq"
)

const deliveryWakeupChannel = "bx_outbox_delivery_ready"

// PostgresWakeupListener coalesces PostgreSQL notifications into an in-process
// signal. Closing it releases its dedicated LISTEN connection.
type PostgresWakeupListener struct {
	listener      *pq.Listener
	notifications chan struct{}
	closeOnce     sync.Once
	closeErr      error
}

func NewPostgresWakeupListener(ctx context.Context, dsn string, logger *slog.Logger) *PostgresWakeupListener {
	if dsn == "" {
		return nil
	}
	listener := pq.NewListener(dsn, time.Second, 30*time.Second, func(event pq.ListenerEventType, err error) {
		if logger == nil {
			return
		}
		switch event {
		case pq.ListenerEventConnected:
			logger.InfoContext(ctx, "outbox PostgreSQL notification listener connected")
		case pq.ListenerEventReconnected:
			logger.InfoContext(ctx, "outbox PostgreSQL notification listener reconnected")
		case pq.ListenerEventDisconnected, pq.ListenerEventConnectionAttemptFailed:
			logger.WarnContext(ctx, "outbox PostgreSQL notification listener unavailable", "error", err)
		}
	})
	w := &PostgresWakeupListener{listener: listener, notifications: make(chan struct{}, 1)}
	go func() {
		if err := listener.Listen(deliveryWakeupChannel); err != nil && ctx.Err() == nil && logger != nil {
			logger.WarnContext(ctx, "listen for outbox delivery notifications", "error", err)
		}
	}()
	go func() {
		defer close(w.notifications)
		for {
			select {
			case _, ok := <-listener.NotificationChannel():
				if !ok {
					return
				}
				select {
				case w.notifications <- struct{}{}:
				default:
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return w
}

func (w *PostgresWakeupListener) Notifications() <-chan struct{} {
	if w == nil {
		return nil
	}
	return w.notifications
}

func (w *PostgresWakeupListener) Close() error {
	if w == nil {
		return nil
	}
	w.closeOnce.Do(func() { w.closeErr = w.listener.Close() })
	return w.closeErr
}
