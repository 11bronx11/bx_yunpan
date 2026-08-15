package storageurl

import (
	"context"
	"net/url"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Presigner struct {
	client     *minio.Client
	pathPrefix string
}

func New(endpoint, accessKey, secretKey, region string, secure bool, pathPrefix string) (*Presigner, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: secure,
		Region: region,
	})
	if err != nil {
		return nil, err
	}
	return &Presigner{client: client, pathPrefix: pathPrefix}, nil
}

func (p *Presigner) Presign(ctx context.Context, method, bucket, object string, expiry time.Duration, query url.Values) (*url.URL, error) {
	signed, err := p.client.Presign(ctx, method, bucket, object, expiry, query)
	return p.publicURL(signed, err)
}

func (p *Presigner) PresignedGetObject(ctx context.Context, bucket, object string, expiry time.Duration, query url.Values) (*url.URL, error) {
	signed, err := p.client.PresignedGetObject(ctx, bucket, object, expiry, query)
	return p.publicURL(signed, err)
}

func (p *Presigner) publicURL(signed *url.URL, err error) (*url.URL, error) {
	if err != nil {
		return nil, err
	}
	if p.pathPrefix == "" {
		return signed, nil
	}
	result := *signed
	result.Path = p.pathPrefix + signed.Path
	result.RawPath = ""
	return &result, nil
}
