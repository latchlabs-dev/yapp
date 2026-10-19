package sessionarchive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxQueryResponseBytes = 64 << 20

// QueryClient is the authenticated, bounded Parquet-page accelerator. Ownership
// and the requested snapshot always come from PostgreSQL in the caller.
type QueryClient struct {
	url, token string
	http       *http.Client
}

func NewQueryClient(url, token string, timeout time.Duration) *QueryClient {
	return &QueryClient{
		url: strings.TrimRight(url, "/"), token: token,
		http: &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // Never forward the service token to a redirect.
		}},
	}
}

func (c *QueryClient) Page(ctx context.Context, state *State, fromSeq, limit int) (*TranscriptPage, error) {
	fromSeq = max(1, fromSeq)
	limit = pageLimit(limit)
	body, err := json.Marshal(map[string]any{
		"tenant_id": state.TenantID, "transcript_id": state.TranscriptID,
		"generation": state.Generation, "last_seq": state.LastSeq,
		"from_seq": fromSeq, "limit": limit,
	})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/v1/page", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("session query request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("session query status %d", response.StatusCode)
	}
	tenantHash := sha256.Sum256([]byte(state.TenantID))
	if response.Header.Get("X-Transcript-ID") != state.TranscriptID ||
		response.Header.Get("X-Transcript-Tenant") != hex.EncodeToString(tenantHash[:]) ||
		response.Header.Get("X-Transcript-Generation") != strconv.Itoa(state.Generation) ||
		response.Header.Get("X-Transcript-Last-Seq") != strconv.Itoa(state.LastSeq) {
		return nil, fmt.Errorf("session query snapshot identity mismatch")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxQueryResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxQueryResponseBytes {
		return nil, fmt.Errorf("session query response exceeds byte limit")
	}
	return RenderJSONLPage(data, state.LastSeq, fromSeq, limit)
}
