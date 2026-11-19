package aigateway

import (
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
)

func TestSelectedAskGrantOutlivesGenerationBudget(t *testing.T) {
	h := &HTTP{AskTimeout: time.Minute}
	for _, mode := range []string{"all", "folder", "selected"} {
		t.Run(mode, func(t *testing.T) {
			scope := aiprotocol.Scope{Mode: mode}
			wantTimeout, wantTTL := time.Minute, 2*time.Minute
			if mode == "selected" {
				wantTimeout, wantTTL = 5*time.Minute, 6*time.Minute
			}
			if got := h.askTimeout(scope); got != wantTimeout {
				t.Fatalf("timeout=%s, want %s", got, wantTimeout)
			}
			if got := h.scopeGrantTTL(scope); got != wantTTL || got <= wantTimeout {
				t.Fatalf("grant lifetime=%s, want %s after timeout %s", got, wantTTL, wantTimeout)
			}
		})
	}
}
