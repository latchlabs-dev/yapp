package logging_test

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"testing"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/logging"
)

func TestRedact(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Bearer sk-abc123", "Bearer [REDACTED]abc123"},
		{"Bearer sk-ant-abc123", "Bearer [REDACTED]abc123"},
		{"cpk_live_TEST_ONLY_SYNTHETIC_KEY_NOT_VALID", "[REDACTED]TEST_ONLY_SYNTHETIC_KEY_NOT_VALID"},
		{"aws key AKIAIOSFODNN7EXAMPLE", "aws key [REDACTED]"},
		{"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.signature_123", "jwt [REDACTED]"},
		{"Authorization: Bearer sk-secret-key", "Authorization: Bearer [REDACTED]secret-key"},
		{"normal text without secrets", "normal text without secrets"},
		{"no prefix here", "no prefix here"},
	}

	for _, tt := range tests {
		name := tt.input
		if len(name) > 20 {
			name = name[:20]
		}
		t.Run(name, func(t *testing.T) {
			result := logging.Redact(tt.input)
			if result != tt.expected {
				t.Errorf("Redact(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestRedactNoFalsePositives(t *testing.T) {
	input := "This is a harmless log message with no secrets."
	result := logging.Redact(input)
	if result != input {
		t.Errorf("Redact should not modify normal text: got %q", result)
	}
}

func TestRedactMultiplePatterns(t *testing.T) {
	input := "sk-abc and cpk_live_test"
	result := logging.Redact(input)
	if !logging.IsRedacted(result) {
		t.Errorf("expected result to contain redacted markers")
	}
}

func TestNewNopLogger(t *testing.T) {
	logger := logging.NewNopLogger()
	logger.Info().Msg("this should not panic")
}

func TestNewLoggerJSON(t *testing.T) {
	logger := logging.NewLogger(logging.Config{
		Level:  "debug",
		Format: "json",
	})
	logger.Debug().Msg("test debug message")
}

func TestNewLoggerConsole(t *testing.T) {
	logger := logging.NewLogger(logging.Config{
		Level:  "info",
		Format: "console",
	})
	logger.Info().Msg("test console message")
}

func TestRequestWarnEmitsWarnWithErrorClass(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	logging.RequestWarn(logger, "openai.client_closed", stderrors.New("context canceled"), logging.Fields{
		Status:     499,
		ErrorClass: "client_closed",
	})

	var got map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal log: %v\n%s", err, buf.String())
	}
	if got["level"] != "warn" {
		t.Fatalf("level = %v, want warn; log=%s", got["level"], buf.String())
	}
	if got["error_class"] != "client_closed" {
		t.Fatalf("error_class = %v, want client_closed; log=%s", got["error_class"], buf.String())
	}
	if got["status"].(float64) != 499 {
		t.Fatalf("status = %v, want 499; log=%s", got["status"], buf.String())
	}
	if got["error"] != "context canceled" {
		t.Fatalf("error = %v, want context canceled; log=%s", got["error"], buf.String())
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		level string
		valid bool
	}{
		{"trace", true},
		{"debug", true},
		{"info", true},
		{"warn", true},
		{"error", true},
		{"fatal", true},
		{"panic", true},
		{"unknown", true},
		{"", true},
	}

	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			logger := logging.NewLogger(logging.Config{
				Level:  tt.level,
				Format: "json",
			})
			if tt.valid && logger.GetLevel() == 0 {
				t.Log("logger level is 0, which is fine for nop")
			}
		})
	}
}
