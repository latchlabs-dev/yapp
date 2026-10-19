package upstream

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Timeout <= 0 {
		t.Fatalf("expected positive timeout, got %s", cfg.Timeout)
	}
	if cfg.MaxConnsPerHost <= 0 {
		t.Fatalf("expected positive MaxConnsPerHost, got %d", cfg.MaxConnsPerHost)
	}
	if cfg.MaxIdleConns <= 0 {
		t.Fatalf("expected positive MaxIdleConns, got %d", cfg.MaxIdleConns)
	}
}

func TestNewClientAppliesConfig(t *testing.T) {
	cfg := Config{
		Timeout:         123 * time.Millisecond,
		MaxConnsPerHost: 7,
		MaxIdleConns:    10,
	}

	client := NewClient(cfg)
	if client == nil || client.client == nil {
		t.Fatal("expected initialized client")
	}
	if client.client.Timeout != cfg.Timeout {
		t.Fatalf("expected timeout %s, got %s", cfg.Timeout, client.client.Timeout)
	}

	transport, ok := client.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", client.client.Transport)
	}
	if transport.MaxConnsPerHost != cfg.MaxConnsPerHost {
		t.Fatalf("expected MaxConnsPerHost %d, got %d", cfg.MaxConnsPerHost, transport.MaxConnsPerHost)
	}
	if transport.MaxIdleConns != cfg.MaxIdleConns {
		t.Fatalf("expected MaxIdleConns %d, got %d", cfg.MaxIdleConns, transport.MaxIdleConns)
	}
	if transport.MaxIdleConnsPerHost != cfg.MaxIdleConns/2 {
		t.Fatalf("expected MaxIdleConnsPerHost %d, got %d", cfg.MaxIdleConns/2, transport.MaxIdleConnsPerHost)
	}
}

func TestDoForwardRejectsInvalidRequest(t *testing.T) {
	client := NewClient(DefaultConfig())

	resp, err := client.DoForward(context.Background(), http.MethodGet, "://bad-url", nil, strings.NewReader("{}"))
	if err == nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		t.Fatal("expected invalid upstream request error")
	}
	if !strings.Contains(err.Error(), "create upstream request") {
		t.Fatalf("expected create request error, got %v", err)
	}
}

func TestCloseIdleConnectionsIsSafe(t *testing.T) {
	client := NewClient(DefaultConfig())
	client.CloseIdleConnections()
}
