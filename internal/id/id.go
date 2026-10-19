// Package id provides prefixed unique identifier generation using ULID.
package id

import (
	crand "crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

var (
	entropyMu sync.Mutex
	entropy   *ulid.MonotonicEntropy
)

func init() {
	// Seed from crypto/rand so the per-ms random component is cryptographically
	// unpredictable (IDs may be used as unguessable handles, not just sortable
	// keys). MonotonicEntropy ensures increasing ULIDs within the same ms.
	entropy = ulid.Monotonic(crand.Reader, 0)
}

// New generates a new prefixed ULID.
// Format: <prefix>_<26-char ULID> or just the ULID if prefix is empty.
// Example: id.New("key") → "key_01JZ8X6WZCX8A9S0M8BMZ7TD9Q"
func New(prefix string) string {
	entropyMu.Lock()
	id := ulid.MustNew(ulid.Timestamp(time.Now()), entropy)
	entropyMu.Unlock()

	str := id.String()
	if prefix != "" {
		return prefix + "_" + str
	}
	return str
}

// ValidPrefix checks if a string looks like a valid prefixed ID.
func ValidPrefix(s string) bool {
	if len(s) < 27 {
		return false
	}
	sep := -1
	for i, ch := range s {
		if ch == '_' {
			sep = i
			break
		}
	}
	if sep <= 0 || sep >= len(s)-ulid.EncodedSize {
		return false
	}
	ulidPart := s[sep+1:]
	if len(ulidPart) != ulid.EncodedSize {
		return false
	}
	_, err := ulid.Parse(ulidPart)
	return err == nil
}

// Prefix extracts the prefix from a prefixed ID.
func Prefix(s string) string {
	for i, ch := range s {
		if ch == '_' {
			return s[:i]
		}
	}
	return ""
}

// Example formats:
//
//	id.New("key") → "key_01JZ8X6WZCX8A9S0M8BMZ7TD9Q"
//	id.New("usr") → "usr_01JZ8X6WZCX8A9S0M8BMZ7TD9Q"
func Example() {
	fmt.Println(New("key"))
}
