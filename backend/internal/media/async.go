package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/storageops"
)

var ErrInvalidResult = errors.New("invalid or incompatible thumbnail result")

// ComputeThumbnail has no business connection and never publishes metadata.
func ComputeThumbnail(ctx context.Context, storage *minio.Client, in asyncprotocol.Input, key string) (asyncprotocol.Output, error) {
	out := asyncprotocol.Output{Noop: true}
	if !strings.HasPrefix(in.Mime, "image/") {
		return out, nil
	}
	reader, err := storage.GetObject(ctx, in.Bucket, in.Key, minio.GetObjectOptions{})
	if err != nil {
		return out, err
	}
	defer reader.Close()
	tracked := &imageReader{Reader: reader}
	cfg, _, err := image.DecodeConfig(tracked)
	if tracked.failure != nil {
		return out, tracked.failure
	}
	if errors.Is(err, image.ErrFormat) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if !validSourceDimensions(cfg.Width, cfg.Height) {
		return out, nil
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return out, err
	}
	source, _, err := image.Decode(tracked)
	if tracked.failure != nil {
		return out, tracked.failure
	}
	if err != nil {
		return out, err
	}
	thumb := fit(source, 512, 512)
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, thumb, &jpeg.Options{Quality: 82}); err != nil {
		return out, err
	}
	_, err = storage.PutObject(ctx, in.Bucket, key, bytes.NewReader(encoded.Bytes()), int64(encoded.Len()), minio.PutObjectOptions{ContentType: "image/jpeg"})
	if err != nil {
		return out, err
	}
	return asyncprotocol.Output{Bucket: in.Bucket, Key: key, Mime: "image/jpeg", Width: thumb.Bounds().Dx(), Height: thumb.Bounds().Dy()}, nil
}

func (s *Service) CommitAsync(tx *gorm.DB, in asyncprotocol.Input, out asyncprotocol.Output) (asyncprotocol.Decision, error) {
	d := asyncprotocol.Decision{Outcome: "succeeded"}
	if out.Noop {
		return d, nil
	}
	if out.Bucket != in.Bucket || !strings.HasPrefix(out.Key, "variants/compute/") || out.Width < 1 || out.Height < 1 || out.Width > 512 || out.Height > 512 {
		return d, ErrInvalidResult
	}
	var object objectstore.Object
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&object, "id=?", in.ObjectID).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return d, err
	}
	valid := err == nil && object.Status == "ready" && object.ContentGeneration == in.ContentGeneration && object.ObjectKey == in.Key
	if valid {
		variant := Variant{ID: uuid.NewSHA1(in.ObjectID, []byte(out.Key)), ObjectID: in.ObjectID, VariantType: "thumbnail", ObjectKey: out.Key, MimeType: out.Mime, Width: out.Width, Height: out.Height, PipelineVersion: thumbnailPipeline, Status: "ready"}
		q := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&variant)
		if q.Error != nil {
			return d, q.Error
		}
		if q.RowsAffected == 1 {
			return d, nil
		}
		var existing Variant
		if err := tx.Where("object_id=? AND variant_type='thumbnail' AND pipeline_version=?", in.ObjectID, thumbnailPipeline).First(&existing).Error; err != nil {
			return d, err
		}
		if existing.Status != "ready" || existing.MimeType != out.Mime || existing.Width != out.Width || existing.Height != out.Height {
			return d, ErrInvalidResult
		}
		if existing.ObjectKey == out.Key {
			return d, nil
		}
		// Object GC removes old-generation metadata before revival. A conflicting
		// current result remains the winner and this attempt's bytes are cleaned.
	} else {
		d.Outcome = "superseded"
	}
	return d, cleanupVariant(tx, in.ObjectID, out.Bucket, out.Key)
}

func cleanupVariant(tx *gorm.DB, id uuid.UUID, bucket, key string) error {
	hash := sha256.Sum256([]byte(bucket + ":" + key))
	_, err := storageops.Enqueue(tx, storageops.Input{OperationType: storageops.VariantDelete, IdempotencyKey: fmt.Sprintf("variant.delete:%x", hash), ObjectID: &id, SourceBucket: bucket, SourceKey: key})
	return err
}
