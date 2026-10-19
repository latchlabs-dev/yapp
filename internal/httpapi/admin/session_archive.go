package admin

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/earlgrey/piproxy/internal/sessionarchive"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

// SessionArchive is the session archive surface the admin API reads:
// metadata and turn summaries from Postgres, transcripts from object storage.
type SessionArchive interface {
	FindTranscripts(ctx context.Context, conversationID string) ([]*sessionarchive.State, error)
	ConversationTenants(ctx context.Context, conversationID string) ([]pgstore.ConversationTenant, error)
	ListTurns(ctx context.Context, transcriptID string, generation int) ([]sessionarchive.Turn, error)
	TurnCosts(ctx context.Context, transcriptID string, generation int) (map[string]float64, error)
	TurnIPs(ctx context.Context, transcriptID string, generation int) ([]pgstore.SessionIP, error)
	OwnerNames(ctx context.Context, tenantID, userID string) (pgstore.OwnerNames, error)
	TranscriptPage(ctx context.Context, tenantID, conversationID string, fromSeq, limit int) (*sessionarchive.TranscriptPage, *sessionarchive.State, error)
	StartRebuild(tenantID, conversationID string) bool
	Rebuilding(tenantID, conversationID string) bool
	ObjectsLocation() string
}

type sessionArchiveView struct {
	ConversationID  string    `json:"conversation_id"`
	TenantID        string    `json:"tenant_id,omitempty"`
	TenantName      string    `json:"tenant_name,omitempty"`
	UserID          string    `json:"user_id,omitempty"`
	UserDisplayName string    `json:"user_display_name,omitempty"`
	UserEmail       string    `json:"user_email,omitempty"`
	KeyID           string    `json:"key_id,omitempty"`
	Provider        string    `json:"provider"`
	Model           string    `json:"model"`
	API             string    `json:"api"`
	OpeningPrompt   string    `json:"opening_prompt,omitempty"`
	LastPrompt      string    `json:"last_prompt,omitempty"`
	RequestCount    int       `json:"request_count"`
	UserTurnCount   int       `json:"user_turn_count"`
	ToolCallCount   int       `json:"tool_call_count"`
	ErrorCount      int       `json:"error_count"`
	ResetCount      int       `json:"reset_count"`
	StartedAt       time.Time `json:"started_at"`
	LastActivityAt  time.Time `json:"last_activity_at"`
	CostTotalCents  float64   `json:"cost_total_cents"`
	TranscriptBytes int64     `json:"transcript_bytes"`
	ObjectsLocation string    `json:"objects_location"`
	Generation      int       `json:"generation"`
	LastSeq         int       `json:"last_seq"`
	NeedsRebuild    bool      `json:"needs_rebuild"`
	LastError       string    `json:"last_error,omitempty"`
}

type sessionTurnView struct {
	sessionarchive.Turn
	CostTotalCents float64 `json:"cost_total_cents"`
}

