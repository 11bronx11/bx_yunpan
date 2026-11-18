package aisystem

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProjectionExistingMappingHasNoRequestBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			w.WriteHeader(400)
			return
		}
		b, _ := io.ReadAll(r.Body)
		if len(b) > 0 {
			t.Error("GET mapping sent a body")
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"test":{"mappings":{"properties":{"content_search":{"type":"text","analyzer":"whitespace"},"content_id":{"type":"keyword"},"child_id":{"type":"keyword"}}}}}`))
	}))
	defer server.Close()
	p := &Projector{ES: server.URL, HTTP: server.Client()}
	if e := p.ensureTextIndex(context.Background(), "test"); e != nil {
		t.Fatal(e)
	}
}
