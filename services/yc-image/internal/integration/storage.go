// Package integration holds yc-image's object storage client (S3: SeaweedFS
// in dev, any S3-compatible store in production).
package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Storage errors.
var (
	ErrNotFound = errors.New("storage: object not found")
	ErrTooLarge = errors.New("storage: object too large")
)

// StorageConfig is the `storage:` block.
type StorageConfig struct {
	// Endpoint is the S3 API URL the service uses, e.g. http://seaweedfs:8333.
	Endpoint string `koanf:"endpoint"`
	// PublicEndpoint is the S3 API URL clients use for presigned uploads;
	// defaults to Endpoint.
	PublicEndpoint string `koanf:"public_endpoint"`
	Region         string `koanf:"region"`
	Bucket         string `koanf:"bucket"`
	AccessKey      string `koanf:"access_key"`
	SecretKey      string `koanf:"secret_key"`
	// PublicBaseURL prefixes object keys in served URLs: a CDN, or the
	// bucket's public-read URL.
	PublicBaseURL string        `koanf:"public_base_url"`
	PresignTTL    time.Duration `koanf:"presign_ttl"`
}

// Storage stores image files under <tenant>/<image>/.
type Storage struct {
	client    *minio.Client
	presigner *minio.Client
	bucket    string
	public    string
	ttl       time.Duration
}

// NewStorage returns a client. It does not contact the store.
func NewStorage(cfg StorageConfig) (*Storage, error) {
	client, err := newClient(cfg.Endpoint, cfg)
	if err != nil {
		return nil, err
	}
	presigner := client
	if cfg.PublicEndpoint != "" && cfg.PublicEndpoint != cfg.Endpoint {
		if presigner, err = newClient(cfg.PublicEndpoint, cfg); err != nil {
			return nil, err
		}
	}
	return &Storage{
		client: client, presigner: presigner, bucket: cfg.Bucket,
		public: strings.TrimSuffix(cfg.PublicBaseURL, "/"), ttl: cfg.PresignTTL,
	}, nil
}

func newClient(endpoint string, cfg StorageConfig) (*minio.Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("storage endpoint %q: want http(s)://host[:port]", endpoint)
	}
	// The region is fixed so presigning never needs a bucket-location call.
	c, err := minio.New(u.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: u.Scheme == "https",
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("storage client: %w", err)
	}
	return c, nil
}

// Ping checks the bucket is reachable.
func (s *Storage) Ping(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("bucket %s does not exist", s.bucket)
	}
	return nil
}

// Put stores data at key. Variants never change, so they are cacheable
// forever.
func (s *Storage) Put(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType: contentType, CacheControl: "public, max-age=31536000, immutable",
	})
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

// Get reads key, failing with ErrTooLarge beyond maxBytes and ErrNotFound
// when it is missing.
func (s *Storage) Get(ctx context.Context, key string, maxBytes int64) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	defer func() { _ = obj.Close() }()
	data, err := io.ReadAll(io.LimitReader(obj, maxBytes+1))
	if err != nil {
		if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrTooLarge
	}
	return data, nil
}

// DeletePrefix deletes every object under prefix.
func (s *Storage) DeletePrefix(ctx context.Context, prefix string) error {
	objects := s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true})
	var errs []error
	for r := range s.client.RemoveObjects(ctx, s.bucket, objects, minio.RemoveObjectsOptions{}) {
		errs = append(errs, r.Err)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("delete %s: %w", prefix, err)
	}
	return nil
}

// PresignPut returns a URL a client can PUT key's content to, and when it
// expires. The content type is signed: the bucket is publicly readable, so
// an unvalidated upload must not be served as anything but an image.
func (s *Storage) PresignPut(ctx context.Context, key, contentType string, now time.Time) (string, time.Time, error) {
	u, err := s.presigner.PresignHeader(ctx, http.MethodPut, s.bucket, key, s.ttl, nil,
		http.Header{"Content-Type": {contentType}})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("presign %s: %w", key, err)
	}
	return u.String(), now.Add(s.ttl), nil
}

// URL is where key is served.
func (s *Storage) URL(key string) string {
	return s.public + "/" + key
}
