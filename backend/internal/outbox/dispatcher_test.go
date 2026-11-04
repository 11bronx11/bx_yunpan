package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

type recordingPublisher struct {
	calls   int
	taskIDs []string
}

func (f *recordingPublisher) Publish(_ context.Context, task taskqueue.Task) error {
	f.calls++
	f.taskIDs = append(f.taskIDs, task.ID)
	return nil
}

func TestFanoutRespectsAIEnabled(t *testing.T) {
	enabled := fanout(EventObjectReady, true)
	if len(enabled) != 2 || enabled[0].queue != "media" || enabled[1].queue != "ai" {
		t.Fatalf("enabled fanout = %#v", enabled)
	}

	disabled := fanout(EventObjectReady, false)
	if len(disabled) != 1 || disabled[0].queue != "media" {
		t.Fatalf("disabled fanout = %#v", disabled)
	}

	if targets := fanout(EventAIReprocessRequested, false); len(targets) != 0 {
		t.Fatalf("disabled reprocess fanout = %#v", targets)
	}
}

func TestPayloadWithTraceCarriesStableTaskMetadata(t *testing.T) {
	version := int64(7)
	event := Event{
		ID: uuid.New(), EventType: EventObjectReady, EventVersion: 1,
		AggregateVersion: &version, Payload: []byte(`{"object_id":"00000000-0000-0000-0000-000000000001"}`),
		CreatedAt: time.Now().UTC(),
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadWithTrace(event, nil), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["event_id"] != event.ID.String() || payload["task_id"] != event.ID.String() || payload["event_type"] != EventObjectReady || payload["aggregate_version"] != float64(7) {
		t.Fatalf("task metadata = %#v", payload)
	}
}

func TestWaitForPollInterruptsForNotificationCancellationAndLeadership(t *testing.T) {
	t.Run("notification", func(t *testing.T) {
		wakeups := make(chan struct{}, 1)
		wakeups <- struct{}{}
		if got := waitForPoll(context.Background(), nil, wakeups, time.Hour); got != pollWaitNotified {
			t.Fatalf("wait result = %v, want notification", got)
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if got := waitForPoll(ctx, nil, nil, time.Hour); got != pollWaitCanceled {
			t.Fatalf("wait result = %v, want cancellation", got)
		}
	})

	t.Run("leadership loss", func(t *testing.T) {
		lost := make(chan struct{})
		close(lost)
		if got := waitForPoll(context.Background(), lost, nil, time.Hour); got != pollWaitLeadershipLost {
			t.Fatalf("wait result = %v, want leadership loss", got)
		}
	})

	t.Run("closed notification channel", func(t *testing.T) {
		wakeups := make(chan struct{})
		close(wakeups)
		if got := waitForPoll(context.Background(), nil, wakeups, time.Hour); got != pollWaitChannelClosed {
			t.Fatalf("wait result = %v, want closed channel", got)
		}
	})
}

func TestPollBackoffIsBounded(t *testing.T) {
	minimum, maximum := time.Second, 30*time.Second
	got := []time.Duration{minimum}
	for range 8 {
		got = append(got, nextPollInterval(got[len(got)-1], minimum, maximum))
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	if len(got) != len(want) {
		t.Fatalf("backoff lengths differ: got=%v want=%v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("backoff[%d] = %s, want %s", i, got[i], want[i])
		}
	}
	if jitter := jitterDown(maximum, minimum); jitter < maximum*4/5 || jitter > maximum {
		t.Fatalf("downward jitter = %s, want within [%s,%s]", jitter, maximum*4/5, maximum)
	}
	if jitter := jitterDown(minimum, minimum); jitter != minimum {
		t.Fatalf("minimum backoff jitter = %s, want %s", jitter, minimum)
	}
}

func TestPollBoundsUseConfiguredFallbackAndClampInvalidDirectConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  config.Outbox
		minimum time.Duration
		maximum time.Duration
	}{
		{name: "configured", config: config.Outbox{PollInterval: 200 * time.Millisecond, MaxPollInterval: 4 * time.Second}, minimum: 200 * time.Millisecond, maximum: 4 * time.Second},
		{name: "default minimum", config: config.Outbox{}, minimum: time.Second, maximum: time.Second},
		{name: "maximum below minimum", config: config.Outbox{PollInterval: 2 * time.Second, MaxPollInterval: time.Second}, minimum: 2 * time.Second, maximum: 2 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			minimum, maximum := newDispatcher(nil, nil, nil, test.config).pollBounds()
			if minimum != test.minimum || maximum != test.maximum {
				t.Fatalf("poll bounds = (%s, %s), want (%s, %s)", minimum, maximum, test.minimum, test.maximum)
			}
		})
	}
}

func TestDispatcherRecoversWhenEnqueueCommitsBeforeDeliveryStatus(t *testing.T) {
	db := isolatedOutboxDB(t)
	eventID, aggregateID := uuid.New(), uuid.New()
	envelope, err := eventcontract.New(eventcontract.EnvelopeInput{
		EventID: eventID, EventType: eventcontract.EventObjectVerifyRequestedV1,
		AggregateID: aggregateID, AggregateVersion: 1, Producer: "relay-window-test",
		Payload: eventcontract.ObjectVerifyRequestedV1{UploadID: aggregateID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return AddEnvelope(tx, envelope) }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Delete(&Event{}, "id = ?", eventID).Error })

	fake := &recordingPublisher{}
	dispatcher := newDispatcher(db, fake, slog.Default(), config.Outbox{BatchSize: 1, MaxRetry: 2, TaskTimeout: time.Minute})
	injected := errors.New("injected delivery status failure")
	dispatcher.markPublished = func(*gorm.DB, Delivery, time.Time) error { return injected }
	if err := dispatcher.dispatchBatch(context.Background()); !errors.Is(err, injected) {
		t.Fatalf("first dispatch error = %v", err)
	}
	var delivery Delivery
	if err := db.First(&delivery, "event_id = ?", eventID).Error; err != nil {
		t.Fatal(err)
	}
	if delivery.Status != "pending" {
		t.Fatalf("rolled-back delivery status = %q", delivery.Status)
	}

	dispatcher.markPublished = markDeliveryPublished
	if err := dispatcher.dispatchBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 2 || len(fake.taskIDs) != 2 || fake.taskIDs[0] == "" || fake.taskIDs[0] != fake.taskIDs[1] {
		t.Fatalf("enqueue calls=%d task_ids=%v", fake.calls, fake.taskIDs)
	}
	if err := db.First(&delivery, "event_id = ?", eventID).Error; err != nil {
		t.Fatal(err)
	}
	if delivery.Status != "published" {
		t.Fatalf("recovered delivery status = %q", delivery.Status)
	}
}
