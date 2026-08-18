package outbox

import "testing"

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
