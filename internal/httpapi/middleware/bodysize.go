package middleware

import (
	"net/http"
)

// DefaultMaxBodyBytes is the shared default request-body byte cap used as the
// floor for the BodySize middleware clamp. It matches the config default
// (config.go PROXY_SERVER_MAX_BODY_BYTES default:"20971520"; 20*1024*1024), the
// policy default (policy.go LimitMaxBodyBytes: 20*1024*1024), and the
// per-provider default (catalog.go defaultProviderBodySize = 20*1024*1024).
const DefaultMaxBodyBytes int64 = 20 * 1024 * 1024

// BodySize limits the maximum request body size, clamping any non-positive
// configured value up to DefaultMaxBodyBytes so an unset/zero/negative
// maxBytes never disables the limit (http.MaxBytesReader treats n <= 0 as
// "no limit", which would silently remove the cap for all provider routes).
func BodySize(maxBytes int64) func(http.Handler) http.Handler {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBodyBytes
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}
