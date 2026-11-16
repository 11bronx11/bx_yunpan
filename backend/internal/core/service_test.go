package core

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/core/pb"
	"github.com/11bronx11/bx_yunpan/backend/internal/objectstore"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/serviceauth"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/storageurl"
	"github.com/11bronx11/bx_yunpan/backend/internal/testdb"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A06: a bearer-authenticated caller still needs a signed, matching task grant.
func TestProcessingGrantRejectsMismatchedClaims(t *testing.T) {
	pub, private, _ := ed25519.GenerateKey(rand.Reader)
	s := Service{ScopeKey: pub, ProcessingKey: pub}
	g := aiprotocol.ProcessingGrant{IntentID: uuid.New(), ObjectID: uuid.New(), ContentGeneration: 1, InputIdentity: strings.Repeat("a", 64), ContentHash: strings.Repeat("b", 64), Deadline: time.Now().Add(time.Minute).Unix(), Action: "object.read"}
	for _, name := range []string{"intent", "object", "generation", "identity", "action", "expired", "scope-as-processing"} {
		t.Run(name, func(t *testing.T) {
			claims := g
			r := &pb.ProcessingRequest{IntentId: g.IntentID.String(), ObjectId: g.ObjectID.String(), ContentGeneration: 1, InputIdentity: g.InputIdentity}
			switch name {
			case "intent":
				r.IntentId = uuid.NewString()
			case "object":
				r.ObjectId = uuid.NewString()
			case "generation":
				r.ContentGeneration++
			case "identity":
				r.InputIdentity += "tampered"
			case "action":
				claims.Action = "retrieve"
			case "expired":
				claims.Deadline = time.Now().Unix()
			}
			audience := "core-processing-v1"
			if name == "scope-as-processing" {
				audience = "core-retrieval-v1"
			}
			r.Grant, _ = serviceauth.Sign(private, audience, time.Now().Add(time.Minute).Unix(), claims)
			out, err := s.GetObjectForProcessing(context.Background(), r)
			if out != nil || status.Code(err) != codes.PermissionDenied {
				t.Fatalf("invalid claim reached business DB: %v", err)
			}
		})
	}
}

// A07: business revocation prevents new URLs while previously issued URLs
// retain their documented finite lifetime. Only this test's bucket is removed.
func TestProcessingReadURLExpiresAfterReferenceRevocation(t *testing.T) {
	endpoint, key, secret := os.Getenv("TEST_MINIO_ENDPOINT"), os.Getenv("TEST_MINIO_ACCESS_KEY"), os.Getenv("TEST_MINIO_SECRET_KEY")
	if endpoint == "" || key == "" || secret == "" {
		t.Skip("dedicated MinIO required")
	}
	db := testdb.Business(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(key, secret, ""), Region: "us-east-1"})
	if err != nil {
		t.Fatal("test object client unavailable")
	}
	bucket := "ai-deep-grant-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	object := "synthetic.txt"
	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
		t.Fatal("test bucket unavailable")
	}
	t.Cleanup(func() {
		cleanup, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		if err := client.RemoveObject(cleanup, bucket, object, minio.RemoveObjectOptions{}); err != nil {
			t.Error("owned object cleanup failed")
		}
		if err := client.RemoveBucket(cleanup, bucket); err != nil {
			t.Error("owned bucket cleanup failed")
		}
	})
	data := []byte("synthetic authorization TTL")
	if _, err := client.PutObject(ctx, bucket, object, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{}); err != nil {
		t.Fatal("test upload failed")
	}
	pub, private, _ := ed25519.GenerateKey(rand.Reader)
	presigner, err := storageurl.New(endpoint, key, secret, "us-east-1", false, "")
	if err != nil {
		t.Fatal("test presigner failed")
	}
	o := objectstore.Object{ID: uuid.New(), SHA256: aiprotocol.Hash(data), SizeBytes: int64(len(data)), MimeType: "text/plain", Bucket: bucket, ObjectKey: object, Status: "ready", ReferenceCount: 1, Version: 1}
	if err := db.Create(&o).Error; err != nil {
		t.Fatal(err)
	}
	g := aiprotocol.ProcessingGrant{IntentID: uuid.New(), ObjectID: o.ID, ContentGeneration: 1, ContentHash: o.SHA256, InputIdentity: strings.Repeat("b", 64), Deadline: time.Now().Add(8 * time.Second).Unix(), Action: "object.read"}
	token, _ := serviceauth.Sign(private, "core-processing-v1", g.Deadline, g)
	request := &pb.ProcessingRequest{Grant: token, IntentId: g.IntentID.String(), ObjectId: o.ID.String(), ContentGeneration: 1, InputIdentity: g.InputIdentity}
	s := Service{DB: db, ProcessingKey: pub, Presigner: presigner}
	authorization, err := s.GetObjectForProcessing(ctx, request)
	if err != nil {
		t.Fatal("live object authorization failed")
	}
	if authorization.ExpiresAt > g.Deadline {
		t.Fatal("read URL outlives processing grant")
	}
	if err := db.Model(&o).Update("reference_count", 0).Error; err != nil {
		t.Fatal(err)
	}
	if out, err := s.GetObjectForProcessing(ctx, request); out != nil || status.Code(err) != codes.FailedPrecondition {
		t.Fatal("revoked object received another URL")
	}
	get := func() int {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, authorization.ReadUrl, nil)
		res, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
		if err != nil {
			t.Fatal("signed read request failed")
		}
		defer res.Body.Close()
		b, err := io.ReadAll(io.LimitReader(res.Body, 4096))
		if err != nil {
			t.Fatal("signed read body failed")
		}
		if res.StatusCode == 200 && !bytes.Equal(b, data) {
			t.Fatal("signed read bytes differ")
		}
		return res.StatusCode
	}
	if get() != 200 {
		t.Fatal("previously issued URL revoked before TTL")
	}
	timer := time.NewTimer(time.Until(time.Unix(authorization.ExpiresAt+1, 0)))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal("TTL boundary exceeded test bound")
	}
	if get() != http.StatusForbidden {
		t.Fatal("expired URL still authorizes read")
	}
}

