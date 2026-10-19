package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/earlgrey/piproxy/internal/hooks"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

// hookEventTypes are the events a subscription may register for. The list is
// served to the UI so the dropdown cannot offer an event the proxy never emits.
var hookEventTypes = []hooks.EventType{
	hooks.EventUserMessagePreSend,
	hooks.EventUserMessagePostSend,
	hooks.EventSessionUploaded,
	hooks.EventToolCallStarted,
	hooks.EventToolCallCompleted,
	hooks.EventToolCallFailed,
	hooks.EventWebConversationCreated,
	hooks.EventWebAttachmentUploaded,
	hooks.EventWebMemoryEdited,
}

type hookSubscriptionInput struct {
	Name           string `json:"name"`
	TenantID       string `json:"tenant_id"`
	ProjectID      string `json:"project_id"`
	EventType      string `json:"event_type"`
	URL            string `json:"url"`
	Secret         string `json:"secret"`
	TimeoutMS      int    `json:"timeout_ms"`
	Mode           string `json:"mode"`
	IncludeContent bool   `json:"include_content"`
	AllowMutation  bool   `json:"allow_mutation"`
	FailMode       string `json:"fail_mode"`
	Priority       int    `json:"priority"`
	Enabled        *bool  `json:"enabled"`
}

func requireHookStore(deps *Dependencies, w http.ResponseWriter) bool {
	if deps.HookSubscriptionStore == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "hook subscriptions are not configured")
		return false
	}
	return true
}

func listHookEventTypesHandler(_ *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"event_types": hookEventTypes})
	}
}

func listHookSubscriptionsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireHookStore(deps, w) {
			return
		}
		records, err := deps.HookSubscriptionStore.List(r.Context())
		if err != nil {
			deps.Logger.Error().Err(err).Msg("hook subscriptions list failed")
			writeError(w, http.StatusInternalServerError, "list_hook_subscriptions", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"subscriptions": records})
	}
}

// upsertHookSubscriptionHandler creates or replaces a subscription.
//
// Registration is admin-only and stays that way: a subscription with
// include_content is a channel that ships user prompts to a third-party URL,
// and one with allow_mutation can rewrite what the model is asked and what the
// user is shown. Neither is a setting a tenant should be able to grant itself.
func upsertHookSubscriptionHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireHookStore(deps, w) {
			return
		}
		var input hookSubscriptionInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}

		record, err := validateHookSubscription(r, deps, input, chi.URLParam(r, "subscriptionID"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation_error", err.Error())
			return
		}

		saved, err := deps.HookSubscriptionStore.Upsert(r.Context(), record)
		if err != nil {
			deps.Logger.Error().Err(err).Msg("hook subscription upsert failed")
			writeError(w, http.StatusInternalServerError, "upsert_hook_subscription", err.Error())
			return
		}
		deps.Logger.Info().
			Str("subscription_id", saved.ID).
			Str("event_type", saved.EventType).
			Bool("include_content", saved.IncludeContent).
			Bool("allow_mutation", saved.AllowMutation).
			Msg("hook subscription saved")
		writeJSON(w, http.StatusOK, saved)
	}
}

func validateHookSubscription(r *http.Request, deps *Dependencies, input hookSubscriptionInput, id string) (*pgstore.HookSubscriptionRecord, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return nil, errors.New("name is required")
	}
	if !knownHookEvent(input.EventType) {
		return nil, errors.New("unknown event_type " + input.EventType)
	}

	mode := strings.TrimSpace(input.Mode)
	if mode == "" {
		mode = hooks.ModeAsync
	}
	if mode != hooks.ModeAsync && mode != hooks.ModeBlocking {
		return nil, errors.New(`mode must be "blocking" or "async"`)
	}
	failMode := strings.TrimSpace(input.FailMode)
	if failMode == "" {
		failMode = hooks.FailOpen
	}
	if failMode != hooks.FailOpen && failMode != hooks.FailClosed {
		return nil, errors.New(`fail_mode must be "open" or "closed"`)
	}
	// An async hook is delivered after the turn is already served, so it has
	// nothing to mutate and nothing to fail closed over. Reject the
	// combination here rather than silently ignoring it at delivery time.
	if mode == hooks.ModeAsync && (input.AllowMutation || failMode == hooks.FailClosed) {
		return nil, errors.New("allow_mutation and fail_mode=closed require mode=blocking")
	}
	if input.AllowMutation && !input.IncludeContent {
		return nil, errors.New("allow_mutation requires include_content: a hook cannot rewrite what it cannot see")
	}

	timeout := input.TimeoutMS
	if timeout == 0 {
		timeout = 1500
	}
	if timeout < 100 || timeout > 30000 {
		return nil, errors.New("timeout_ms must be between 100 and 30000")
	}

	// Resolve the URL now so a bad endpoint is rejected at registration rather
	// than discovered on a live request. It is checked again at delivery time,
	// because DNS can change under us.
	if err := hooks.ValidateSubscriptionURL(r.Context(), input.URL, deps.AllowPrivateHookEgress); err != nil {
		return nil, err
	}

	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	priority := input.Priority
	if priority == 0 {
		priority = 100
	}

	return &pgstore.HookSubscriptionRecord{
		ID:             id,
		Name:           name,
		TenantID:       strings.TrimSpace(input.TenantID),
		ProjectID:      strings.TrimSpace(input.ProjectID),
		EventType:      input.EventType,
		URL:            strings.TrimSpace(input.URL),
		Secret:         input.Secret,
		TimeoutMS:      timeout,
		Mode:           mode,
		IncludeContent: input.IncludeContent,
		AllowMutation:  input.AllowMutation,
		FailMode:       failMode,
		Priority:       priority,
		Enabled:        enabled,
	}, nil
}

func deleteHookSubscriptionHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireHookStore(deps, w) {
			return
		}
		err := deps.HookSubscriptionStore.Delete(r.Context(), chi.URLParam(r, "subscriptionID"))
		if errors.Is(err, pgstore.ErrHookSubscriptionNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "hook subscription not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "delete_hook_subscription", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}
}

// testHookSubscriptionHandler delivers a synthetic event so an operator can
// confirm an endpoint is reachable and returns a decision the proxy
// understands, without waiting for real traffic to hit it.
func testHookSubscriptionHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireHookStore(deps, w) {
			return
		}
		record, err := deps.HookSubscriptionStore.Get(r.Context(), chi.URLParam(r, "subscriptionID"))
		if errors.Is(err, pgstore.ErrHookSubscriptionNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "hook subscription not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "get_hook_subscription", err.Error())
			return
		}

		active, err := deps.HookSubscriptionStore.LoadActive(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "load_hook_subscriptions", err.Error())
			return
		}
		var target *hooks.Subscription
		for _, sub := range active {
			if sub.ID == record.ID {
				target = sub
				break
			}
		}
		if target == nil {
			writeError(w, http.StatusConflict, "not_active", "subscription is disabled or its secret could not be decrypted")
			return
		}

		// A one-shot manager so the test delivery cannot disturb the live
		// hook pipeline or land in the delivery log as real traffic.
		probe := hooks.NewManager(hooks.Config{Timeout: target.Timeout}, deps.Logger)
		defer probe.Close()
		probe.SetSubscriptions(hooks.SubscriptionFunc(func() []*hooks.Subscription {
			return []*hooks.Subscription{{
				ID: target.ID, Name: target.Name, EventType: target.EventType,
				URL: target.URL, Secret: target.Secret, Timeout: target.Timeout,
				Mode: hooks.ModeBlocking, IncludeContent: target.IncludeContent,
				AllowMutation: target.AllowMutation, FailMode: hooks.FailOpen,
				Enabled: true,
			}}
		}))
		probe.SetAllowPrivateHookEgress(deps.AllowPrivateHookEgress)

		started := time.Now()
		decision, hookErr := probe.EmitBlocking(r.Context(), &hooks.Event{
			Type:     target.EventType,
			TenantID: record.TenantID,
			Model:    "test-model",
			Provider: "test",
			Content: &hooks.EventContent{
				System:   "This is a piproxy hook connectivity test.",
				Messages: []hooks.ContentMessage{{Role: "user", Text: "hook connectivity test"}},
			},
		})
		result := map[string]any{
			"delivered":  hookErr == nil,
			"latency_ms": time.Since(started).Milliseconds(),
			"decision":   decision,
		}
		if hookErr != nil {
			result["error"] = hookErr.Error()
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func listHookDeliveriesHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireHookStore(deps, w) {
			return
		}
		limit := 100
		if raw := r.URL.Query().Get("limit"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil {
				limit = parsed
			}
		}
		records, err := deps.HookSubscriptionStore.ListDeliveries(r.Context(), r.URL.Query().Get("subscription_id"), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_hook_deliveries", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deliveries": records})
	}
}

func knownHookEvent(candidate string) bool {
	for _, known := range hookEventTypes {
		if string(known) == candidate {
			return true
		}
	}
	return false
}
