package blob

import (
	"context"
	"errors"
	"testing"
)

func TestMemoryObjectsRoundTripAndIsolation(t *testing.T) {
	ctx := context.Background()
	var objects Objects = NewMemory()

	data := []byte("hello")
	if err := objects.Put(ctx, "a/b", data, "text/plain"); err != nil {
		t.Fatal(err)
	}
	data[0] = 'j' // caller mutation must not reach the store

	got, err := objects.Read(ctx, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("read %q, want hello", got)
	}
	got[0] = 'y' // reader mutation must not reach the store either
	again, _ := objects.Read(ctx, "a/b")
	if string(again) != "hello" {
		t.Fatalf("store mutated through read: %q", again)
	}

	if err := objects.Delete(ctx, "a/b"); err != nil {
		t.Fatal(err)
	}
	if err := objects.Delete(ctx, "a/b"); err != nil {
		t.Fatalf("deleting an absent object must succeed: %v", err)
	}
	if _, err := objects.Read(ctx, "a/b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read after delete err = %v, want ErrNotFound", err)
	}
}
