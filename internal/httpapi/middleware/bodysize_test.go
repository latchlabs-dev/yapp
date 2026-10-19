package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
)

// TestBodySize_ClampsNonPositive proves a non-positive configured limit does
// NOT disable the cap: it is clamped to DefaultMaxBodyBytes, so a body larger
// than that default is still rejected.
func TestBodySize_ClampsNonPositive(t *testing.T) {
	for _, maxBytes := range []int64{0, -1} {
		handler := middleware.BodySize(maxBytes)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Read enough to trigger MaxBytesReader's limit error.
			buf := make([]byte, 4096)
			for {
				_, err := r.Body.Read(buf)
				if err != nil {
					if strings.Contains(err.Error(), "request body too large") {
						w.WriteHeader(http.StatusRequestEntityTooLarge)
						return
					}
					break
				}
			}
			w.WriteHeader(http.StatusOK)
		}))

		// Body strictly larger than the clamped default.
		big := strings.NewReader(strings.Repeat("a", int(middleware.DefaultMaxBodyBytes)+1))
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/test", big)
		handler.ServeHTTP(w, r)

		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("maxBytes=%d: expected 413 (limit not disabled), got %d", maxBytes, w.Code)
		}
	}
}

// TestBodySize_PositivePassThrough confirms the clamp does not alter an
// explicitly configured positive limit.
func TestBodySize_PositivePassThrough(t *testing.T) {
	handler := middleware.BodySize(10)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 100)
		_, err := r.Body.Read(buf)
		if err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	// Small body passes.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("small")))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for small body, got %d", w.Code)
	}

	// Body over the explicit 10-byte limit fails (proves clamp left 10 intact).
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("this body is way too long for the limit")))
	if w2.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for large body, got %d", w2.Code)
	}
}
