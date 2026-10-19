// Package ratelimit provides IP-based rate limiting.
package ratelimit

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Store interface for rate limit counters.
type Store interface {
	Increment(ctx context.Context, key string, window time.Duration) (int64, error)
	Reset(ctx context.Context, key string) error
}

// MemoryStore is an in-memory rate limit store for development.
type MemoryStore struct {
	mu        sync.Mutex
	counters  map[string]*counter
	lastSweep time.Time
}

type counter struct {
	count       int64
	windowStart time.Time
}

// NewMemoryStore creates a new in-memory rate limit store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		counters: make(map[string]*counter),
	}
}

// Increment increments the counter for a key and returns the new count.
func (s *MemoryStore) Increment(_ context.Context, key string, window time.Duration) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.sweepLocked(now, window)
	c, ok := s.counters[key]
	if !ok || now.Sub(c.windowStart) > window {
		c = &counter{
			count:       0,
			windowStart: now,
		}
		s.counters[key] = c
	}

	c.count++
	return c.count, nil
}

// sweepLocked evicts counters whose window has expired. Without this the map
// grows unbounded — one entry per distinct client IP, forever — which is a
// memory-exhaustion DoS vector (especially if an attacker can vary the keyed
// IP). Runs at most once per window to keep Increment close to O(1) amortized.
// Caller must hold s.mu.
func (s *MemoryStore) sweepLocked(now time.Time, window time.Duration) {
	if now.Sub(s.lastSweep) < window {
		return
	}
	s.lastSweep = now
	for k, c := range s.counters {
		if now.Sub(c.windowStart) > window {
			delete(s.counters, k)
		}
	}
}

// Len returns the number of tracked counters. Intended for tests/observability.
func (s *MemoryStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.counters)
}

// Reset clears the counter for a key.
func (s *MemoryStore) Reset(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.counters, key)
	return nil
}

// Middleware provides IP-based rate limiting.
type Middleware struct {
	store  Store
	limit  int64
	window time.Duration
}

// NewMiddleware creates a new IP rate limiter middleware.
func NewMiddleware(store Store, limit int64, window time.Duration) *Middleware {
	return &Middleware{
		store:  store,
		limit:  limit,
		window: window,
	}
}

// Wrap wraps a handler with rate limiting.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m == nil || m.limit <= 0 {
			next.ServeHTTP(w, r)
			return
		}

		// Skip rate limiting for health/metrics endpoints.
		path := r.URL.Path
		if path == "/healthz" || path == "/readyz" || path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		ip := extractIP(r)
		if ip == "" {
			next.ServeHTTP(w, r)
			return
		}

		count, err := m.store.Increment(r.Context(), "ratelimit:"+ip, m.window)
		if err != nil {
			// Store error, allow the request.
			next.ServeHTTP(w, r)
			return
		}

		if count > m.limit {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", fmt.Sprintf("%.0f", m.window.Seconds()))
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"rate_limited","message":"too many requests from this IP"}}`))
			return
		}

		next.ServeHTTP(w, r)
	})
}

// ClientIP returns the source IP the proxy attributes to the request, using the
// same trusted-proxy resolution as rate limiting (X-Real-IP / rightmost
// X-Forwarded-For only behind a trusted peer; never a spoofable client value).
// It returns "" when no valid IP can be determined, so callers can store it
// directly into an inet column without risking a parse failure.
func ClientIP(r *http.Request) string {
	ip := extractIP(r)
	if net.ParseIP(ip) == nil {
		return ""
	}
	return ip
}

func extractIP(r *http.Request) string {
	remoteIP := remoteAddrIP(r.RemoteAddr)
	// Only honor forwarding headers when the immediate peer is a trusted proxy
	// (loopback/private). A direct public client cannot forge its source IP.
	if !isTrustedProxyIP(remoteIP) {
		return remoteIP
	}
	// Behind a trusted single-hop proxy (our nginx). Trust only the address the
	// proxy itself observed — never a client-supplied value. Prefer X-Real-IP
	// (nginx sets it to the real peer and overwrites any client value); else
	// take the RIGHTMOST X-Forwarded-For entry. An attacker can prepend XFF
	// entries but cannot control the rightmost hop the proxy appends, so the
	// leftmost-entry approach (which they could spoof to rotate rate-limit
	// buckets) must not be used.
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		if ip := net.ParseIP(xr); ip != nil {
			return ip.String()
		}
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if ip := rightmostIP(xff); ip != "" {
			return ip
		}
	}
	return remoteIP
}

func remoteAddrIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if ip := net.ParseIP(strings.TrimSpace(host)); ip != nil {
		return ip.String()
	}
	return host
}

// rightmostIP returns the last valid IP in an X-Forwarded-For list — the hop
// our trusted proxy observed and appended. Walking from the right defeats
// client-prepended spoof entries.
func rightmostIP(xff string) string {
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		if ip := net.ParseIP(strings.TrimSpace(parts[i])); ip != nil {
			return ip.String()
		}
	}
	return ""
}

func isTrustedProxyIP(ip string) bool {
	parsed := net.ParseIP(ip)
	return parsed != nil && (parsed.IsLoopback() || parsed.IsPrivate())
}
