// Package blob stores web console attachments in object storage. Attachment
// bytes never pass through the proxy: the browser uploads and downloads them
// directly against signed URLs, and the proxy only reads an object back when it
// has to build a model payload from it.
package blob

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when an object does not exist.
var ErrNotFound = errors.New("object not found")

// Objects is the server-side object surface the session archive needs: raw
// capture bodies, derived transcripts and Pi session exports. Unlike
// attachments, these bytes are written and read only by the proxy and admin
// processes, never by a browser.
type Objects interface {
	// Put stores data under key, replacing any existing object.
	Put(ctx context.Context, key string, data []byte, contentType string) error

	// Read returns the whole object, or ErrNotFound.
	Read(ctx context.Context, key string) ([]byte, error)

	// Delete removes an object; deleting an absent object is not an error.
	Delete(ctx context.Context, key string) error

	// Location identifies the backend (for example "postgres" or
	// "gcs://bucket/prefix") so stored metadata records where bodies live.
	Location() string
}

// ObjectInfo is the metadata the console verifies after an upload completes.
// Size and ContentType come from the stored object, never from what the client
// claimed when it asked for the upload URL.
type ObjectInfo struct {
	Key         string
	Size        int64
	ContentType string
	Updated     time.Time
}

// Store is the object-storage surface the web console needs.
type Store interface {
	// Bucket names the backing bucket, recorded alongside each attachment so
	// stored objects stay resolvable if the configured bucket later changes.
	Bucket() string

	// SignedUpload binds a create-only PUT to its content type, exact reserved
	// size, and expiry. Callers must send UploadHeaders with the request.
	SignedUpload(ctx context.Context, key, contentType string, sizeBytes int64, expiresAt time.Time) (string, error)

	// SignedDownload returns a URL the browser may GET, with a
	// Content-Disposition that forces a download under the original filename.
	SignedDownload(ctx context.Context, key, filename string, ttl time.Duration) (string, error)

	// Stat returns the stored object's metadata, or ErrNotFound.
	Stat(ctx context.Context, key string) (*ObjectInfo, error)

	// ReadAtMost returns up to limit bytes from the start of an object. It
	// bounds how much a caller can be made to buffer: attachment objects are
	// client-written, so their metadata may understate what a later read finds.
	ReadAtMost(ctx context.Context, key string, limit int64) ([]byte, error)

	// Read returns the whole object. Callers must have already checked Stat
	// against their size cap.
	Read(ctx context.Context, key string) ([]byte, error)

	// Delete removes an object. Deleting an absent object is not an error, so
	// the retention sweeper is idempotent.
	Delete(ctx context.Context, key string) error

	// Close releases the underlying client.
	Close() error
}
