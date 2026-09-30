package artefactd_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"google.golang.org/protobuf/proto"

	artefactsv1 "github.com/garm-ai/artefactd/gen/garm/artefacts/v1"
	"github.com/garm-ai/artefactd/internal/blob"
)

// THE TEST THAT PROVES THE DESIGN WORKS RATHER THAN COMPILES.
//
// begin_write, then the bytes written to the object store by a caller holding
// nothing but a presigned URL, then commit, then a read URL minted through the
// governed call and fetched with an ordinary HTTP client. Every hop is real: a
// broker, a Postgres, and an S3-compatible store that has to honour a signature
// this process produced.
//
// A double cannot tell you any of that. It cannot tell you that SeaweedFS
// accepts a presigned PUT with the content type baked into the signature, that a
// HEAD answers a size in the shape the SDK models, or that a presigned GET works
// with no credential on the client at all — which is the entire premise of the
// read path, and the reason the bytes never cross NATS.
//
// It skips without an endpoint, because a test that invented an object store
// would be testing the invention.
func TestTheWholeRoundTripAgainstARealObjectStore(t *testing.T) {
	endpoint, bucket := objectStoreFromEnv(t)

	objects, err := blob.NewS3(context.Background(), blob.Config{
		Bucket: bucket, Endpoint: endpoint, Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	makeBucket(t, endpoint, bucket)

	f := newFixture(t, objects)
	payload := []byte("the third quarter, as a spreadsheet nobody will read\n")
	sum := sha256.Sum256(payload)
	plaintext := hex.EncodeToString(sum[:])

	// 1. Name it. The service chooses the key and hands back a place to put the
	//    bytes; nothing about the key is the caller's to decide.
	var begun artefactsv1.BeginWriteResponse
	f.ok(t, routeBegin, generator(), &artefactsv1.BeginWriteRequest{
		MediaType: proto.String(xlsx), Label: proto.String("Q3.xlsx"),
		ToolFqn: proto.String(theTool), RetentionSeconds: proto.Uint32(3600),
	}, &begun)
	id := begun.GetArtefactId()
	t.Cleanup(func() { removeObject(t, endpoint, bucket, keyOf(id)) })

	// 2. Write the bytes with the presigned URL and nothing else. No credential,
	//    no SDK, no knowledge of the bucket: this is the generator's half of
	//    ruling 2, and it is what keeps the payload off NATS.
	writeWith(t, begun.GetUploadUrl(), payload)

	// 3. Commit. The service asks the store what it holds and refuses a size that
	//    disagrees, which is as far as it can check without reading a payload it
	//    must never read.
	var committed artefactsv1.CommitResponse
	f.ok(t, routeCommit, generator(), &artefactsv1.CommitRequest{
		ArtefactId:         proto.String(id),
		PlaintextSha256:    proto.String(plaintext),
		PlaintextSizeBytes: proto.Uint64(uint64(len(payload))),
		StoredSha256:       proto.String(plaintext),
		StoredSizeBytes:    proto.Uint64(uint64(len(payload))),
	}, &committed)
	if committed.GetState() != artefactsv1.State_STATE_COMMITTED {
		t.Fatalf("commit answered %v", committed.GetState())
	}

	// 4. And a size the store disagrees with is refused, so the check above is
	//    not decoration. It has to be a SECOND artefact: a repeated commit of the
	//    first names the same digest, which is the idempotent retry a generator
	//    makes after a timeout it never saw the answer to, and it is answered from
	//    the row without asking the store anything.
	var second artefactsv1.BeginWriteResponse
	f.ok(t, routeBegin, generator(), &artefactsv1.BeginWriteRequest{
		MediaType: proto.String(xlsx), Label: proto.String("Q3-again.xlsx"),
		ToolFqn: proto.String(theTool), RetentionSeconds: proto.Uint32(3600),
	}, &second)
	t.Cleanup(func() { removeObject(t, endpoint, bucket, keyOf(second.GetArtefactId())) })
	writeWith(t, second.GetUploadUrl(), payload)

	code, why := f.call(t, routeCommit, generator(), &artefactsv1.CommitRequest{
		ArtefactId:         proto.String(second.GetArtefactId()),
		PlaintextSha256:    proto.String(plaintext),
		PlaintextSizeBytes: proto.Uint64(1),
		StoredSha256:       proto.String(plaintext),
		StoredSizeBytes:    proto.Uint64(1),
	}, nil)
	if code != "400" {
		t.Errorf("a commit claiming one byte for a %d-byte object answered %s (%s), want 400",
			len(payload), code, why)
	}

	// 5. Mint a read URL through the governed call, as a person.
	var released artefactsv1.ReadUrlResponse
	f.ok(t, routeReadURL, person(), &artefactsv1.ReadUrlRequest{
		ArtefactId: proto.String(id),
	}, &released)
	url := factOf(released.GetRelease(), "url")
	if url == "" {
		t.Fatal("read_url released a card with no url fact")
	}

	// 6. Fetch it back with nothing but the URL, and check the bytes are the
	//    bytes. This is the read path end to end: the object store serves the
	//    payload with no proxy in the middle, which is what collapses if the
	//    daemon is ever put in the way.
	got, err := http.Get(url)
	if err != nil {
		t.Fatalf("fetching the presigned URL: %v", err)
	}
	fetched, err := io.ReadAll(got.Body)
	got.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusCode/100 != 2 {
		t.Fatalf("the object store refused the presigned GET: %s %s", got.Status, fetched)
	}
	if !bytes.Equal(fetched, payload) {
		t.Fatalf("fetched %d bytes, wrote %d", len(fetched), len(payload))
	}
	back := sha256.Sum256(fetched)
	if hex.EncodeToString(back[:]) != plaintext {
		t.Error("the digest of what came back is not the digest that was committed")
	}

	// 7. An agent asking for the same artefact is refused against the real store
	//    as well, because the check is the service's and not the bucket's.
	code, why = f.call(t, routeReadURL, throughAnAgent(),
		&artefactsv1.ReadUrlRequest{ArtefactId: proto.String(id)}, nil)
	if code != "403" {
		t.Errorf("an agent was answered %s (%s), want 403", code, why)
	}
}

// writeWith is a generator's half of the write path: an ordinary HTTP PUT with
// nothing but the URL the service signed.
//
// The content type is sent because it is part of that signature — the service
// signed the media type the artefact declared — so a writer that sends a
// different one is refused. That is worth having: it means the object's recorded
// type is the type the store will serve it as.
func writeWith(t *testing.T, url string, payload []byte) {
	t.Helper()
	put, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	put.Header.Set("Content-Type", xlsx)
	put.ContentLength = int64(len(payload))
	resp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatalf("writing the bytes with the presigned URL: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("the object store refused the presigned PUT: %s %s", resp.Status, body)
	}
}

// keyOf is the key the service will have chosen, which the cleanup needs and
// which nothing else in this test is allowed to assume.
func keyOf(id string) string {
	key, err := blob.Key(theTenant, theRun, id)
	if err != nil {
		panic(err)
	}
	return key
}

// objectStoreFromEnv reads where the real store is, and skips when there is
// none.
//
// Two variables rather than one, and the bucket is required: a test that
// defaulted to a bucket name would write into somebody else's.
func objectStoreFromEnv(t *testing.T) (endpoint, bucket string) {
	t.Helper()
	endpoint = os.Getenv("ARTEFACT_S3_ENDPOINT")
	bucket = os.Getenv("ARTEFACT_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("ARTEFACT_S3_ENDPOINT and ARTEFACT_BUCKET are not set; run `mise run s3` and " +
			"export what it prints")
	}
	if strings.TrimSpace(os.Getenv("AWS_ACCESS_KEY_ID")) == "" {
		t.Skip("AWS_ACCESS_KEY_ID is not set; the SDK's default chain has no credentials to sign with")
	}
	return endpoint, bucket
}

func testClient(t *testing.T, endpoint string) *s3.Client {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
}

// makeBucket creates the test's own bucket, and is happy if it is already there.
// The service never creates one: a service that invented a bucket would create
// it and look like it worked.
func makeBucket(t *testing.T, endpoint, bucket string) {
	t.Helper()
	c := testClient(t, endpoint)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err == nil {
		return
	}
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		var exists interface{ ErrorCode() string }
		if errors.As(err, &exists) && strings.Contains(exists.ErrorCode(), "AlreadyOwned") {
			return
		}
		t.Fatalf("creating the bucket %q: %v", bucket, err)
	}
}

// removeObject leaves the store as the test found it. A failure is reported and
// not fatal: the assertions have already run, and a leftover object in a
// development bucket is worth knowing about rather than worth a red suite.
func removeObject(t *testing.T, endpoint, bucket, key string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := testClient(t, endpoint).DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Logf("could not remove %s: %v", fmt.Sprintf("s3://%s/%s", bucket, key), err)
	}
}
