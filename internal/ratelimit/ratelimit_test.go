package ratelimit_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/ratelimit"
)

func TestMemoryStore(t *testing.T) {
	store := ratelimit.NewMemoryStore()
	ctx := context.Background()

	count, err := store.Increment(ctx, "test-key", time.Minute)
	if err != nil {
		t.Fatalf("Increment failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1, got %d", count)
	}

	count, err = store.Increment(ctx, "test-key", time.Minute)
	if err != nil {
		t.Fatalf("Increment failed: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2, got %d", count)
	}

	if err := store.Reset(ctx, "test-key"); err != nil {
		t.Fatalf("Reset failed: %v", err)
	}

	count, err = store.Increment(ctx, "test-key", time.Minute)
	if err != nil {
		t.Fatalf("Increment failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 after reset, got %d", count)
	}
}

func TestMemoryStoreExpiry(t *testing.T) {
	store := ratelimit.NewMemoryStore()
	ctx := context.Background()

	// Use a very short window.
	count, err := store.Increment(ctx, "expire-key", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("Increment failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1, got %d", count)
	}

	time.Sleep(20 * time.Millisecond)

	// Should reset after expiry.
	count, err = store.Increment(ctx, "expire-key", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("Increment failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 after expiry, got %d", count)
	}
}

func TestRateLimitMiddleware(t *testing.T) {
	store := ratelimit.NewMemoryStore()
	mw := ratelimit.NewMiddleware(store, 2, time.Minute)

	handler := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First two requests should pass.
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/test", nil)
		r.RemoteAddr = "192.168.1.1:12345"
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("request %d: expected 200, got %d", i+1, w.Code)
		}
	}

	// Third should be rate limited.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	r.RemoteAddr = "192.168.1.1:12345"
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", w.Code)
	}
}

func TestRateLimitSkipsHealthEndpoint(t *testing.T) {
	store := ratelimit.NewMemoryStore()
	mw := ratelimit.NewMiddleware(store, 1, time.Minute)

	handler := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/healthz", nil)
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for healthz, got %d", w.Code)
	}
}

func TestRateLimitDisabledWhenLimitNonPositive(t *testing.T) {
	store := ratelimit.NewMemoryStore()
	mw := ratelimit.NewMiddleware(store, 0, time.Minute)

	handler := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/test", nil)
		r.RemoteAddr = "192.168.1.1:12345"
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected disabled limiter to pass, got %d", i+1, w.Code)
		}
	}
}

func TestExtractIP(t *testing.T) {
	tests := []struct {
		remoteAddr string
		xff        string
		expected   string
	}{
		{"192.168.1.1:12345", "", "192.168.1.1"},
		{"[::1]:8080", "", "::1"},
		{"10.0.0.1:9999", "203.0.113.5", "203.0.113.5"},
	}

	for _, tt := range tests {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tt.remoteAddr
		if tt.xff != "" {
			r.Header.Set("X-Forwarded-For", tt.xff)
		}

		// Test via middleware.
		store := ratelimit.NewMemoryStore()
		mw := ratelimit.NewMiddleware(store, 100, time.Minute)
		handler := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("expected 200 for %s, got %d", tt.remoteAddr, w.Code)
		}
	}
}

func TestRateLimitIgnoresSpoofedXForwardedForFromUntrustedRemote(t *testing.T) {
	store := ratelimit.NewMemoryStore()
	mw := ratelimit.NewMiddleware(store, 1, time.Minute)
	handler := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	first := httptest.NewRequest("GET", "/", nil)
	first.RemoteAddr = "198.51.100.10:12345"
	first.Header.Set("X-Forwarded-For", "203.0.113.5")
	firstRec := httptest.NewRecorder()
	handler.ServeHTTP(firstRec, first)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("expected first request to pass, got %d", firstRec.Code)
	}

	second := httptest.NewRequest("GET", "/", nil)
	second.RemoteAddr = "198.51.100.10:12345"
	second.Header.Set("X-Forwarded-For", "203.0.113.6")
	secondRec := httptest.NewRecorder()
	handler.ServeHTTP(secondRec, second)
	if secondRec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected spoofed XFF to be ignored and remote IP limited, got %d", secondRec.Code)
	}
}

func TestRateLimitUsesXForwardedForFromTrustedProxy(t *testing.T) {
	store := ratelimit.NewMemoryStore()
	mw := ratelimit.NewMiddleware(store, 1, time.Minute)
	handler := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, xff := range []string{"203.0.113.5", "203.0.113.6"} {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "10.0.0.1:12345"
		req.Header.Set("X-Forwarded-For", xff)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected trusted proxy XFF %s to get separate bucket, got %d", xff, rec.Code)
		}
	}
}

// A client behind our trusted proxy cannot evade the per-IP limiter by rotating
// a *prepended* X-Forwarded-For entry: the limiter must key on the rightmost
// (proxy-observed) hop, which stays constant. Regression for the XFF-spoof
// bypass.
func TestRateLimitRejectsSpoofedLeftmostXForwardedFor(t *testing.T) {
	store := ratelimit.NewMemoryStore()
	mw := ratelimit.NewMiddleware(store, 1, time.Minute)
	handler := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Same real client (rightmost = 203.0.113.7), attacker rotates leftmost.
	codes := make([]int, 0, 2)
	for _, spoof := range []string{"9.9.9.1", "9.9.9.2"} {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "127.0.0.1:12345" // trusted (loopback nginx)
		req.Header.Set("X-Forwarded-For", spoof+", 203.0.113.7")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
	}
	if codes[0] != http.StatusOK || codes[1] != http.StatusTooManyRequests {
		t.Fatalf("rotating leftmost XFF bypassed limiter: got %v, want [200 429]", codes)
	}
}

// X-Real-IP (set by our nginx to the real peer) is preferred and a rotated XFF
// cannot create fresh buckets when X-Real-IP is constant.
func TestRateLimitPrefersXRealIP(t *testing.T) {
	store := ratelimit.NewMemoryStore()
	mw := ratelimit.NewMiddleware(store, 1, time.Minute)
	handler := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	codes := make([]int, 0, 2)
	for _, spoof := range []string{"9.9.9.1", "9.9.9.2"} {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Real-IP", "203.0.113.8")
		req.Header.Set("X-Forwarded-For", spoof)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
	}
	if codes[0] != http.StatusOK || codes[1] != http.StatusTooManyRequests {
		t.Fatalf("X-Real-IP not honored: got %v, want [200 429]", codes)
	}
}

// MemoryStore must evict expired counters rather than grow unboundedly.
func TestMemoryStoreEvictsExpired(t *testing.T) {
	store := ratelimit.NewMemoryStore()
	ctx := context.Background()
	window := 20 * time.Millisecond
	for i := 0; i < 100; i++ {
		if _, err := store.Increment(ctx, fmt.Sprintf("ratelimit:10.0.0.%d", i), window); err != nil {
			t.Fatalf("increment: %v", err)
		}
	}
	time.Sleep(2 * window)
	// A later increment for one key triggers a sweep of the now-expired entries.
	if _, err := store.Increment(ctx, "ratelimit:10.0.0.0", window); err != nil {
		t.Fatalf("increment after expiry: %v", err)
	}
	if n := store.Len(); n != 1 {
		t.Fatalf("expected stale counters evicted to 1, got %d", n)
	}
}
