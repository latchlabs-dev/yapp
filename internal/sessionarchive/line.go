package sessionarchive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// hashSize is the stored length of each chained item hash. 128 bits keeps
// accidental collisions out of reach; a deliberate collision would only
// mis-render its author's own transcript, never another tenant's.
const hashSize = 16

// extendChain appends the chained hashes of items to chain. Each hash commits
// to the item and every item before it, so two lines share a prefix of length
// i exactly when their i-th hashes are equal.
func extendChain(chain []byte, items []json.RawMessage) []byte {
	out := make([]byte, len(chain), len(chain)+hashSize*len(items))
	copy(out, chain)
	h := sha256.New()
	var sum [sha256.Size]byte
	for _, item := range items {
		h.Reset()
		if len(out) >= hashSize {
			h.Write(out[len(out)-hashSize:])
		}
		h.Write(item)
		out = append(out, h.Sum(sum[:0])[:hashSize]...)
	}
	return out
}

// commonPrefixItems returns the number of leading items two chains share.
func commonPrefixItems(a, b []byte) int {
	n := min(len(a), len(b)) / hashSize
	for i := 0; i < n; i++ {
		lo, hi := i*hashSize, (i+1)*hashSize
		if !bytes.Equal(a[lo:hi], b[lo:hi]) {
			return i
		}
	}
	return n
}

// systemDigest fingerprints system text; empty text has an empty digest so a
// conversation without a system prompt never records a "change".
func systemDigest(text string) string {
	if text == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
