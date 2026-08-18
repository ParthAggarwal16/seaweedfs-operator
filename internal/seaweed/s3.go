package seaweed

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type S3Client struct {
	api      *s3.Client
	endpoint string
}

type S3Options struct {
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	HTTPClient      *http.Client
}

func NewS3Client(opts S3Options) *S3Client {

	region := opts.Region
	if region == "" {
		region = "us-east-1"
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	api := s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(opts.Endpoint),
		UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(
			opts.AccessKeyID, opts.SecretAccessKey, ""),
		HTTPClient: awshttp.NewBuildableClient().WithTimeout(httpClient.Timeout),
	})

	return &S3Client{api: api, endpoint: opts.Endpoint}
}

func (c *S3Client) Endpoint() string { return c.endpoint }

func (c *S3Client) EnsureBucket(ctx context.Context, name string, objectLock bool) (created bool, err error) {
	exists, err := c.BucketExists(ctx, name)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}

	in := &s3.CreateBucketInput{Bucket: &name}
	if objectLock {
		in.ObjectLockEnabledForBucket = aws.Bool(true)
	}
	if _, err := c.api.CreateBucket(ctx, in); err != nil {
		var owned *types.BucketAlreadyOwnedByYou
		var already *types.BucketAlreadyExists
		if errors.As(err, &owned) || errors.As(err, &already) {
			return false, nil
		}
		return false, fmt.Errorf("create bucket %q: %w", name, err)
	}
	return true, nil
}

func (c *S3Client) BucketExists(ctx context.Context, name string) (bool, error) {
	_, err := c.api.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &name})
	if err == nil {
		return true, nil
	}
	var notFound *types.NotFound
	var noBucket *types.NoSuchBucket
	if errors.As(err, &notFound) || errors.As(err, &noBucket) {
		return false, nil
	}
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusNotFound {
		return false, nil
	}
	return false, fmt.Errorf("head bucket %q: %w", name, err)
}

func (c *S3Client) DeleteBucket(ctx context.Context, name string) error {
	if err := c.emptyBucket(ctx, name); err != nil {
		return err
	}
	if _, err := c.api.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &name}); err != nil {
		var noBucket *types.NoSuchBucket
		if errors.As(err, &noBucket) {
			return nil
		}
		var respErr *awshttp.ResponseError
		if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("delete bucket %q: %w", name, err)
	}
	return nil
}

func (c *S3Client) emptyBucket(ctx context.Context, name string) error {
	paginator := s3.NewListObjectsV2Paginator(c.api, &s3.ListObjectsV2Input{Bucket: &name})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			var noBucket *types.NoSuchBucket
			if errors.As(err, &noBucket) {
				return nil
			}
			var respErr *awshttp.ResponseError
			if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusNotFound {
				return nil
			}
			return fmt.Errorf("list objects in %q: %w", name, err)
		}
		if len(page.Contents) == 0 {
			continue
		}
		ids := make([]types.ObjectIdentifier, 0, len(page.Contents))
		for _, obj := range page.Contents {
			ids = append(ids, types.ObjectIdentifier{Key: obj.Key})
		}
		out, err := c.api.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: &name,
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("delete objects in %q: %w", name, err)
		}
		if len(out.Errors) > 0 {
			return fmt.Errorf("delete objects in %q: %d objects failed, first: %s",
				name, len(out.Errors), aws.ToString(out.Errors[0].Message))
		}
	}
	return nil
}

func (c *S3Client) ListBuckets(ctx context.Context) ([]string, error) {
	out, err := c.api.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return nil, fmt.Errorf("list buckets: %w", err)
	}
	names := make([]string, 0, len(out.Buckets))
	for _, b := range out.Buckets {
		names = append(names, aws.ToString(b.Name))
	}
	return names, nil
}

func GenerateAccessKeyID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate access key: %w", err)
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	return strings.ToUpper(enc[:20]), nil
}

func GenerateSecretAccessKey() (string, error) {
	raw := make([]byte, 30)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate secret key: %w", err)
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	return enc[:40], nil
}
