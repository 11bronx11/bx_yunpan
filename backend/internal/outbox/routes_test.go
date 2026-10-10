package outbox

import (
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
		t.Fatal("retired transport unexpectedly accepted")
	}
	if _, ok := targetFromDelivery(Delivery{Transport: "rabbitmq", Destination: "missing-queue-separator"}); ok {
		t.Fatal("malformed destination unexpectedly parsed")
	}
}

func TestTargetAllowedForEventRejectsDestinationDrift(t *testing.T) {
	delivery := Delivery{Transport: "rabbitmq", Destination: EventObjectReady + "@maintenance"}
	if _, ok := targetAllowedForEvent(EventObjectReady, true, delivery); ok {
		t.Fatal("destination drift unexpectedly accepted")
	}
	valid := Delivery{Transport: "rabbitmq", Destination: EventObjectReady + "@media"}
	if _, ok := targetAllowedForEvent(EventObjectReady, true, valid); !ok {
		t.Fatal("registered destination rejected")
	}
}
