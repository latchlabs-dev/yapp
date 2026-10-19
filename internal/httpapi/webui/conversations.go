package webui

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

const maxTitleLen = 200

// conversationRequest is the create/patch body. Pointer fields on the patch
// path distinguish "leave alone" from "set to empty".
type conversationRequest struct {
	Title      *string `json:"title,omitempty"`
	Model      *string `json:"model,omitempty"`
	ProviderID *string `json:"provider_id,omitempty"`
	ProjectID  *string `json:"project_id,omitempty"`
}

func listConversationsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authCtx, ok := requireAuth(w, r)
		if !ok {
			return
		}
		conversations, err := deps.Conversations.List(r.Context(), authCtx.TenantID, authCtx.UserID, 100)
		if err != nil {
			deps.Logger.Error().Err(err).Msg("web conversations list failed")
			writeError(w, http.StatusInternalServerError, "list_conversations", "could not load conversations")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"conversations": conversations})
	}
}

func createConversationHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authCtx, ok := requireAuth(w, r)
		if !ok {
			return
		}
		if !deps.checkWriteBudget(r.Context(), authCtx) {
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many conversations created; wait a moment and try again")
			return
		}
		var body conversationRequest
		if err := decodeJSON(w, r, &body); err != nil {
			return
		}

		conv := &pgstore.WebConversation{
			TenantID: authCtx.TenantID,
			UserID:   authCtx.UserID,
			Title:    truncate(deref(body.Title), maxTitleLen),
			Model:    deref(body.Model),
		}
		if body.ProviderID != nil {
			conv.ProviderID = *body.ProviderID
		}
		if pid := strings.TrimSpace(deref(body.ProjectID)); pid != "" {
			if _, err := deps.resolveTenantProject(r.Context(), authCtx.TenantID, pid); err != nil {
				writeError(w, http.StatusNotFound, "project_not_found", "project not found")
				return
			}
			conv.ProjectID = pid
		}

		created, err := deps.Conversations.Create(r.Context(), conv)
		if err != nil {
			deps.Logger.Error().Err(err).Msg("web conversation create failed")
			writeError(w, http.StatusInternalServerError, "create_conversation", "could not create conversation")
			return
		}
		writeJSON(w, http.StatusCreated, created)
	}
}

func getConversationHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authCtx, ok := requireAuth(w, r)
		if !ok {
			return
		}
		conv, ok := deps.loadConversation(w, r, authCtx)
		if !ok {
			return
		}
		messages, err := deps.Conversations.ListMessages(r.Context(), conv.ID, deps.Config.MaxHistoryMessages)
		if err != nil {
			deps.Logger.Error().Err(err).Msg("web conversation messages failed")
			writeError(w, http.StatusInternalServerError, "list_messages", "could not load messages")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"conversation": conv,
			"messages":     messages,
		})
	}
}

func updateConversationHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authCtx, ok := requireAuth(w, r)
		if !ok {
			return
		}
		var body conversationRequest
		if err := decodeJSON(w, r, &body); err != nil {
			return
		}

		update := pgstore.ConversationUpdate{Model: body.Model, Provider: body.ProviderID}
		if body.Title != nil {
			trimmed := truncate(*body.Title, maxTitleLen)
			update.Title = &trimmed
		}
		if body.ProjectID != nil {
			pid := strings.TrimSpace(*body.ProjectID)
			if pid != "" {
				if _, err := deps.resolveTenantProject(r.Context(), authCtx.TenantID, pid); err != nil {
					writeError(w, http.StatusNotFound, "project_not_found", "project not found")
					return
				}
			}
			update.ProjectID = &pid
		}

		conv, err := deps.Conversations.Update(r.Context(), authCtx.TenantID, authCtx.UserID, chi.URLParam(r, "conversationID"), update)
		if errors.Is(err, pgstore.ErrConversationNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "conversation not found")
			return
		}
		if err != nil {
			deps.Logger.Error().Err(err).Msg("web conversation update failed")
			writeError(w, http.StatusInternalServerError, "update_conversation", "could not update conversation")
			return
		}
		writeJSON(w, http.StatusOK, conv)
	}
}

func archiveConversationHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authCtx, ok := requireAuth(w, r)
		if !ok {
			return
		}
		err := deps.Conversations.Archive(r.Context(), authCtx.TenantID, authCtx.UserID, chi.URLParam(r, "conversationID"))
		if errors.Is(err, pgstore.ErrConversationNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "conversation not found")
			return
		}
		if err != nil {
			deps.Logger.Error().Err(err).Msg("web conversation archive failed")
			writeError(w, http.StatusInternalServerError, "archive_conversation", "could not archive conversation")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "archived"})
	}
}

// loadConversation fetches the URL-named conversation and writes the 404 for
// the caller when it is missing, so handlers stay a single early return.
func (deps *Dependencies) loadConversation(w http.ResponseWriter, r *http.Request, authCtx *auth.Context) (*pgstore.WebConversation, bool) {
	conv, err := deps.Conversations.Get(r.Context(), authCtx.TenantID, authCtx.UserID, chi.URLParam(r, "conversationID"))
	if errors.Is(err, pgstore.ErrConversationNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "conversation not found")
		return nil, false
	}
	if err != nil {
		deps.Logger.Error().Err(err).Msg("web conversation load failed")
		writeError(w, http.StatusInternalServerError, "get_conversation", "could not load conversation")
		return nil, false
	}
	return conv, true
}

func requireAuth(w http.ResponseWriter, r *http.Request) (*auth.Context, bool) {
	authCtx := middleware.GetAuthContext(r.Context())
	if authCtx == nil || authCtx.TenantID == "" || authCtx.UserID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "sign-in required")
		return nil, false
	}
	return authCtx, true
}

// decodeJSON reads a small JSON body, writing the 400 itself. The cap is well
// below the router's body limit; these are metadata bodies, not payloads.
func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is not valid JSON")
		return err
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	// Cut on a rune boundary: slicing bytes blindly can split a multi-byte
	// character, putting invalid UTF-8 in titles and filenames the UI renders.
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut])
}
