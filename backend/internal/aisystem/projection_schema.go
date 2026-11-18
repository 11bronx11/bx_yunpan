package aisystem

import (
	"context"
	"errors"
	"strings"
)

func (p *Projector) writeTextChild(ctx context.Context, endpoint string, document any) error {
	var response struct {
		Result string `json:"result"`
		Shards *struct {
			Failed *int `json:"failed"`
		} `json:"_shards"`
	}
	if err := p.request(ctx, "PUT", endpoint, document, "es", &response); err != nil {
		return err
	}
	if (response.Result != "created" && response.Result != "updated") || response.Shards == nil || response.Shards.Failed == nil || *response.Shards.Failed != 0 {
		return errors.New("projection.write_incomplete")
	}
	return nil
}

func (p *Projector) writeVectorPoints(ctx context.Context, endpoint string, points []any) error {
	var response struct {
		Status string `json:"status"`
		Result struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	if err := p.request(ctx, "PUT", endpoint, map[string]any{"points": points}, "qd", &response); err != nil {
		return err
	}
	if response.Status != "ok" || response.Result.Status != "completed" {
		return errors.New("projection.write_incomplete")
	}
	return nil
}

func (p *Projector) ensureVectorCollection(ctx context.Context, target string) error {
	endpoint := strings.TrimRight(p.QD, "/") + "/collections/" + target
	var created struct {
		Status string `json:"status"`
		Result bool   `json:"result"`
	}
	err := p.request(ctx, "PUT", endpoint, map[string]any{"vectors": map[string]any{"size": 1024, "distance": "Cosine"}}, "qd", &created)
	if err == nil && created.Status == "ok" && created.Result {
		return nil
	}
	// A failed create is idempotent only when the existing collection has the
	// exact unnamed-vector geometry expected by these published embeddings.
	var existing struct {
		Status string `json:"status"`
		Result struct {
			Config struct {
				Params struct {
					Vectors struct {
						Size     int
						Distance string
					}
				}
			}
		}
	}
	if err := p.request(ctx, "GET", endpoint, nil, "qd", &existing); err != nil {
		return err
	}
	vectors := existing.Result.Config.Params.Vectors
	if existing.Status != "ok" || vectors.Size != 1024 || vectors.Distance != "Cosine" {
		return errors.New("projection.incompatible_collection")
	}
	return nil
}

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
