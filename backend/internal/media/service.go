package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/minio/minio-go/v7"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/storageurl"
)

const (
	thumbnailPipeline = "thumbnail-v1"
	maxSourcePixels   = 40_000_000
)

type Variant struct {
	ID              uuid.UUID `gorm:"type:uuid;primaryKey"`
	ObjectID        uuid.UUID `gorm:"type:uuid"`
	VariantType     string
	ObjectKey       string
	MimeType        string
	Width           int
	Height          int
	PipelineVersion string
	Status          string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (Variant) TableName() string { return "object_variants" }

type Service struct {
	db         *gorm.DB
	objects    *objectstore.Service
	storage    *minio.Client
	presigner  *storageurl.Presigner
	bucket     string
	readURLTTL time.Duration
}

func NewService(db *gorm.DB, objects *objectstore.Service, storage *minio.Client, presigner *storageurl.Presigner, bucket string, readURLTTL time.Duration) *Service {
	return &Service{db: db, objects: objects, storage: storage, presigner: presigner, bucket: bucket, readURLTTL: readURLTTL}
}

func (s *Service) Process(ctx context.Context, objectID uuid.UUID) error {
	object, err := s.objects.Get(objectID)
	if err != nil || !strings.HasPrefix(object.MimeType, "image/") {
		return nil
	}
	var existing Variant
	if err := s.db.Where("object_id = ? AND variant_type = 'thumbnail' AND pipeline_version = ? AND status = 'ready'", objectID, thumbnailPipeline).First(&existing).Error; err == nil {
		return nil
	}
	reader, err := s.storage.GetObject(ctx, object.Bucket, object.ObjectKey, minio.GetObjectOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	imageConfig, _, err := image.DecodeConfig(reader)
	if err != nil || !validSourceDimensions(imageConfig.Width, imageConfig.Height) {
		return nil
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return err
	}
	source, _, err := image.Decode(reader)
	if err != nil {
		return nil
	}
	thumbnail := fit(source, 512, 512)
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, thumbnail, &jpeg.Options{Quality: 82}); err != nil {
		return err
	}
	key := fmt.Sprintf("variants/%s/thumbnail/%s.jpg", objectID, thumbnailPipeline)
	if _, err := s.storage.PutObject(ctx, s.bucket, key, bytes.NewReader(encoded.Bytes()), int64(encoded.Len()), minio.PutObjectOptions{ContentType: "image/jpeg"}); err != nil {
		return err
	}
	bounds := thumbnail.Bounds()
	variant := Variant{
		ID: uuid.Must(uuid.NewV7()), ObjectID: objectID, VariantType: "thumbnail", ObjectKey: key,
		MimeType: "image/jpeg", Width: bounds.Dx(), Height: bounds.Dy(), PipelineVersion: thumbnailPipeline, Status: "ready",
	}
	return s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&variant).Error
}

func validSourceDimensions(width, height int) bool {
	return width > 0 && height > 0 && int64(width)*int64(height) <= maxSourcePixels
}

func fit(source image.Image, maxWidth, maxHeight int) *image.RGBA {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	scale := 1.0
	if width > maxWidth || height > maxHeight {
		widthScale := float64(maxWidth) / float64(width)
		heightScale := float64(maxHeight) / float64(height)
		if widthScale < heightScale {
			scale = widthScale
		} else {
			scale = heightScale
		}
	}
	targetWidth, targetHeight := maxInt(1, int(float64(width)*scale)), maxInt(1, int(float64(height)*scale))
	result := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	for y := 0; y < targetHeight; y++ {
		for x := 0; x < targetWidth; x++ {
			sourceX := bounds.Min.X + x*width/targetWidth
			sourceY := bounds.Min.Y + y*height/targetHeight
			result.Set(x, y, source.At(sourceX, sourceY))
		}
	}
	return result
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func (s *Service) PreviewURL(ctx context.Context, objectID uuid.UUID) (string, time.Time, bool, error) {
	var variant Variant
	if err := s.db.Where("object_id = ? AND variant_type = 'thumbnail' AND status = 'ready'", objectID).Order("created_at DESC").First(&variant).Error; err != nil {
		return "", time.Time{}, false, nil
	}
	expiresAt := time.Now().UTC().Add(s.readURLTTL)
	params := url.Values{"response-content-type": {"image/jpeg"}, "response-content-disposition": {"inline"}}
	value, err := s.presigner.PresignedGetObject(ctx, s.bucket, variant.ObjectKey, s.readURLTTL, params)
	if err != nil {
		return "", time.Time{}, false, err
	}
	return value.String(), expiresAt, true, nil
}

func Handler(service *Service) asynq.HandlerFunc {
	return func(ctx context.Context, task *asynq.Task) error {
		var payload struct {
			ObjectID uuid.UUID `json:"object_id"`
		}
		if json.Unmarshal(task.Payload(), &payload) != nil {
			return asynq.SkipRetry
		}
		return service.Process(ctx, payload.ObjectID)
	}
}
