// Package logging provides structured JSON logging with secret redaction.
package logging

import (
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// RedactPatterns contains strings that must never appear in logs.
// These are substring patterns checked against any value that might contain
// sensitive data. Patterns should avoid false positives on common words.
var RedactPatterns = []string{
	"sk-ant-",
	"sk-",
	"cpk_live_",
}

var redactRegexps = []*regexp.Regexp{
	regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`),
}

// Config holds logging configuration.
type Config struct {
	Level     string
	Format    string // "json" or "console"
	LogBodies bool
}

// NewLogger creates a new zerolog logger from config.
func NewLogger(cfg Config) zerolog.Logger {
	var output io.Writer = os.Stdout

	if cfg.Format == "console" {
		output = zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: time.RFC3339,
		}
	}

	level := parseLevel(cfg.Level)
	logger := zerolog.New(output).
		Level(level).
		With().
		Timestamp().
		Logger()

	return logger
}

// NewNopLogger returns a no-op logger for testing.
func NewNopLogger() zerolog.Logger {
	return zerolog.Nop()
}

// Redact returns a copy of the string with sensitive patterns replaced.
// This is applied to any log field that might contain user data.
func Redact(s string) string {
	result := s
	for _, re := range redactRegexps {
		result = re.ReplaceAllString(result, "[REDACTED]")
	}
	for _, pattern := range RedactPatterns {
		result = strings.ReplaceAll(result, pattern, "[REDACTED]")
	}
	return result
}

// IsRedacted checks if a string contains any redacted marker.
func IsRedacted(s string) bool {
	return strings.Contains(s, "[REDACTED]")
}

func parseLevel(level string) zerolog.Level {
	switch strings.ToLower(level) {
	case "trace":
		return zerolog.TraceLevel
	case "debug":
		return zerolog.DebugLevel
	case "info":
		return zerolog.InfoLevel
	case "warn":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	case "fatal":
		return zerolog.FatalLevel
	case "panic":
		return zerolog.PanicLevel
	default:
		return zerolog.InfoLevel
	}
}
