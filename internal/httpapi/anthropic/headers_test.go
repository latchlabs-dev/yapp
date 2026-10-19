package anthropic

import (
	"net/http"
	"testing"
)

func TestForwardResponseHeaders(t *testing.T) {
	src := http.Header{}
	src.Set("Content-Type", "application/json")
	src.Add("Set-Cookie", "session=abc")
	src.Set("Content-Length", "12345")
	src.Set("Content-Encoding", "gzip")
	src.Set("Transfer-Encoding", "chunked")
	src.Set("Connection", "keep-alive")
	src.Set("Keep-Alive", "timeout=5")
	src.Set("Proxy-Authenticate", "Basic")
	src.Set("Upgrade", "h2c")
	src.Set("X-Upstream-Internal", "leak-me")
	src.Add("Retry-After", "30")

	dst := http.Header{}
	forwardResponseHeaders(dst, src)

	if got := dst.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := dst.Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want 30", got)
	}
	for _, blocked := range []string{
		"Set-Cookie", "Content-Length", "Content-Encoding",
		"Transfer-Encoding", "Connection", "Keep-Alive",
		"Proxy-Authenticate", "Upgrade", "X-Upstream-Internal",
	} {
		if v := dst.Get(blocked); v != "" {
			t.Errorf("header %q should be dropped, got %q", blocked, v)
		}
	}
}
