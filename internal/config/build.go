// Package config
package config

import (
	"runtime"
)

// BuildInfo returns a map of build metadata.
func BuildInfo() map[string]string {
	return map[string]string{
		"version":    Version,
		"commit":     Commit,
		"build_date": BuildDate,
		"go_version": runtime.Version(),
		"go_os":      runtime.GOOS,
		"go_arch":    runtime.GOARCH,
	}
}
