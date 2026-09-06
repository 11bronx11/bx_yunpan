package outbox

import (
	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"testing"
)

func TestRoutesAreExplicitAndFanoutDestinationsAreIndependent(t *testing.T) {
	ready := Routes(EventObjectReady, true)
	if len(ready) != 2 {
		t.Fatalf("object.ready routes = %#v", ready)
	}
	if ready[0].Destination == ready[1].Destination {
		t.Fatalf("object.ready destinations collide: %#v", ready)
	}
	if ready[0].Queue != "media" || ready[1].Queue != "ai" {
		t.Fatalf("object.ready queues = %#v", ready)
	}
	if got := Routes("unknown.future.v1", true); got != nil {
		t.Fatalf("unknown route = %#v, want nil", got)
	}
	if got := fanout("unknown.future.v1", true); got != nil {
		t.Fatalf("unknown fanout = %#v, want nil", got)
	}
}

func TestRoutesOmitDisabledAIConsumers(t *testing.T) {
	if got := Routes(EventObjectReady, false); len(got) != 1 || got[0].Queue != "media" {
		t.Fatalf("AI-disabled object.ready routes = %#v", got)
	}
	if got := Routes(EventAIReprocessRequested, false); got != nil {
		t.Fatalf("AI-disabled reprocess routes = %#v", got)
	}
}

func TestTargetFromDeliveryRejectsMalformedDestinations(t *testing.T) {
	if _, ok := targetFromDelivery(Delivery{Transport: "kafka", Destination: "topic"}); ok {
		t.Fatal("kafka destination unexpectedly parsed as asynq")
	}
	if _, ok := targetFromDelivery(Delivery{Transport: "asynq", Destination: "missing-queue-separator"}); ok {
		t.Fatal("malformed destination unexpectedly parsed")
	}
}

func TestTargetAllowedForEventRejectsDestinationDrift(t *testing.T) {
	delivery := Delivery{Transport: "asynq", Destination: EventObjectReady + "@maintenance"}
	if _, ok := targetAllowedForEvent(EventObjectReady, true, delivery); ok {
		t.Fatal("destination drift unexpectedly accepted")
	}
	valid := Delivery{Transport: "asynq", Destination: EventObjectReady + "@media"}
	if _, ok := targetAllowedForEvent(EventObjectReady, true, valid); !ok {
		t.Fatal("registered destination rejected")
	}
}

func TestKafkaRoutesOnlyDomainEvents(t *testing.T) {
	if got := KafkaRoutes(eventcontract.EventFileUpdatedV1, true); len(got) != 1 || got[0].Transport != "kafka" || got[0].Destination != "yunpan.file-events" {
		t.Fatalf("file event Kafka route = %#v", got)
	}
	if got := KafkaRoutes(EventObjectVerifyRequested, true); got != nil {
		t.Fatalf("command Kafka route = %#v, want nil", got)
	}
	if got := KafkaRoutes(eventcontract.EventFileUpdatedV1, false); got != nil {
		t.Fatalf("disabled Kafka route = %#v, want nil", got)
	}
}
