package dataplane

import (
	"errors"
	"fmt"
	"io"
)

const (
	// DefaultMaxUpstreamResponseBytes caps buffered non-streaming upstream
	// responses so a bad provider response cannot exhaust proxy memory.
	DefaultMaxUpstreamResponseBytes int64 = 64 * 1024 * 1024
)

// ErrUpstreamResponseTooLarge is returned when a buffered upstream response
// exceeds the configured cap.
var ErrUpstreamResponseTooLarge = errors.New("upstream response too large")

// ReadUpstreamBody reads an upstream response with a hard byte limit.
func ReadUpstreamBody(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = DefaultMaxUpstreamResponseBytes
	}

	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w: limit %d bytes", ErrUpstreamResponseTooLarge, limit)
	}
	return body, nil
}

// IsUpstreamResponseTooLarge reports whether err is an upstream body-size error.
func IsUpstreamResponseTooLarge(err error) bool {
	return errors.Is(err, ErrUpstreamResponseTooLarge)
}
