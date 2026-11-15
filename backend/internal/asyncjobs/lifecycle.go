package asyncjobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
)

// EnsureLifecycle adds only the managed staging rule, preserving other rules.
// Canonical and thumbnail output prefixes are intentionally never expired:
// they can become live business objects. Their abandoned generations have
// durable, targeted cleanup records instead.
func EnsureLifecycle(ctx context.Context, storage *minio.Client, bucket string, days int, recoveryWindow time.Duration) error {
	if days < 1 || time.Duration(days)*24*time.Hour <= recoveryWindow {
		return errors.New("staging lifecycle must exceed upload and recovery windows")
	}
	cfg, err := storage.GetBucketLifecycle(ctx, bucket)
	if err != nil && minio.ToErrorResponse(err).Code != "NoSuchLifecycleConfiguration" {
		return err
	}
	if cfg == nil {
		cfg = lifecycle.NewConfiguration()
	}
	rule := lifecycle.Rule{ID: "bx-async-v1-staging", Status: "Enabled", RuleFilter: lifecycle.Filter{Prefix: "staging/"}, Expiration: lifecycle.Expiration{Days: lifecycle.ExpirationDays(days)}, AbortIncompleteMultipartUpload: lifecycle.AbortIncompleteMultipartUpload{DaysAfterInitiation: lifecycle.ExpirationDays(days)}}
	for _, r := range cfg.Rules {
		if r.ID == rule.ID {
			if r.Status != rule.Status || r.RuleFilter.Prefix != "staging/" || r.Expiration.Days != rule.Expiration.Days {
				return fmt.Errorf("managed staging lifecycle differs; review rule %s", rule.ID)
			}
			return nil
		}
	}
	cfg.Rules = append(cfg.Rules, rule)
	if err := storage.SetBucketLifecycle(ctx, bucket, cfg); err != nil {
		return fmt.Errorf("install staging lifecycle: %w", err)
	}
	verified, err := storage.GetBucketLifecycle(ctx, bucket)
	if err != nil {
		return err
	}
	for _, r := range verified.Rules {
		if r.ID == rule.ID && r.Expiration.Days == rule.Expiration.Days {
			if r.AbortIncompleteMultipartUpload.DaysAfterInitiation == 0 {
				slog.Warn("storage omits multipart lifecycle; using directed operation sweep and MinIO stale-upload policy")
			}
			return nil
		}
	}
	return errors.New("storage did not preserve managed multipart lifecycle")
}
