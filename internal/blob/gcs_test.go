package blob

import (
	"crypto/sha256"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
)

func TestSignedUploadBindsStorageAdmission(t *testing.T) {
	expires := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	opts, err := uploadOptions("text/plain", 123, expires)
	if err != nil {
		t.Fatal(err)
	}
	opts.GoogleAccessID = "test@example.invalid"
	opts.SignBytes = func(input []byte) ([]byte, error) { hash := sha256.Sum256(input); return hash[:], nil }
	signed, err := storage.SignedURL("test-bucket", "tenant/file", opts)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"content-type", "x-goog-content-length-range", "x-goog-if-generation-match"} {
		if !strings.Contains(parsed.Query().Get("X-Goog-SignedHeaders"), header) {
			t.Fatalf("unsigned upload constraint: %s", header)
		}
	}
	headers := UploadHeaders("text/plain", 123)
	if headers["X-Goog-Content-Length-Range"] != "123,123" || headers["X-Goog-If-Generation-Match"] != "0" {
		t.Fatalf("headers do not enforce exact reservation and create-only upload: %v", headers)
	}
	if opts.Expires != expires || opts.Method != "PUT" {
		t.Fatal("upload authority differs from reservation")
	}
	// An attacker changing the reservation range must also forge the signature.
	opts.Headers[0] = "x-goog-content-length-range:123,999999"
	changed, err := storage.SignedURL("test-bucket", "tenant/file", opts)
	if err != nil {
		t.Fatal(err)
	}
	if changed == signed {
		t.Fatal("changing the storage byte limit left the signature valid")
	}
}

func TestUploadOptionsRejectUnboundedAuthority(t *testing.T) {
	for _, input := range []struct {
		size   int64
		expiry time.Time
	}{
		{0, time.Now().Add(time.Minute)}, {-1, time.Now().Add(time.Minute)},
		{1, time.Now().Add(-time.Minute)}, {1, time.Now().Add(8 * 24 * time.Hour)},
	} {
		if _, err := uploadOptions("text/plain", input.size, input.expiry); err == nil {
			t.Fatalf("accepted size=%d expiry=%v", input.size, input.expiry)
		}
	}
}