// sessionIPView is one distinct client IP observed among the session's
// requests: which device/server drove it, how often, and when.
type sessionIPView struct {
	IP        string    `json:"ip"`
	Requests  int       `json:"requests"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

type sessionMatch struct {
	TenantID       string    `json:"tenant_id"`
	TenantName     string    `json:"tenant_name,omitempty"`
	LastActivityAt time.Time `json:"last_activity_at"`
	RequestCount   int       `json:"request_count"`
	HasTranscript  bool      `json:"has_transcript"`
}

// resolveSessionTenant picks which tenant's copy of a conversation id to
// serve: the tenant_id query parameter when present (an empty value selects
// unauthenticated captures), otherwise the most recently active one.
func resolveSessionTenant(r *http.Request, states []*sessionarchive.State, tenants []pgstore.ConversationTenant) (string, []sessionMatch, bool) {
	byTenant := make(map[string]*sessionMatch)
	for _, ct := range tenants {
		byTenant[ct.TenantID] = &sessionMatch{TenantID: ct.TenantID, TenantName: ct.TenantName, LastActivityAt: ct.LastPayloadAt, RequestCount: ct.RequestCount}
	}
	for _, st := range states {
		m, ok := byTenant[st.TenantID]
		if !ok {
			m = &sessionMatch{TenantID: st.TenantID}
			byTenant[st.TenantID] = m
		}
		m.HasTranscript = true
		if st.LastActivityAt.After(m.LastActivityAt) {
			m.LastActivityAt = st.LastActivityAt
		}
	}
	matches := make([]sessionMatch, 0, len(byTenant))
	for _, m := range byTenant {
		matches = append(matches, *m)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].LastActivityAt.After(matches[j].LastActivityAt) })

	if r.URL.Query().Has("tenant_id") {
		tenantID := r.URL.Query().Get("tenant_id")
		_, ok := byTenant[tenantID]
		return tenantID, matches, ok
	}
	if len(matches) == 0 {
		return "", matches, false
	}
	return matches[0].TenantID, matches, true
}

// getSessionArchiveHandler serves GET /sessions/{conversationID}: the session
// header and turn list, without reading any object. A missing or stale
// transcript starts a background rebuild; the response then says so and the
// client polls.
func getSessionArchiveHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.SessionArchive == nil {
			writeError(w, http.StatusServiceUnavailable, "session_archive_unavailable", "session archive is not configured")
			return
		}
		ctx := r.Context()
		conversationID := chi.URLParam(r, "conversationID")

		states, err := deps.SessionArchive.FindTranscripts(ctx, conversationID)
		if err != nil {
			deps.Logger.Error().Err(err).Str("conversation_id", conversationID).Msg("find session transcripts failed")
			writeError(w, http.StatusInternalServerError, "session_lookup_failed", err.Error())
			return
		}
		tenants, err := deps.SessionArchive.ConversationTenants(ctx, conversationID)
		if err != nil {
			deps.Logger.Error().Err(err).Str("conversation_id", conversationID).Msg("list session tenants failed")
			writeError(w, http.StatusInternalServerError, "session_lookup_failed", err.Error())
			return
		}
		tenantID, matches, ok := resolveSessionTenant(r, states, tenants)
		if !ok {
			writeError(w, http.StatusNotFound, "session_not_found", "no captured requests or transcript for this conversation")
			return
		}

		var state *sessionarchive.State
		for _, st := range states {
			if st.TenantID == tenantID {
				state = st
			}
		}
		hasCaptures := false
		for _, ct := range tenants {
			if ct.TenantID == tenantID {
				hasCaptures = true
			}
		}
		// A transcript written to another object store (before a backend
		// switch) has keys that do not resolve here: it must be rebuilt, and
		// until then its pages cannot be read.
		readable := state != nil && !state.NeedsRebuild && state.StoredIn(deps.SessionArchive.ObjectsLocation())
		rebuilding := deps.SessionArchive.Rebuilding(tenantID, conversationID)
		if hasCaptures && !rebuilding && (state == nil || state.NeedsRebuild || !readable) {
			rebuilding = deps.SessionArchive.StartRebuild(tenantID, conversationID) || deps.SessionArchive.Rebuilding(tenantID, conversationID)
		}

		resp := map[string]any{
			"tenant_id":           tenantID,
			"matches":             matches,
			"rebuilding":          rebuilding,
			"transcript_readable": readable,
			"session":             nil,
			"turns":               []sessionTurnView{},
			"client_ips":          []sessionIPView{},
		}
		if state == nil {
			writeJSON(w, http.StatusAccepted, resp)
			return
		}

		turns, err := deps.SessionArchive.ListTurns(ctx, state.TranscriptID, state.Generation)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "session_turns_failed", err.Error())
			return
		}
		costs, err := deps.SessionArchive.TurnCosts(ctx, state.TranscriptID, state.Generation)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "session_costs_failed", err.Error())
			return
		}
		ips, err := deps.SessionArchive.TurnIPs(ctx, state.TranscriptID, state.Generation)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "session_ips_failed", err.Error())
			return
		}
		names, err := deps.SessionArchive.OwnerNames(ctx, state.TenantID, state.UserID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "session_owner_failed", err.Error())
			return
		}

		views := make([]sessionTurnView, len(turns))
		var total float64
		for i, turn := range turns {
			views[i] = sessionTurnView{Turn: turn, CostTotalCents: costs[turn.RequestID]}
			total += costs[turn.RequestID]
		}
		resp["session"] = sessionArchiveView{
			ConversationID: state.ConversationID, TenantID: state.TenantID, TenantName: names.TenantName,
			UserID: state.UserID, UserDisplayName: names.UserDisplayName, UserEmail: names.UserEmail, KeyID: state.KeyID,
			Provider: state.Provider, Model: state.Model, API: string(state.API),
			OpeningPrompt: state.OpeningPrompt, LastPrompt: state.LastPrompt,
			RequestCount: state.RequestCount, UserTurnCount: state.UserTurnCount, ToolCallCount: state.ToolCallCount,
			ErrorCount: state.ErrorCount, ResetCount: state.ResetCount,
			StartedAt: state.StartedAt, LastActivityAt: state.LastActivityAt, CostTotalCents: total,
			TranscriptBytes: state.TranscriptBytes(), ObjectsLocation: state.ObjectsLocation,
			Generation: state.Generation, LastSeq: state.LastSeq, NeedsRebuild: state.NeedsRebuild, LastError: state.LastError,
		}
		resp["turns"] = views
		ipViews := make([]sessionIPView, len(ips))
		for i, ip := range ips {
			ipViews[i] = sessionIPView{IP: ip.IP, Requests: ip.Requests, FirstSeen: ip.FirstSeen, LastSeen: ip.LastSeen}
		}
		resp["client_ips"] = ipViews
		writeJSON(w, http.StatusOK, resp)
	}
}

// getSessionTranscriptHandler serves GET /sessions/{conversationID}/transcript.
func getSessionTranscriptHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.SessionArchive == nil {
			writeError(w, http.StatusServiceUnavailable, "session_archive_unavailable", "session archive is not configured")
			return
		}
		ctx := r.Context()
		conversationID := chi.URLParam(r, "conversationID")
		q := r.URL.Query()

		tenantID := q.Get("tenant_id")
		if !q.Has("tenant_id") {
			states, err := deps.SessionArchive.FindTranscripts(ctx, conversationID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "session_lookup_failed", err.Error())
				return
			}
			if len(states) == 0 {
				tenants, err := deps.SessionArchive.ConversationTenants(ctx, conversationID)
				if err != nil {
					writeError(w, http.StatusInternalServerError, "session_lookup_failed", err.Error())
					return
				}
				if len(tenants) == 0 {
					writeError(w, http.StatusNotFound, "transcript_not_found", "no transcript for this conversation")
					return
				}
				tenantID = tenants[0].TenantID
			} else {
				tenantID = states[0].TenantID
			}
		}
		fromSeq := 1
		if n, err := strconv.Atoi(q.Get("from_seq")); err == nil && n > 0 {
			fromSeq = n
		}
		limit := sessionarchive.DefaultPageTurns
		if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
			limit = n
		}

		page, state, err := deps.SessionArchive.TranscriptPage(ctx, tenantID, conversationID, fromSeq, limit)
		if errors.Is(err, sessionarchive.ErrTranscriptNotFound) {
			writeError(w, http.StatusNotFound, "transcript_not_found", "no transcript for this conversation")
			return
		}
		if errors.Is(err, sessionarchive.ErrTranscriptRebuilding) {
			writeError(w, http.StatusConflict, "transcript_rebuilding", "the transcript is queued for repair; retry shortly")
			return
		}
		if err != nil {
			deps.Logger.Error().Err(err).Str("conversation_id", conversationID).Msg("read session transcript failed")
			writeError(w, http.StatusInternalServerError, "transcript_read_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"tenant_id":  tenantID,
			"generation": state.Generation,
			"turns":      page.Turns,
			"next_seq":   page.NextSeq,
			"last_seq":   page.LastSeq,
		})
	}
}

// rebuildSessionTranscriptHandler serves POST /sessions/{conversationID}/rebuild.
func rebuildSessionTranscriptHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.SessionArchive == nil {
			writeError(w, http.StatusServiceUnavailable, "session_archive_unavailable", "session archive is not configured")
			return
		}
		ctx := r.Context()
		conversationID := chi.URLParam(r, "conversationID")
		tenants, err := deps.SessionArchive.ConversationTenants(ctx, conversationID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "session_lookup_failed", err.Error())
			return
		}
		tenantID, _, ok := resolveSessionTenant(r, nil, tenants)
		if !ok {
			writeError(w, http.StatusConflict, "no_captures", "no captured requests to rebuild this transcript from")
			return
		}
		started := deps.SessionArchive.StartRebuild(tenantID, conversationID)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"tenant_id":       tenantID,
			"rebuilding":      true,
			"already_running": !started,
		})
	}
}
