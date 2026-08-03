//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// s3OpTimeout bounds every in-process S3 operation. A hung or unreachable MinIO
// must fail fast and actionably rather than block the harness on a bare
// context.Background().
const s3OpTimeout = 30 * time.Second

// newMinioClient builds an in-process minio-go S3 client for the harness MinIO
// instance. It strips the scheme from the http(s) endpoint (minio.New wants a
// bare host:port), authenticates with the harness's static root credentials, and
// selects TLS from the scheme (always plain http for the local podman MinIO).
//
// This client replaces the previous `podman run mc ...` text-parsing surface: it
// returns typed ObjectInfo, so an empty bucket counts as 0 regardless of any
// cgroup/warning lines podman writes to stderr. Podman 4.9.3 can emit several
// stderr lines per run, which text-based counting once mistook for objects.
func newMinioClient(endpoint string) (*minio.Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse S3 endpoint %q: %w", endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("S3 endpoint %q scheme %q is not http or https", endpoint, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("S3 endpoint %q is missing host", endpoint)
	}
	return minio.New(u.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(minioUser, minioPassword, ""),
		Secure: u.Scheme == "https",
	})
}

// transcriptBucketObjectCount counts the objects in the transcript bucket via a
// typed ListObjects, under a bounded context. It aborts actionably on the first
// per-item obj.Err (e.g. NoSuchBucket), mirroring the old mc-error fatal — but it
// is structurally immune to stderr noise because it never parses CLI text.
func transcriptBucketObjectCount(t *testing.T, endpoint, bucket string) int {
	t.Helper()
	bucket = requireTranscriptBucket(t, bucket)
	client, err := newMinioClient(endpoint)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "count transcript bucket objects failed",
			what:  "S3_ENDPOINT could not be converted into a MinIO client",
			why:   err.Error(),
			where: "internal/e2e/minio.go transcriptBucketObjectCount",
			when:  "asserting seeded baseline before push#1",
			means: "the harness cannot prove S3 is clean before publishing",
			fix:   "provide S3_ENDPOINT as http(s)://host:port for the harness MinIO instance",
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), s3OpTimeout)
	defer cancel()
	count := 0
	for obj := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			fatalActionable(t, actionableFailure{
				title: "count transcript bucket objects failed",
				what:  fmt.Sprintf("ListObjects could not enumerate %s", bucket),
				why:   obj.Err.Error(),
				where: "internal/e2e/minio.go transcriptBucketObjectCount",
				when:  "asserting seeded baseline before push#1",
				means: "the harness cannot prove S3 is clean before publishing",
				fix:   "check that the MinIO endpoint is reachable and the transcript bucket exists",
			})
		}
		count++
	}
	return count
}

// listTranscriptBucketObjectKeys returns the object keys in the transcript bucket
// for diagnostics (the lean on-failure baseline dump). It is best-effort: it
// returns whatever it enumerated plus the first error encountered, so the caller
// can log a self-diagnosing snapshot without aborting the run a second time.
func listTranscriptBucketObjectKeys(endpoint, bucket string) ([]string, error) {
	client, err := newMinioClient(endpoint)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), s3OpTimeout)
	defer cancel()
	var keys []string
	for obj := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			return keys, obj.Err
		}
		keys = append(keys, obj.Key)
	}
	return keys, nil
}

// ensureTranscriptBucket creates the transcript bucket, ignoring the
// already-own-it/already-exists responses (the minio-go equivalent of the old
// `mc mb --ignore-existing`).
func ensureTranscriptBucket(ctx context.Context, client *minio.Client, bucket string) error {
	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		code := minio.ToErrorResponse(err).Code
		if code == minio.BucketAlreadyOwnedByYou || code == minio.BucketAlreadyExists {
			return nil
		}
		return err
	}
	return nil
}

// startEphemeralMinIOBucket creates the transcript bucket on a freshly-started
// MinIO and health-gates it on a real BucketExists check. It returns an error so
// the caller can t.Skip on an image-pull/provisioning failure (matching the prior
// `mc mb` skip semantics).
func startEphemeralMinIOBucket(t *testing.T, endpoint, bucket string) error {
	t.Helper()
	client, err := newMinioClient(endpoint)
	if err != nil {
		return fmt.Errorf("build minio client for %s: %w", endpoint, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), s3OpTimeout)
	defer cancel()
	if err := ensureTranscriptBucket(ctx, client, bucket); err != nil {
		return fmt.Errorf("create bucket %s: %w", bucket, err)
	}
	waitReady(t, "minio-bucket", func() bool {
		opCtx, opCancel := context.WithTimeout(context.Background(), s3OpTimeout)
		defer opCancel()
		exists, err := client.BucketExists(opCtx, bucket)
		return err == nil && exists
	})
	return nil
}

// clearTranscriptBucket drains every object from the transcript bucket (the
// minio-go equivalent of `mc rm --recursive --force`, draining the
// RemoveObjects error channel), then re-ensures the bucket exists. Used between
// warm-stack distro iterations so no run inherits a prior run's objects.
func clearTranscriptBucket(t *testing.T, endpoint, bucket string) {
	t.Helper()
	bucket = requireTranscriptBucket(t, bucket)
	client, err := newMinioClient(endpoint)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "clear transcript bucket failed",
			what:  "S3_ENDPOINT could not be converted into a MinIO client",
			why:   err.Error(),
			where: "internal/e2e/minio.go clearTranscriptBucket",
			when:  "clearing S3 state during warm-stack refresh",
			means: "the next distro could inherit transcript objects from a previous run",
			fix:   "provide S3_ENDPOINT as http(s)://host:port for the harness MinIO instance",
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), s3OpTimeout)
	defer cancel()
	objectsCh := client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true})
	for rmErr := range client.RemoveObjects(ctx, bucket, objectsCh, minio.RemoveObjectsOptions{}) {
		if rmErr.Err != nil {
			fatalActionable(t, actionableFailure{
				title: "clear transcript bucket failed",
				what:  fmt.Sprintf("RemoveObjects could not delete %s from %s", rmErr.ObjectName, bucket),
				why:   rmErr.Err.Error(),
				where: "internal/e2e/minio.go clearTranscriptBucket",
				when:  "clearing S3 state during warm-stack refresh",
				means: "the next distro could inherit transcript objects from a previous run",
				fix:   "check that the MinIO endpoint is reachable and the credentials can delete objects",
			})
		}
	}
	if err := ensureTranscriptBucket(ctx, client, bucket); err != nil {
		fatalActionable(t, actionableFailure{
			title: "recreate transcript bucket failed",
			what:  fmt.Sprintf("MakeBucket could not ensure %s exists after clearing", bucket),
			why:   err.Error(),
			where: "internal/e2e/minio.go clearTranscriptBucket",
			when:  "recreating S3 state during warm-stack refresh",
			means: "peasant village push will not have a transcript bucket to write into",
			fix:   "check that the MinIO endpoint is reachable and the credentials can create buckets",
		})
	}
}
