package aigateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pbv2"
)

func TestHTTPVersionAndSourceOffsetsRemainExactJSONIntegers(t *testing.T) {
	response := &pbv2.SearchResponse{Hits: []*pbv2.SearchHit{{Citations: []*pbv2.Citation{{ContentGeneration: 9007199254740993, SourceLocation: &pbv2.SourceLocation{SourceStart: 9007199254740995, RowStart: 81}}}}}}
	raw, e := json.Marshal(asMap(response))
	if e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{`"content_generation":9007199254740993`, `"source_start":9007199254740995`, `"row_start":81`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("HTTP number changed or lost precision: %s", raw)
		}
	}
}
