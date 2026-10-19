package dataplane_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
)

func TestReadUpstreamBodyWithinLimit(t *testing.T) {
	body, err := dataplane.ReadUpstreamBody(strings.NewReader("hello"), 5)
	if err != nil {
		t.Fatalf("ReadUpstreamBody failed: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("expected hello, got %q", string(body))
	}
}

func TestReadUpstreamBodyTooLarge(t *testing.T) {
	_, err := dataplane.ReadUpstreamBody(strings.NewReader("hello!"), 5)
	if err == nil {
		t.Fatal("expected size error")
	}
	if !errors.Is(err, dataplane.ErrUpstreamResponseTooLarge) {
		t.Fatalf("expected ErrUpstreamResponseTooLarge, got %v", err)
	}
	if !dataplane.IsUpstreamResponseTooLarge(err) {
		t.Fatal("expected IsUpstreamResponseTooLarge to return true")
	}
}