// A07: outstanding processing grants cannot mint NEW URLs after the last
// business reference disappears. Previously minted URLs keep their short TTL.
func TestProcessingGrantRequiresLiveReference(t *testing.T) {
	db := testdb.Business(t)
	pub, private, _ := ed25519.GenerateKey(rand.Reader)
	presigner, err := storageurl.New("127.0.0.1:1", "fixture", "fixture-secret", "us-east-1", false, "")
	if err != nil {
		t.Fatal(err)
	}
	s := Service{DB: db, ProcessingKey: pub, Presigner: presigner}
	o := objectstore.Object{ID: uuid.New(), SHA256: strings.Repeat("a", 64), SizeBytes: 4, MimeType: "text/plain", Bucket: "fixture", ObjectKey: uuid.NewString(), Status: "ready", ReferenceCount: 1, Version: 1}
	if err := db.Create(&o).Error; err != nil {
		t.Fatal(err)
	}
	g := aiprotocol.ProcessingGrant{IntentID: uuid.New(), ObjectID: o.ID, ContentGeneration: 1, ContentHash: o.SHA256, InputIdentity: strings.Repeat("b", 64), Deadline: time.Now().Add(time.Minute).Unix(), Action: "object.read"}
	token, _ := serviceauth.Sign(private, "core-processing-v1", g.Deadline, g)
	r := &pb.ProcessingRequest{Grant: token, IntentId: g.IntentID.String(), ObjectId: o.ID.String(), ContentGeneration: 1, InputIdentity: g.InputIdentity}
	if out, err := s.GetObjectForProcessing(context.Background(), r); err != nil || out.ReadUrl == "" {
		t.Fatalf("live grant rejected: %v", err)
	}
	if err := db.Model(&o).Update("reference_count", 0).Error; err != nil {
		t.Fatal(err)
	}
	if out, err := s.GetObjectForProcessing(context.Background(), r); out != nil || status.Code(err) != codes.FailedPrecondition {
		t.Fatal("unreferenced object received a new read authorization")
	}
	if err := db.Model(&o).Updates(map[string]any{"reference_count": 1, "content_generation": 2}).Error; err != nil {
		t.Fatal(err)
	}
	if out, err := s.GetObjectForProcessing(context.Background(), r); out != nil || status.Code(err) != codes.FailedPrecondition {
		t.Fatal("stale generation received a read authorization")
	}
}

func TestReprocessUnavailableFileDoesNotCreateJob(t *testing.T) {
	db := testdb.Business(t)
	job, _, err := RequestReprocess(context.Background(), db, uuid.New(), uuid.New(), uuid.NewString(), []string{"summary"}, aiprotocol.Profile{ID: "default", Revision: "v1", EnableSummary: true})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("inaccessible command must match HTTP 404 contract: %v", err)
	}
	var jobs, requests int64
	if err := db.Table("async_tasks").Where("id=?", job.ID).Count(&jobs).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("ai_job_requests").Where("job_id=?", job.ID).Count(&requests).Error; err != nil {
		t.Fatal(err)
	}
	if jobs != 0 || requests != 0 {
		t.Fatal("unauthorized command persisted effects")
	}
}
