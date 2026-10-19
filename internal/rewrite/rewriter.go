package rewrite

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/earlgrey/piproxy/internal/providers"
)

// RequestRewriter handles rewriting incoming proxy requests to upstream requests.
type RequestRewriter struct {
	registry *providers.Registry
}

// NewRequestRewriter creates a new rewriter.
func NewRequestRewriter(registry *providers.Registry) *RequestRewriter {
	return &RequestRewriter{registry: registry}
}

// Result contains the result of rewriting a request.
type Result struct {
	UpstreamURL    string
	UpstreamMethod string
	Headers        http.Header
}

// Rewrite rewrites an incoming request to an upstream request.
func (rw *RequestRewriter) Rewrite(r *http.Request, provider *providers.Provider) (*Result, error) {
	// Extract the API path from the incoming request.
	apiPath := strings.TrimPrefix(r.URL.Path, provider.PublicBasePath)
	if apiPath == "" {
		apiPath = r.URL.Path
	}
	apiPath = SanitizePath(apiPath)

	// Validate the path against the allowed paths.
	if !rw.isPathAllowed(apiPath, provider.AllowedPaths) {
		return nil, fmt.Errorf("path %s not allowed for provider %s", apiPath, provider.Type)
	}

	// Build upstream URL.
	upstreamURL, err := url.JoinPath(provider.UpstreamBaseURL, apiPath)
	if err != nil {
		return nil, fmt.Errorf("build upstream URL: %w", err)
	}
	allowedHosts := provider.AllowedHosts
	if len(allowedHosts) == 0 {
		if host := upstreamHost(provider.UpstreamBaseURL); host != "" {
			allowedHosts = []string{host}
		}
	}
	if err := ValidateEgressWithOptions(upstreamURL, allowedHosts, provider.AllowPrivateEgress); err != nil {
		return nil, err
	}

	// Preserve query parameters.
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	// Build upstream headers.
	headers := make(http.Header)
	for k, v := range r.Header {
		// Skip headers that should not be forwarded.
		if isForwardedHeader(k) {
			headers[k] = v
		}
	}

	// Add corporate headers.
	headers.Set("X-Corp-Request-ID", r.Header.Get("X-Request-ID"))

	return &Result{
		UpstreamURL:    upstreamURL,
		UpstreamMethod: r.Method,
		Headers:        headers,
	}, nil
}

func (rw *RequestRewriter) isPathAllowed(apiPath string, allowedPaths []string) bool {
	for _, allowed := range allowedPaths {
		allowsChildren := strings.HasSuffix(allowed, "/")
		cleanAllowed := SanitizePath(allowed)
		if apiPath == cleanAllowed {
			return true
		}
		if allowsChildren && strings.HasPrefix(apiPath, strings.TrimSuffix(cleanAllowed, "/")+"/") {
			return true
		}
	}
	return false
}

// isForwardedHeader checks if a header should be forwarded upstream.
func isForwardedHeader(key string) bool {
	lower := strings.ToLower(key)

	// Never forward these. Includes auth/credential material that must not
	// leak to upstreams (kept in sync with middleware.StripAuthHeaders).
	skipHeaders := map[string]bool{
		"host":                true,
		"authorization":       true,
		"x-corp-proxy-key":    true,
		"x-corp-provider-id":  true,
		"x-project-id":        true,
		"x-extension-version": true,
		"x-api-key":           true,
		"api-key":             true,
		"x-auth-token":        true,
		"cookie":              true,
		"set-cookie":          true,
		"proxy-authorization": true,
		"proxy-connection":    true,
	}

	if skipHeaders[lower] {
		return false
	}

	// Skip Hop-by-hop headers.
	hopByHop := []string{"connection", "keep-alive", "transfer-encoding", "te", "trailer", "upgrade", "proxy-authorization", "proxy-authenticate"}
	for _, h := range hopByHop {
		if lower == h {
			return false
		}
	}

	return true
}

// ValidateEgress checks that a URL's host is in the allowed egress list.
func ValidateEgress(rawURL string, allowedHosts []string) error {
	return ValidateEgressWithOptions(rawURL, allowedHosts, false)
}

// ValidateEgressWithOptions checks that a URL's host is allowed and not an
// internal/private target unless explicitly enabled for tests or local dev.
func ValidateEgressWithOptions(rawURL string, allowedHosts []string, allowPrivate bool) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}

	if parsed.Scheme != "https" && !allowPrivate {
		return fmt.Errorf("egress scheme %s not allowed", parsed.Scheme)
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return fmt.Errorf("egress host is required")
	}
	if !allowPrivate && isPrivateEgressHost(host) {
		return fmt.Errorf("egress to private host %s not allowed", host)
	}
	for _, allowed := range allowedHosts {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		if allowed == "" {
			continue
		}
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return nil
		}
	}

	return fmt.Errorf("egress to %s not allowed", host)
}

func upstreamHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func isPrivateEgressHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// InjectCredential adds the upstream credential to the request headers.
func InjectCredential(upstreamReq *http.Request, apiType providers.ProviderType, credential string) {
	InjectCredentialHeaders(upstreamReq.Header, apiType, credential)
}

// InjectCredentialHeaders adds the upstream credential to a header map.
func InjectCredentialHeaders(headers http.Header, apiType providers.ProviderType, credential string) {
	switch apiType {
	case providers.ProviderOpenAI, providers.ProviderOpenAIResponses:
		headers.Set("Authorization", "Bearer "+credential)
	case providers.ProviderAnthropic:
		headers.Set("x-api-key", credential)
		headers.Set("anthropic-version", "2023-06-01")
	default:
		headers.Set("Authorization", "Bearer "+credential)
	}
}

// ContextOverflowNormalized checks if an error response indicates context overflow.
// Returns a normalized error type string.
func ContextOverflowNormalized(providerType providers.ProviderType, body []byte) (bool, string) {
	bodyStr := string(body)

	switch providerType {
	case providers.ProviderOpenAI, providers.ProviderOpenAIResponses:
		if strings.Contains(bodyStr, "context_length_exceeded") ||
			strings.Contains(bodyStr, "maximum context length") ||
			strings.Contains(bodyStr, "token limit") {
			return true, "context_length_exceeded"
		}
	case providers.ProviderAnthropic:
		if strings.Contains(bodyStr, "prompt is too long") ||
			strings.Contains(bodyStr, "input too long") ||
			strings.Contains(bodyStr, "max_tokens") {
			return true, "context_length_exceeded"
		}
	}

	return false, ""
}

// PathOrDefault returns the path if non-empty, otherwise returns defaultPath.
func PathOrDefault(p, defaultPath string) string {
	if p == "" {
		return defaultPath
	}
	return p
}

// SanitizePath removes path traversal attempts.
func SanitizePath(p string) string {
	cleaned := path.Clean(p)
	if strings.HasPrefix(cleaned, "..") {
		return "/"
	}
	return cleaned
}
