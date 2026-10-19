// Package codexauth implements OAuth2 refresh-token rotation for OpenAI Codex
// (ChatGPT) credentials stored by the proxy.
//
// The Codex CLI authenticates against chatgpt.com/backend-api using an OAuth2
// access token issued by https://auth.openai.com. Access tokens are short-lived
// (~10 days) and the refresh token ROTATES on every refresh: the response
// returns a new refresh_token and the previous one is invalidated. The proxy
// must therefore persist the rotated refresh token after each successful
// refresh, and only one owner may refresh a given token at a time (two
// independent refreshers will invalidate each other).
package codexauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/earlgrey/piproxy/internal/secrets"
)

// Refresher exchanges a Codex OAuth2 refresh token for a fresh access token.
type Refresher struct {
	client *http.Client
}

// NewRefresher creates a Refresher. OAuth endpoint/client settings are loaded
// from the encrypted DB credential payload, not source-code defaults.
func NewRefresher() *Refresher {
	return &Refresher{
		client: &http.Client{Timeout: 20 * time.Second},
	}
}

// TokenResponse is the OAuth2 refresh response from auth.openai.com.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	// OpenAI has returned earliest_refresh_at as both an RFC3339 string and a
	// numeric timestamp. It is advisory and currently unused, so retain the raw
	// JSON value instead of making token refresh depend on one wire type.
	EarliestRefreshAt json.RawMessage `json:"earliest_refresh_at"`
}

// Refresh exchanges refreshToken for a new access token.
func (r *Refresher) Refresh(ctx context.Context, refreshToken, tokenURL, clientID, scope string) (*TokenResponse, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("codexauth: refresh token is empty")
	}
	if strings.TrimSpace(tokenURL) == "" {
		return nil, fmt.Errorf("codexauth: oauth token URL is empty")
	}
	if strings.TrimSpace(clientID) == "" {
		return nil, fmt.Errorf("codexauth: oauth client_id is empty")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	}
	if strings.TrimSpace(scope) != "" {
		form.Set("scope", scope)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("codexauth: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codexauth: token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("codexauth: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codexauth: token endpoint returned %d: %s", resp.StatusCode, snippet(body))
	}

	var tr TokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("codexauth: decode token response: %w", err)
	}
	if tr.AccessToken == "" || tr.RefreshToken == "" {
		return nil, fmt.Errorf("codexauth: token response missing access_token or refresh_token")
	}
	return &tr, nil
}

// RefreshCredential implements secrets.OAuthRefresher. It refreshes a
// Credential holding a Codex OAuth2 token pair and returns an updated copy with
// the new access token, rotated refresh token, and recomputed expiry.
func (r *Refresher) RefreshCredential(ctx context.Context, cred *secrets.Credential) (*secrets.Credential, error) {
	if cred == nil {
		return nil, fmt.Errorf("codexauth: nil credential")
	}
	tr, err := r.Refresh(ctx, cred.RefreshToken, cred.OAuthTokenURL, cred.OAuthClientID, cred.OAuthScope)
	if err != nil {
		return nil, err
	}
	out := *cred // shallow copy is sufficient; fields are all value types
	out.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		out.RefreshToken = tr.RefreshToken
	}
	if tr.ExpiresIn > 0 {
		out.ExpiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return &out, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 256 {
		s = s[:256] + "..."
	}
	return s
}
