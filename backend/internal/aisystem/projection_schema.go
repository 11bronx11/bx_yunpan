package aisystem

import (
	"context"
	"errors"
	"strings"
)

func (p *Projector) ensureTextIndex(ctx context.Context, target string) error {
	endpoint := strings.TrimRight(p.ES, "/") + "/" + target
	mapping := map[string]any{"properties": map[string]any{
		"content":            map[string]any{"type": "text"},
		"content_normalized": map[string]any{"type": "text"},
		"content_search":     map[string]any{"type": "text", "analyzer": "whitespace"},
		"lexical_version":    map[string]any{"type": "keyword"},
		"content_id":         map[string]any{"type": "keyword"},
		"child_id":           map[string]any{"type": "keyword"},
		"parent_id":          map[string]any{"type": "keyword"},
	}}
	if err := p.request(ctx, "PUT", endpoint, map[string]any{"mappings": mapping}, "es", nil); err != nil {
		// Only a verified compatible existing mapping makes create idempotent.
		var existing map[string]struct {
			Mappings struct {
				Properties map[string]struct{ Type, Analyzer string }
			}
		}
		if err := p.request(ctx, "GET", endpoint+"/_mapping", nil, "es", &existing); err != nil {
			return err
		}
		props := existing[target].Mappings.Properties
		if props["content_search"].Type != "text" || props["content_search"].Analyzer != "whitespace" || props["content_id"].Type != "keyword" || props["child_id"].Type != "keyword" {
			return errors.New("projection.incompatible_mapping")
		}
	}
	return nil
}
