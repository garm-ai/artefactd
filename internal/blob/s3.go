package blob

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ErrNoObject is a key the store has nothing at.
var ErrNoObject = errors.New("blob: no object at that key")

// Store is the object store as this service uses it: two presigned URLs, a
// head, and a delete.
//
// An interface because the service is tested against a double as well as
// against a real SeaweedFS, and because a deployment on something other than
// S3 replaces one file. It deliberately has no Get and no Put: this service
// never handles a byte of an artefact, and a method that could would be the
// first step back towards bytes on the request bus.
type Store interface {
	// PresignPut is a credential to write exactly one object.
	PresignPut(ctx context.Context, key, mediaType string, ttl time.Duration) (string, time.Time, error)
	// PresignGet is a credential to read exactly one object.
	PresignGet(ctx context.Context, key, mediaType, filename string, ttl time.Duration) (string, time.Time, error)
	// Head is how commit checks that the bytes arrived: the size the store
	// says it holds, or ErrNoObject.
	Head(ctx context.Context, key string) (int64, error)
	// Delete removes one object. It is what makes an artefact sealed by the
	// null codec unreadable, since there is no key to destroy.
	Delete(ctx context.Context, key string) error
}

// S3 is Store over any S3-compatible endpoint.
type S3 struct {
	client *s3.Client
	signer *s3.PresignClient
	bucket string
}

// Config is what S3 needs. Credentials are not here: they come from the AWS
// SDK's default chain, as `garm catalogue publish` takes them, so a deployment
// configures this service the way it configures everything else that reaches
// the same bucket.
type Config struct {
	// Bucket is the one bucket every tenant's prefix lives in.
	Bucket string
	// Endpoint is AWS_ENDPOINT_URL for anything that is not AWS — SeaweedFS
	// in the development plane, MinIO, a test double. Empty means AWS.
	Endpoint string
	// Region is what the signer signs for. An S3-compatible store ignores it
	// and the signature still has to name one.
	Region string
}

// NewS3 builds the client.
//
// Path-style addressing whenever an endpoint is set, because virtual-host
// addressing needs a wildcard DNS name per bucket and the stores this is
// pointed at locally serve path-style only. Checksums only when the operation
// requires them, because the SDK's default adds a CRC32 trailer to every
// PutObject and several S3-compatible stores reject it outright — and a
// presigned PUT is signed here and executed by somebody else, so a trailer
// this side decided on is a signature the writer cannot satisfy.
func NewS3(ctx context.Context, cfg Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("blob: no bucket configured")
	}
	loaded, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("blob: loading AWS configuration: %w", err)
	}
	if cfg.Region != "" {
		loaded.Region = cfg.Region
	}
	if loaded.Region == "" {
		loaded.Region = "us-east-1"
	}
	opts := func(o *s3.Options) {
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true
		}
	}
	client := s3.NewFromConfig(loaded, opts)
	return &S3{client: client, signer: s3.NewPresignClient(client), bucket: cfg.Bucket}, nil
}

// Bucket is which bucket this store writes to, for a startup log line.
func (s *S3) Bucket() string { return s.bucket }

func (s *S3) PresignPut(ctx context.Context, key, mediaType string, ttl time.Duration) (string, time.Time, error) {
	if _, err := TenantOf(key); err != nil {
		return "", time.Time{}, err
	}
	in := &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if mediaType != "" {
		in.ContentType = aws.String(mediaType)
	}
	req, err := s.signer.PresignPutObject(ctx, in, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("blob: signing a write of %s: %w", key, err)
	}
	return req.URL, time.Now().UTC().Add(ttl), nil
}

// PresignGet signs a read, and asks the store to serve it as a download with
// the artefact's own media type and name.
//
// The response-header overrides are on the signature, so a URL that leaks
// cannot be turned into an inline-rendered page of somebody else's choosing:
// what the store sends back is what was signed.
func (s *S3) PresignGet(ctx context.Context, key, mediaType, filename string, ttl time.Duration) (string, time.Time, error) {
	if _, err := TenantOf(key); err != nil {
		return "", time.Time{}, err
	}
	in := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if mediaType != "" {
		in.ResponseContentType = aws.String(mediaType)
	}
	if filename != "" {
		in.ResponseContentDisposition = aws.String(contentDisposition(filename))
	}
	req, err := s.signer.PresignGetObject(ctx, in, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("blob: signing a read of %s: %w", key, err)
	}
	return req.URL, time.Now().UTC().Add(ttl), nil
}

func (s *S3) Head(ctx context.Context, key string) (int64, error) {
	if _, err := TenantOf(key); err != nil {
		return 0, err
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key),
	})
	if err != nil {
		var missing *types.NotFound
		if errors.As(err, &missing) {
			return 0, ErrNoObject
		}
		// SeaweedFS answers a 404 that the SDK does not always model as
		// NotFound for HeadObject, because a HEAD has no body to parse an
		// error code out of. A status-only 404 is still "not there".
		if isNotFoundStatus(err) {
			return 0, ErrNoObject
		}
		return 0, fmt.Errorf("blob: reading the size of %s: %w", key, err)
	}
	if out.ContentLength == nil {
		return 0, fmt.Errorf("blob: %s exists and the store reported no size", key)
	}
	return *out.ContentLength, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if _, err := TenantOf(key); err != nil {
		return err
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("blob: deleting %s: %w", key, err)
	}
	return nil
}
