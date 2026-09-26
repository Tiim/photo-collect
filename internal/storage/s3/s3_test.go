package s3_test

import (
	"context"
	"os"
	"testing"

	"github.com/tiim/photo-collect/internal/storage/s3"
	"github.com/tiim/photo-collect/internal/storage/storagetest"
)

// Runs against a real S3-compatible server, e.g.:
//
//	docker run -p 9000:9000 -e MINIO_ROOT_USER=minio -e MINIO_ROOT_PASSWORD=minio12345 minio/minio server /data
//	(create the bucket first) then
//	TEST_S3_ENDPOINT=http://localhost:9000 TEST_S3_BUCKET=test TEST_S3_ACCESS_KEY=minio TEST_S3_SECRET_KEY=minio12345 go test ./internal/storage/s3
func TestContract(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_S3_ENDPOINT not set")
	}
	s, err := s3.New(context.Background(), s3.Options{
		Endpoint: endpoint, Bucket: os.Getenv("TEST_S3_BUCKET"), Region: "us-east-1",
		AccessKey: os.Getenv("TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("TEST_S3_SECRET_KEY"), PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	storagetest.Run(t, s)
}
