package asyncjobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"
	"gorm.io/gorm"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/media"
)

type Computer struct {
	Control   *asynccontrol.Store
	Read      *gorm.DB
	Storage   *minio.Client
	Auxiliary func(context.Context, asynccontrol.Task, asyncprotocol.Input) error
}

func (c *Computer) Compute(ctx context.Context, t asynccontrol.Task) (asyncprotocol.Output, error) {
	var in asyncprotocol.Input
	var out asyncprotocol.Output
	if err := json.Unmarshal(t.Input, &in); err != nil {
		return out, asynccontrol.ErrInvalid
	}
	core := minio.Core{Client: c.Storage}
	switch t.Kind {
	case "multipart.create":
		// Multipart creation is not idempotent at S3. Recover only uploads belonging
		// to this operation's exclusive key; lifecycle catches late unknown creates.
		keyMarker, idMarker := "", ""
		for page := 0; page < 8; page++ {
			listed, err := core.ListMultipartUploads(ctx, in.Bucket, in.Key, keyMarker, idMarker, "", 128)
			if err != nil {
				return out, err
			}
			for _, u := range listed.Uploads {
				if u.Key == in.Key {
					out.UploadID = u.UploadID
					return out, nil
				}
			}
			if !listed.IsTruncated {
				break
			}
			keyMarker = listed.NextKeyMarker
			idMarker = listed.NextUploadIDMarker
			if page == 7 {
				return out, fmt.Errorf("%w: multipart recovery listing bound reached", asynccontrol.ErrInvalid)
			}
		}
		id, err := core.NewMultipartUpload(ctx, in.Bucket, in.Key, minio.PutObjectOptions{ContentType: in.Mime})
		if err != nil {
			return out, err
		}
		out.UploadID = id
		return out, nil
	case "multipart.complete":
		parts := make([]minio.CompletePart, 0, len(in.Parts))
		for _, p := range in.Parts {
			parts = append(parts, minio.CompletePart{PartNumber: p.Number, ETag: p.ETag})
		}
		_, err := core.CompleteMultipartUpload(ctx, in.Bucket, in.Key, in.UploadID, parts, minio.PutObjectOptions{})
		if err != nil && minio.ToErrorResponse(err).Code == "NoSuchUpload" {
			stat, statErr := c.Storage.StatObject(ctx, in.Bucket, in.Key, minio.StatObjectOptions{})
			if statErr == nil && stat.Size == in.Size {
				return out, nil
			}
			return out, errors.Join(err, statErr)
		}
		return out, err
	case "upload.verify":
		reader, err := c.Storage.GetObject(ctx, in.Bucket, in.Key, minio.GetObjectOptions{})
		if err != nil {
			return out, err
		}
		defer reader.Close()
		h := sha256.New()
		size, err := io.Copy(h, reader)
		if err != nil {
			return out, err
		}
		out.SHA256 = hex.EncodeToString(h.Sum(nil))
		out.Size = size
		return out, nil
	case "canonical.ensure":
		key := fmt.Sprintf("%s%s/g%d", in.TargetKey, t.ID, t.Generation)
		if !strings.HasPrefix(key, "objects/compute/") {
			return out, asynccontrol.ErrInvalid
		}
		if err := c.Control.RecordOutput(ctx, t, in.TargetBucket, key); err != nil {
			return out, err
		}
		_, err := c.Storage.CopyObject(ctx, minio.CopyDestOptions{Bucket: in.TargetBucket, Object: key}, minio.CopySrcOptions{Bucket: in.Bucket, Object: in.Key})
		return asyncprotocol.Output{Bucket: in.TargetBucket, Key: key}, err
	case "thumbnail":
		key := fmt.Sprintf("variants/compute/%s/%d/%s/g%d.jpg", in.ObjectID, in.ContentGeneration, t.ID, t.Generation)
		if strings.HasPrefix(in.Mime, "image/") {
			if err := c.Control.RecordOutput(ctx, t, in.Bucket, key); err != nil {
				return out, err
			}
		}
		return media.ComputeThumbnail(ctx, c.Storage, in, key)
	case "multipart.sweep":
		if !strings.HasPrefix(in.Key, "staging/") || c.Read == nil {
			return out, asynccontrol.ErrInvalid
		}
		var active int64
		if err := c.Read.WithContext(ctx).Table("upload_sessions").Where("id=? AND status NOT IN ?", in.SessionID, []string{"completed", "failed", "aborted", "expired"}).Count(&active).Error; err != nil {
			return out, err
		}
		if active > 0 {
			return out, asynccontrol.ErrInvalid
		}
		keyMarker, idMarker := "", ""
		for page := 0; page < 8; page++ {
			listed, err := core.ListMultipartUploads(ctx, in.Bucket, in.Key, keyMarker, idMarker, "", 128)
			if err != nil {
				return out, err
			}
			for _, u := range listed.Uploads {
				if u.Key == in.Key {
					if err := core.AbortMultipartUpload(ctx, in.Bucket, u.Key, u.UploadID); err != nil && minio.ToErrorResponse(err).Code != "NoSuchUpload" {
						return out, err
					}
				}
			}
			if !listed.IsTruncated {
				return out, nil
			}
			keyMarker = listed.NextKeyMarker
			idMarker = listed.NextUploadIDMarker
		}
		return out, fmt.Errorf("multipart sweep listing exceeded bound")
	case "multipart.abort":
		if in.UploadID == "" {
			return out, nil
		}
		err := core.AbortMultipartUpload(ctx, in.Bucket, in.Key, in.UploadID)
		if minio.ToErrorResponse(err).Code == "NoSuchUpload" {
			return out, nil
		}
		return out, err
	case "object.delete":
		for _, key := range in.VariantKeys {
			if err := c.Storage.RemoveObject(ctx, in.Bucket, key, minio.RemoveObjectOptions{}); err != nil {
				return out, err
			}
		}
		return out, c.Storage.RemoveObject(ctx, in.Bucket, in.Key, minio.RemoveObjectOptions{})
	case "staging.delete", "variant.delete", "output.delete":
		if !(strings.HasPrefix(in.Key, "staging/") || strings.HasPrefix(in.Key, "objects/compute/") || strings.HasPrefix(in.Key, "variants/")) {
			return out, asynccontrol.ErrInvalid
		}
		// A private output may have won a business commit after the cleanup was
		// reserved. The authoritative read protects that published output.
		if c.Read == nil {
			return out, errors.New("cleanup requires business readonly connection")
		}
		var used int64
		if err := c.Read.WithContext(ctx).Raw(`SELECT (SELECT count(*) FROM file_objects WHERE bucket=? AND object_key=? AND status IN ('ready','deleting'))+(SELECT count(*) FROM object_variants WHERE object_key=? AND status='ready')`, in.Bucket, in.Key, in.Key).Scan(&used).Error; err != nil {
			return out, err
		}
		if used > 0 {
			out.Noop = true
			return out, nil
		}
		return out, c.Storage.RemoveObject(ctx, in.Bucket, in.Key, minio.RemoveObjectOptions{})
	case "ai.index", "ai.reprocess", "search.project":
		if c.Auxiliary == nil {
			return out, errors.New("auxiliary executor not configured")
		}
		return out, c.Auxiliary(ctx, t, in)
	default:
		return out, asynccontrol.ErrInvalid
	}
}
