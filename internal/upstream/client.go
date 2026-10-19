// Package upstream provides a reusable HTTP client for forwarding requests to LLM providers.
package upstream

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Config holds configuration for the upstream HTTP client.
type Config struct {
	Timeout         time.Duration
	MaxConnsPerHost int
	MaxIdleConns    int
}

// DefaultConfig returns a sensible default configuration.
func DefaultConfig() Config {
	return Config{
		Timeout:         10 * time.Minute,
		MaxConnsPerHost: 100,
		MaxIdleConns:    100,
	}
}

// Client is a reusable HTTP client for forwarding requests to LLM providers.
type Client struct {
	client *http.Client
}

// NewClient creates a new upstream HTTP client with the given config.
func NewClient(cfg Config) *Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          cfg.MaxIdleConns,
		MaxConnsPerHost:       cfg.MaxConnsPerHost,
		MaxIdleConnsPerHost:   cfg.MaxIdleConns / 2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	return &Client{
		client: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
		},
	}
}

// DoForward forwards an HTTP request to an upstream provider.
func (c *Client) DoForward(ctx context.Context, method, url string, headers http.Header, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("create upstream request: %w", err)
	}

	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream request failed: %w", err)
	}

	return resp, nil
}

// DoForwardAndStream forwards a request and returns the response for streaming.
// The caller is responsible for closing resp.Body and managing timeouts.
func (c *Client) DoForwardAndStream(ctx context.Context, method, url string, headers http.Header, body io.Reader) (*http.Response, error) {
	return c.DoForward(ctx, method, url, headers, body)
}

// CloseIdleConnections closes idle connections in the transport.
func (c *Client) CloseIdleConnections() {
	c.client.CloseIdleConnections()
}
