package id_test

import (
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/earlgrey/piproxy/internal/id"
)

func TestNew(t *testing.T) {
	id1 := id.New("key")
	id2 := id.New("key")
	id3 := id.New("usr")

	if !strings.HasPrefix(id1, "key_") {
		t.Errorf("expected prefix 'key_', got %q", id1)
	}
	if !strings.HasPrefix(id3, "usr_") {
		t.Errorf("expected prefix 'usr_', got %q", id3)
	}
	if len(id1) != 4+ulid.EncodedSize {
		t.Errorf("expected length %d, got %d (%q)", 4+ulid.EncodedSize, len(id1), id1)
	}
	if id1 == id2 {
		t.Errorf("expected unique IDs, got same: %s", id1)
	}
}

func TestNewNoPrefix(t *testing.T) {
	id1 := id.New("")
	if len(id1) != ulid.EncodedSize {
		t.Errorf("expected length %d for unprefixed ULID, got %d (%q)", ulid.EncodedSize, len(id1), id1)
	}
}

func TestValidPrefix(t *testing.T) {
	valid := id.New("key")
	if !id.ValidPrefix(valid) {
		t.Errorf("expected %q to be valid", valid)
	}
	if id.ValidPrefix("invalid") {
		t.Errorf("expected 'invalid' to be invalid")
	}
	if id.ValidPrefix("key_short") {
		t.Errorf("expected 'key_short' to be invalid")
	}
}

func TestPrefix(t *testing.T) {
	if p := id.Prefix(id.New("key")); p != "key" {
		t.Errorf("expected 'key', got %q", p)
	}
	if p := id.Prefix(id.New("")); p != "" {
		t.Errorf("expected '', got %q", p)
	}
	if p := id.Prefix("no_underscore"); p != "no" {
		t.Errorf("expected 'no', got %q", p)
	}
}

func TestMonotonicity(t *testing.T) {
	ids := make([]string, 100)
	for i := range ids {
		ids[i] = id.New("seq")
	}
	seen := make(map[string]bool)
	for _, s := range ids {
		if seen[s] {
			t.Errorf("duplicate ID: %s", s)
		}
		seen[s] = true
	}
	if len(seen) != len(ids) {
		t.Errorf("expected %d unique IDs, got %d", len(ids), len(seen))
	}
}
