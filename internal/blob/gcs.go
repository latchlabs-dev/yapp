package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"mime"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
)

// GCS stores attachments in a Google Cloud Storage bucket.
//
// Signing deliberately passes no credentials of its own: the storage client
// resolves the signer from Application Default Credentials, which works both
// with a service-account key file and, on Workload Identity where no private
// key exists, by delegating to the IAM signBlob API. That second path needs the
// runtime service account to hold roles/iam.serviceAccountTokenCreator on
// itself — the usual reason signed URLs work locally and fail in production.
type GCS struct {
	client *storage.Client
	bucket string
	prefix string
}

// NewGCS opens a storage client for the given bucket. prefix is prepended to
// every key so one bucket can host several environments.
func NewGCS(ctx context.Context, bucket, prefix string) (*GCS, error) {
	if bucket == "" {
		return nil, errors.New("attachments bucket is required")
	}
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("open gcs client: %w", err)
	}
	return &GCS{
		client: client,
		bucket: bucket,
		prefix: strings.Trim(prefix, "/"),
	}, nil
}

// Bucket returns the configured bucket name.
func (g *GCS) Bucket() string { return g.bucket }

// Close releases the storage client.
func (g *GCS) Close() error { return g.client.Close() }

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Location returns gcs://bucket[/prefix] for stored metadata.
func (g *GCS) Location() string {
	if g.prefix == "" {
		return "gcs://" + g.bucket
	}
	return "gcs://" + g.bucket + "/" + g.prefix
}

// Put uploads data as one object. No Content-Encoding is set even for gzip
// payloads: GCS would otherwise transparently decompress on read, and callers
// that stored gzip bytes expect gzip bytes back.
func (g *GCS) Put(ctx context.Context, key string, data []byte, contentType string) error {
	return writeObject(ctx, g.object(key), data, contentType)
}

// PutImmutable never overwrites a durable raw archive, even if a stale repair
// continues after losing its lease. Existing identical bytes are idempotent.
func (g *GCS) PutImmutable(ctx context.Context, key string, data []byte, contentType string) error {
	err := writeObject(ctx, g.object(key).If(storage.Conditions{DoesNotExist: true}), data, contentType)
	if err == nil {
		return nil
	}
	existing, readErr := g.Read(ctx, key)
	if readErr == nil && bytes.Equal(existing, data) {
		return nil
	}
	return err
}

func writeObject(ctx context.Context, object *storage.ObjectHandle, data []byte, contentType string) error {
	w := object.NewWriter(ctx)
	w.ContentType = contentType
	// Objects here are small enough to upload in one request; a single-shot
	// upload also makes the write atomic from a reader's point of view.
	w.ChunkSize = 0
	// The server rejects the upload if the stored bytes do not match this
	// checksum, which is what makes it safe to drop the Postgres copy of a
	// body after a successful Put.
	w.CRC32C = crc32.Checksum(data, castagnoli)
	w.SendCRC32C = true
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return fmt.Errorf("write object: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finalize object: %w", err)
	}
	return nil
}

// UploadHeaders are part of the signature, not client-side validation. GCS
// rejects missing/changed headers, oversized writes, and overwrites itself.
func UploadHeaders(contentType string, sizeBytes int64) map[string]string {
	size := strconv.FormatInt(sizeBytes, 10)
	return map[string]string{
		"Content-Type":                contentType,
		"X-Goog-Content-Length-Range": size + "," + size,
		"X-Goog-If-Generation-Match":  "0",
	}
}

func uploadOptions(contentType string, sizeBytes int64, expiresAt time.Time) (*storage.SignedURLOptions, error) {
	if sizeBytes <= 0 || !expiresAt.After(time.Now()) || expiresAt.After(time.Now().Add(7*24*time.Hour)) {
		return nil, errors.New("invalid upload size or expiry")
	}
	headers := UploadHeaders(contentType, sizeBytes)
	return &storage.SignedURLOptions{
		Scheme:      storage.SigningSchemeV4,
		Method:      "PUT",
		Expires:     expiresAt,
		ContentType: contentType,
		Headers: []string{
			"x-goog-content-length-range:" + headers["X-Goog-Content-Length-Range"],
			"x-goog-if-generation-match:0",
		},
	}, nil
}

// SignedUpload gives storage exactly the authority represented by the reserved
// row. Fixing expiry before signing also lets cleanup wait out that authority.
func (g *GCS) SignedUpload(_ context.Context, key, contentType string, sizeBytes int64, expiresAt time.Time) (string, error) {
	opts, err := uploadOptions(contentType, sizeBytes, expiresAt)
	if err != nil {
		return "", err
	}
	return g.sign(key, opts)
}

// SignedDownload returns a V4 signed GET URL that forces an attachment
// download under the original filename. The filename is quoted and escaped so
// a crafted name cannot inject extra response headers.
func (g *GCS) SignedDownload(_ context.Context, key, filename string, ttl time.Duration) (string, error) {
	disposition := "attachment"
	if filename != "" {
		disposition = mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(filename)})
		if disposition == "" {
			disposition = "attachment"
		}
	}
	return g.sign(key, &storage.SignedURLOptions{
		Scheme:  storage.SigningSchemeV4,
		Method:  "GET",
		Expires: time.Now().Add(ttl),
		QueryParameters: url.Values{
			"response-content-disposition": []string{disposition},
		},
	})
}

// sign takes no context: URL signing is a local cryptographic operation on the
// key-file path, and the IAM signBlob fallback carries its own client timeout.
func (g *GCS) sign(key string, opts *storage.SignedURLOptions) (string, error) {
	signed, err := g.client.Bucket(g.bucket).SignedURL(g.Key(key), opts)
	if err != nil {
		return "", fmt.Errorf("sign %s url for %s: %w", opts.Method, key, err)
	}
	return signed, nil
}

// Stat returns the stored object's verified metadata.
func (g *GCS) Stat(ctx context.Context, key string) (*ObjectInfo, error) {
	attrs, err := g.object(key).Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", key, err)
	}
	return &ObjectInfo{
		Key:         key,
		Size:        attrs.Size,
		ContentType: attrs.ContentType,
		Updated:     attrs.Updated,
	}, nil
}

// ReadAtMost returns at most limit bytes from the start of an object. The
// range read is what keeps a client from making the proxy buffer an object
// larger than its metadata claimed when it was finalized.
func (g *GCS) ReadAtMost(ctx context.Context, key string, limit int64) ([]byte, error) {
	reader, err := g.object(key).NewRangeReader(ctx, 0, limit)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	return data, nil
}

// Read returns the whole object.
func (g *GCS) Read(ctx context.Context, key string) ([]byte, error) {
	reader, err := g.object(key).NewReader(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	return data, nil
}

// Delete removes an object, treating an already-absent object as success so
// the retention sweeper can be re-run safely.
func (g *GCS) Delete(ctx context.Context, key string) error {
	err := g.object(key).Delete(ctx)
	if err == nil || errors.Is(err, storage.ErrObjectNotExist) {
		return nil
	}
	return fmt.Errorf("delete %s: %w", key, err)
}

// Key returns the fully-qualified object name for a logical key.
func (g *GCS) Key(key string) string {
	if g.prefix == "" {
		return key
	}
	return g.prefix + "/" + key
}

func (g *GCS) object(key string) *storage.ObjectHandle {
	return g.client.Bucket(g.bucket).Object(g.Key(key))
}
