package asyncjobs

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"

	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asyncprotocol"
)

func TestMultipartUnknownSuccessRecoveryAndDirectedSweep(t *testing.T) {
	endpoint := os.Getenv("TEST_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_MINIO_ENDPOINT required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(os.Getenv("TEST_MINIO_ACCESS_KEY"), os.Getenv("TEST_MINIO_SECRET_KEY"), "")})
	if err != nil {
		t.Fatal(err)
	}
	bucket := "async-window-" + uuid.NewString()
	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	defer client.RemoveBucket(context.Background(), bucket)
	other := lifecycle.Rule{ID: "existing-user-rule", Status: "Enabled", RuleFilter: lifecycle.Filter{Prefix: "other/"}, Expiration: lifecycle.Expiration{Days: 30}}
	if err := client.SetBucketLifecycle(ctx, bucket, &lifecycle.Configuration{Rules: []lifecycle.Rule{other}}); err != nil {
		t.Fatal(err)
	}
	if err := EnsureLifecycle(ctx, client, bucket, 14, 48*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := EnsureLifecycle(ctx, client, bucket, 14, 48*time.Hour); err != nil {
		t.Fatal(err)
	}
	rules, err := client.GetBucketLifecycle(ctx, bucket)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules.Rules) != 2 {
		t.Fatal("existing lifecycle changed", rules)
	}
	found := false
	for _, r := range rules.Rules {
		if r.ID == other.ID && r.RuleFilter.Prefix == "other/" && r.Expiration.Days == 30 {
			found = true
		}
	}
	if !found {
		t.Fatal("unrelated lifecycle overwritten")
	}
	c, notices, _ := expiryBatch(t)
	task, _, err := c.Control.LoadResult(ctx, notices[0])
	if err != nil {
		t.Fatal(err)
	}
	var sessionIn asyncprotocol.Input
	if err := json.Unmarshal(task.Input, &sessionIn); err != nil {
		t.Fatal(err)
	}
	in := asyncprotocol.Input{Bucket: bucket, Key: "staging/" + uuid.NewString(), SessionID: sessionIn.SessionID}
	body, _ := json.Marshal(in)
	computer := &Computer{Storage: client, Read: c.Business}
	create := asynccontrol.Task{Kind: "multipart.create", Input: body}
	first, err := computer.Compute(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	core := minio.Core{Client: client}
	defer core.AbortMultipartUpload(context.Background(), bucket, in.Key, first.UploadID)
	// Remote create succeeded; deliberately omit SaveResult and retry the same key.
	recovered, err := computer.Compute(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	if first.UploadID == "" || recovered.UploadID != first.UploadID {
		t.Fatal("unknown create duplicated", first, recovered)
	}
	neighbor := in.Key + "-another-operation"
	neighborID, err := core.NewMultipartUpload(ctx, bucket, neighbor, minio.PutObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer core.AbortMultipartUpload(context.Background(), bucket, neighbor, neighborID)
	sweep := asynccontrol.Task{Kind: "multipart.sweep", Input: body}
	if _, err := computer.Compute(ctx, sweep); err == nil {
		t.Fatal("active session swept")
	}
	if err := c.Business.Table("upload_sessions").Where("id=?", in.SessionID).Update("status", "aborted").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := computer.Compute(ctx, sweep); err != nil {
		t.Fatal(err)
	}
	listed, err := core.ListMultipartUploads(ctx, bucket, in.Key, "", "", "", 128)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range listed.Uploads {
		if u.Key == in.Key {
			t.Fatal("operation upload not swept")
		}
	}
	neighbors, err := core.ListMultipartUploads(ctx, bucket, neighbor, "", "", "", 128)
	if err != nil {
		t.Fatal(err)
	}
	if len(neighbors.Uploads) != 1 || neighbors.Uploads[0].UploadID != neighborID {
		t.Fatal("sweep crossed operation boundary", neighbors.Uploads)
	}
	if _, err := computer.Compute(ctx, sweep); err != nil {
		t.Fatal("sweep not idempotent", err)
	}
}
