package eventstream

import (
	"context"
	"testing"
)

type fakeWriter struct{ records []Record }

func (w *fakeWriter) Write(_ context.Context, record Record) error {
	w.records = append(w.records, record)
	return nil
}
func (*fakeWriter) Ping(context.Context) error { return nil }

func TestKafkaProducerPublishesStableRecord(t *testing.T) {
	w := &fakeWriter{}
	p := NewKafkaProducer(w)
	record := Record{Topic: "yunpan.file-events", Key: "file-1", EventID: "event-1", EventType: "file.updated.v1", Payload: []byte(`{}`)}
	if err := p.Publish(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if len(w.records) != 1 || w.records[0].EventID != record.EventID || w.records[0].Key != record.Key {
		t.Fatalf("records = %#v", w.records)
	}
	p.Close()
	if err := p.Publish(context.Background(), record); err == nil {
		t.Fatal("publish after close unexpectedly succeeded")
	}
}

func TestKafkaProducerRejectsIncompleteRecord(t *testing.T) {
	if err := NewKafkaProducer(&fakeWriter{}).Publish(context.Background(), Record{Topic: "topic"}); err != ErrInvalidRecord {
		t.Fatalf("error = %v", err)
	}
}

func TestTopicForEventUsesDomainTopics(t *testing.T) {
	checks := map[string]string{
		"file.updated.v1":       "yunpan.file-events",
		"folder.moved.v1":       "yunpan.file-events",
		"share.revoked.v1":      "yunpan.share-events",
		"object.ready.v1":       "yunpan.object-events",
		"ai.index_requested.v1": "yunpan.ai-events",
	}
	for event, want := range checks {
		if got := TopicForEvent(event); got != want {
			t.Fatalf("%s topic = %q, want %q", event, got, want)
		}
	}
	if got := TopicForEvent("unknown.v1"); got != "" {
		t.Fatalf("unknown topic = %q", got)
	}
}
