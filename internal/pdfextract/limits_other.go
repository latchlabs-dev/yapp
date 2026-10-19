//go:build !linux

package pdfextract

// The production container is Linux. Other kernels do not offer the same hard
// anonymous-memory guarantee through RLIMIT_DATA/AS, so fail closed instead of
// presenting a goroutine timeout or a Go soft heap target as a security bound.
func applyLimits() error { return ErrUnavailable }
