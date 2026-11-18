package aigateway

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestQuotaAndScopeBudgetRemainDistinctHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"quota", ai.GRPCStatus(ai.ErrQuota).Err(), 429, "ai.quota_exhausted"},
		{"scope", status.Error(codes.ResourceExhausted, "ai.scope_too_large"), 422, "ai.scope_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writer := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(writer)
			(&HTTP{}).fail(ctx, tc.err)
			var body struct{ Error struct{ Code string } }
			if err := json.Unmarshal(writer.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if writer.Code != tc.status || body.Error.Code != tc.code {
				t.Fatalf("got HTTP %d %s, want %d %s", writer.Code, writer.Body, tc.status, tc.code)
			}
		})
	}
}
