package webui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/providers"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

func testRoute(providerType providers.ProviderType, basePath string) *providers.ModelRoute {
	return &providers.ModelRoute{
		Provider: &providers.Provider{
			ID:             "acct_1",
			Type:           providerType,
			PublicBasePath: basePath,
			Models: []providers.Model{
				{PublicID: "vision-model", Input: []string{"text", "image"}},
				{PublicID: "text-model", Input: []string{"text"}},
			},
		},
		PublicModel:   "text-model",
		UpstreamModel: "text-model",
	}
}

// The synthesised request is what carries console traffic into the shared
// pipeline, so the attribution headers it sets are what make a console turn
// land on the right project and conversation in usage and capture.
func TestInternalRequestCarriesAttribution(t *testing.T) {
	authCtx := &auth.Context{TenantID: "tnt_1", UserID: "usr_1", Scopes: []string{"llm:invoke"}}
	conv := &pgstore.WebConversation{ID: "wcv_1", TenantID: "tnt_1", UserID: "usr_1", ProjectID: "mprj_1"}
	outer := httptest.NewRequest(http.MethodPost, "/web/v1/conversations/wcv_1/messages", nil)
	outer = outer.WithContext(middleware.WithAuthContext(outer.Context(), authCtx))

	inner := internalRequest(outer, authCtx, testRoute(providers.ProviderOpenAI, "/openai"), conv,
		[]byte(`{"model":"text-model"}`), protocolOpenAI)

	if inner.URL.Path != "/openai/v1/chat/completions" {
		t.Errorf("path = %q, want the provider's chat-completions path", inner.URL.Path)
	}
	if got := inner.Header.Get(providers.RouteHeader); got != "acct_1" {
		t.Errorf("%s = %q, want the provider account id", providers.RouteHeader, got)
	}
	if got := inner.Header.Get(capture.HeaderConversationID); got != "wcv_1" {
		t.Errorf("conversation header = %q, want wcv_1", got)
	}
	if got := inner.Header.Get(capture.HeaderProjectID); got != "mprj_1" {
		t.Errorf("project header = %q, want mprj_1", got)
	}
	if middleware.GetAuthContext(inner.Context()) == nil {
		t.Error("the auth context did not survive onto the internal request")
	}
}

func TestInternalRequestUsesAnthropicPath(t *testing.T) {
	authCtx := &auth.Context{TenantID: "tnt_1", UserID: "usr_1"}
	conv := &pgstore.WebConversation{ID: "wcv_1"}
	outer := httptest.NewRequest(http.MethodPost, "/web/v1/x", nil)

	inner := internalRequest(outer, authCtx, testRoute(providers.ProviderAnthropic, "/anthropic"), conv,
		[]byte(`{}`), protocolAnthropic)

	if inner.URL.Path != "/anthropic/v1/messages" {
		t.Errorf("path = %q, want the messages path", inner.URL.Path)
	}
}

// The browser's request carries a session cookie and a CSRF token. Neither has
// any business travelling further into capture or towards a provider, so the
// internal request is built with a fresh header set rather than a clone.
func TestInternalRequestDropsBrowserCredentials(t *testing.T) {
	authCtx := &auth.Context{TenantID: "tnt_1", UserID: "usr_1"}
	conv := &pgstore.WebConversation{ID: "wcv_1"}
	outer := httptest.NewRequest(http.MethodPost, "/web/v1/x", nil)
	outer.Header.Set("Cookie", "piproxy_web_session=super-secret")
	outer.Header.Set("X-Web-CSRF", "csrf-token")
	outer.Header.Set("Authorization", "Bearer leaked")

	inner := internalRequest(outer, authCtx, testRoute(providers.ProviderOpenAI, "/openai"), conv,
		[]byte(`{}`), protocolOpenAI)

	for _, header := range []string{"Cookie", "X-Web-CSRF", "Authorization"} {
		if value := inner.Header.Get(header); value != "" {
			t.Errorf("%s survived onto the internal request: %q", header, value)
		}
	}
}

// A conversation with no project must not set an empty project header, which
// would otherwise be recorded as an attribution attempt.
func TestInternalRequestOmitsEmptyProject(t *testing.T) {
	authCtx := &auth.Context{TenantID: "tnt_1", UserID: "usr_1"}
	outer := httptest.NewRequest(http.MethodPost, "/web/v1/x", nil)

	inner := internalRequest(outer, authCtx, testRoute(providers.ProviderOpenAI, "/openai"),
		&pgstore.WebConversation{ID: "wcv_1"}, []byte(`{}`), protocolOpenAI)

	if _, present := inner.Header[http.CanonicalHeaderKey(capture.HeaderProjectID)]; present {
		t.Error("an empty project id was still sent as a header")
	}
}

// A session auth context has no KeyID. The data plane already handles that —
// quota scopes skip empty IDs and usage_events.key_id is nullable — and this
// asserts the console does not invent one.
func TestConsoleAuthContextCarriesNoKey(t *testing.T) {
	authCtx := &auth.Context{TenantID: "tnt_1", UserID: "usr_1", Scopes: []string{"llm:invoke"}}
	outer := httptest.NewRequest(http.MethodPost, "/web/v1/x", nil)

	inner := internalRequest(outer, authCtx, testRoute(providers.ProviderOpenAI, "/openai"),
		&pgstore.WebConversation{ID: "wcv_1"}, []byte(`{}`), protocolOpenAI)

	resolved := middleware.GetAuthContext(inner.Context())
	if resolved.KeyID != "" {
		t.Errorf("KeyID = %q, want empty for a browser session", resolved.KeyID)
	}
	if resolved.TenantID == "" || resolved.UserID == "" {
		t.Error("tenant and user must still be present for quota and usage attribution")
	}
}

func TestInternalRequestBodyIsReadable(t *testing.T) {
	payload := []byte(`{"model":"text-model","messages":[]}`)
	outer := httptest.NewRequest(http.MethodPost, "/web/v1/x", nil)

	inner := internalRequest(outer, &auth.Context{TenantID: "t"}, testRoute(providers.ProviderOpenAI, "/openai"),
		&pgstore.WebConversation{ID: "wcv_1"}, payload, protocolOpenAI)

	body, err := io.ReadAll(inner.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != string(payload) {
		t.Errorf("body = %q, want the assembled payload", body)
	}
	if inner.ContentLength != int64(len(payload)) {
		t.Errorf("ContentLength = %d, want %d", inner.ContentLength, len(payload))
	}
}

// --- turnRecorder ---

func recordStream(t *testing.T, proto protocol, buffered bool, frames ...string) (*turnRecorder, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	sse := newSSEWriter(rec)
	recorder := newTurnRecorder(sse, proto, buffered)
	recorder.Header().Set("Content-Type", "text/event-stream")
	recorder.WriteHeader(http.StatusOK)
	for _, frame := range frames {
		if _, err := recorder.Write([]byte(frame)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return recorder, rec.Body.String()
}

func TestRecorderNormalisesOpenAIStream(t *testing.T) {
	recorder, out := recordStream(t, protocolOpenAI, false,
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n",
		"data: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n",
		"data: [DONE]\n\n",
	)

	if recorder.Text() != "Hello" {
		t.Errorf("text = %q, want Hello", recorder.Text())
	}
	if recorder.Usage().TotalTokens != 9 {
		t.Errorf("usage = %+v, want 9 total tokens", recorder.Usage())
	}
	if !strings.Contains(out, "event: delta") || !strings.Contains(out, "event: usage") {
		t.Errorf("normalised stream is missing events:\n%s", out)
	}
}

func TestRecorderNormalisesAnthropicStream(t *testing.T) {
	recorder, out := recordStream(t, protocolAnthropic, false,
		"event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":11}}}\n\n",
		"event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n",
		"event: content_block_delta\ndata: {\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n",
		"event: message_delta\ndata: {\"usage\":{\"output_tokens\":3}}\n\n",
	)

	if recorder.Text() != "Hi" {
		t.Errorf("text = %q, want Hi — thinking must not land in the reply", recorder.Text())
	}
	if recorder.Usage().InputTokens != 11 || recorder.Usage().OutputTokens != 3 {
		t.Errorf("usage = %+v", recorder.Usage())
	}
	if !strings.Contains(out, "event: reasoning") {
		t.Error("thinking was not surfaced as a reasoning event")
	}
}

// A provider frame can be split across writes at any byte. Dropping the
// remainder would silently lose tokens from the middle of a reply.
func TestRecorderReassemblesSplitFrames(t *testing.T) {
	whole := "data: {\"choices\":[{\"delta\":{\"content\":\"split across writes\"}}]}\n\n"
	recorder, _ := recordStream(t, protocolOpenAI, false, whole[:20], whole[20:])

	if recorder.Text() != "split across writes" {
		t.Errorf("text = %q, want the reassembled content", recorder.Text())
	}
}

// In buffered mode nothing may reach the browser before a reviewing hook has
// run: a flushed token cannot be recalled.
func TestBufferedRecorderWithholdsDeltas(t *testing.T) {
	recorder, out := recordStream(t, protocolOpenAI, true,
		"data: {\"choices\":[{\"delta\":{\"content\":\"secret\"}}]}\n\n",
	)

	if recorder.Text() != "secret" {
		t.Errorf("text = %q, the recorder must still accumulate", recorder.Text())
	}
	if strings.Contains(out, "secret") {
		t.Fatalf("a buffered turn leaked content to the browser:\n%s", out)
	}
}

// A failure from the delegated handler arrives as a JSON body with a non-200
// status. The browser already holds an open 200 stream, so it has to be
// re-reported as an event rather than a status.
func TestRecorderReportsNonStreamingFailure(t *testing.T) {
	rec := httptest.NewRecorder()
	recorder := newTurnRecorder(newSSEWriter(rec), protocolOpenAI, false)
	recorder.Header().Set("Content-Type", "application/json")
	recorder.WriteHeader(http.StatusForbidden)
	_, _ = recorder.Write([]byte(`{"error":{"type":"hook_blocked","message":"blocked by policy"}}`))

	errType, message, failed := recorder.Failed()
	if !failed {
		t.Fatal("expected the recorder to report a failure")
	}
	if errType != "hook_blocked" || message != "blocked by policy" {
		t.Errorf("errType/message = %q/%q", errType, message)
	}

	recorder.emitError(errType, message)
	if !strings.Contains(rec.Body.String(), "event: blocked") {
		t.Errorf("a hook refusal must be a blocked event, not an error:\n%s", rec.Body.String())
	}
}

// After an error event no further token may reach the browser: it would render
// as trailing text under a turn the UI has already marked failed, while the
// late content must not be appended to the partial reply that is persisted.
func TestRecorderStopsDeltasAfterError(t *testing.T) {
	rec := httptest.NewRecorder()
	recorder := newTurnRecorder(newSSEWriter(rec), protocolOpenAI, false)
	recorder.Header().Set("Content-Type", "text/event-stream")
	recorder.WriteHeader(http.StatusOK)

	recorder.emitError("upstream_error", "boom")
	recorder.emitDelta("late text")

	if strings.Contains(rec.Body.String(), "late text") {
		t.Fatalf("a delta reached the browser after an error event:\n%s", rec.Body.String())
	}
	if recorder.Text() != "" {
		t.Errorf("text = %q, late content after an error must not be persisted", recorder.Text())
	}
}

func TestRecorderReportsUpstreamErrorAsError(t *testing.T) {
	rec := httptest.NewRecorder()
	recorder := newTurnRecorder(newSSEWriter(rec), protocolOpenAI, false)
	recorder.Header().Set("Content-Type", "application/json")
	recorder.WriteHeader(http.StatusBadGateway)
	_, _ = recorder.Write([]byte(`{"error":{"type":"provider_unavailable","message":"upstream down"}}`))

	errType, message, _ := recorder.Failed()
	recorder.emitError(errType, message)
	body := rec.Body.String()
	if !strings.Contains(body, "event: error") || strings.Contains(body, "event: blocked") {
		t.Errorf("an upstream failure must be an error event:\n%s", body)
	}
}

func TestRecorderSurfacesHookAnnotations(t *testing.T) {
	rec := httptest.NewRecorder()
	recorder := newTurnRecorder(newSSEWriter(rec), protocolOpenAI, false)
	recorder.Header().Set(dataplane.HookAnnotationsHeader, `{"pii":"redacted"}`)

	annotations := recorder.Annotations()
	if annotations["pii"] != "redacted" {
		t.Errorf("annotations = %+v, want the hook's annotations", annotations)
	}
}

func TestSSEWriterSetsStreamingHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	newSSEWriter(rec)

	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	// Without this an intermediate proxy buffers the whole turn and the console
	// looks frozen until the reply finishes.
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}
}

func TestSSEWriterEmitsParseableEvents(t *testing.T) {
	rec := httptest.NewRecorder()
	sse := newSSEWriter(rec)
	sse.Event(eventDone, map[string]any{"status": "ok", "message_id": "wmsg_1"})

	body := rec.Body.String()
	if !strings.HasPrefix(body, "event: done\ndata: ") {
		t.Fatalf("unexpected frame shape: %q", body)
	}
	var payload map[string]any
	data := strings.TrimSuffix(strings.TrimPrefix(body, "event: done\ndata: "), "\n\n")
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("event payload is not valid JSON: %v", err)
	}
	if payload["message_id"] != "wmsg_1" {
		t.Errorf("payload = %+v", payload)
	}
}

// --- misc pure helpers ---

func TestModelAcceptsImages(t *testing.T) {
	route := testRoute(providers.ProviderOpenAI, "/openai")
	if !modelAcceptsImages(route.Provider, "vision-model") {
		t.Error("vision-model declares image input")
	}
	if modelAcceptsImages(route.Provider, "text-model") {
		t.Error("text-model does not declare image input")
	}
	if modelAcceptsImages(route.Provider, "unknown-model") {
		t.Error("an unknown model must not be assumed to accept images")
	}
}

func TestFirstLineAndTruncate(t *testing.T) {
	if got := firstLine("first\nsecond"); got != "first" {
		t.Errorf("firstLine = %q", got)
	}
	if got := truncate("  padded  ", 100); got != "padded" {
		t.Errorf("truncate trimmed incorrectly: %q", got)
	}
	if got := truncate("abcdefghij", 4); got != "abcd" {
		t.Errorf("truncate = %q, want abcd", got)
	}
	// A multi-byte character at the cut boundary must not be split: titles and
	// filenames are rendered directly in the UI.
	if got := truncate("héllo world", 3); got != "hé" {
		t.Errorf("truncate split a rune: %q", got)
	}
}

// --- console protocol support ---

// The catalog the console serves and the protocols a console turn can invoke
// are two halves of one contract. If they drift, the picker offers a model that
// is refused at send time — which is exactly what this pair exists to prevent.
func TestConsoleAPITypesMatchSupportedProtocols(t *testing.T) {
	for _, apiType := range consoleAPITypes() {
		if _, ok := consoleProtocol(providers.ProviderType(apiType)); !ok {
			t.Errorf("the console advertises api_type %q but cannot invoke it", apiType)
		}
	}

	advertised := make(map[string]bool, len(consoleAPITypes()))
	for _, apiType := range consoleAPITypes() {
		advertised[apiType] = true
	}
	for _, providerType := range []providers.ProviderType{
		providers.ProviderOpenAI,
		providers.ProviderAnthropic,
		providers.ProviderDeepSeek,
		providers.ProviderOpenAIResponses,
		providers.ProviderOpenAICodexResponses,
		providers.ProviderBedrockConverse,
		providers.ProviderGoogleVertex,
		providers.ProviderGoogleGenerativeAI,
		providers.ProviderMistralConversations,
		providers.ProviderAzureOpenAIResponses,
	} {
		_, supported := consoleProtocol(providerType)
		if supported != advertised[string(providerType)] {
			t.Errorf("provider type %q: invokable=%v but advertised=%v",
				providerType, supported, advertised[string(providerType)])
		}
	}
}

func TestConsoleProtocolPicksTheRightHandler(t *testing.T) {
	if proto, ok := consoleProtocol(providers.ProviderAnthropic); !ok || proto != protocolAnthropic {
		t.Errorf("anthropic-messages resolved to %v/%v", proto, ok)
	}
	for _, providerType := range []providers.ProviderType{providers.ProviderOpenAI, providers.ProviderDeepSeek} {
		if proto, ok := consoleProtocol(providerType); !ok || proto != protocolOpenAI {
			t.Errorf("%s resolved to %v/%v", providerType, proto, ok)
		}
	}
}

// --- turns that end without a delivered reply ---

// A stopped turn's closing writes run when the request context is already
// cancelled. Reusing it would mean the partial reply is never stored, which is
// the whole failure this path exists to avoid.
func TestDetachedContextOutlivesTheRequest(t *testing.T) {
	authCtx := &auth.Context{TenantID: "tnt_1", UserID: "usr_1"}
	outer := httptest.NewRequest(http.MethodPost, "/web/v1/x", nil)
	outer = outer.WithContext(middleware.WithAuthContext(outer.Context(), authCtx))
	cancellable, cancel := context.WithCancel(outer.Context())
	outer = outer.WithContext(cancellable)
	cancel()

	if outer.Context().Err() == nil {
		t.Fatal("the request context should be cancelled for this test to mean anything")
	}

	ctx, release := detachedContext(outer)
	defer release()

	if err := ctx.Err(); err != nil {
		t.Errorf("detached context is already done: %v", err)
	}
	if middleware.GetAuthContext(ctx) == nil {
		t.Error("the detached context lost the request's values")
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > detachedTurnTimeout {
		t.Errorf("detached context deadline = %v, want a bound of at most %v", deadline, detachedTurnTimeout)
	}
}

// A hook's annotations describe the same reply, so labelling it stopped must
// not throw them away.
func TestStoppedAnnotationsKeepHookAnnotations(t *testing.T) {
	hook := map[string]string{"pii": "redacted"}
	annotations := stoppedAnnotations(hook)

	if annotations["pii"] != "redacted" {
		t.Errorf("annotations = %+v, want the hook's annotations preserved", annotations)
	}
	if annotations["stopped"] == "" {
		t.Error("a stopped reply must say so, or it reads as a complete answer")
	}
	if _, mutated := hook["stopped"]; mutated {
		t.Error("stoppedAnnotations mutated the recorder's own map")
	}
	if got := stoppedAnnotations(nil); got["stopped"] == "" {
		t.Error("a turn with no hook annotations must still be labelled")
	}
}

// The prompt is stored before the model is called, so a refusal leaves it with
// no answer. The reason goes on the prompt itself rather than nowhere.
func TestUndeliveredAnnotationsPreferTheReadableMessage(t *testing.T) {
	if got := undeliveredAnnotations("quota_exceeded", "daily spend limit reached")["undelivered"]; got != "daily spend limit reached" {
		t.Errorf("undelivered = %q, want the human-readable message", got)
	}
	if got := undeliveredAnnotations("quota_exceeded", "")["undelivered"]; got != "quota_exceeded" {
		t.Errorf("undelivered = %q, want the error type when there is no message", got)
	}
	if got := undeliveredAnnotations("", "")["undelivered"]; got == "" {
		t.Error("an undelivered prompt must carry some reason")
	}
}

// A buffered turn withholds its deltas until a mutating hook has reviewed them.
// The browser was shown nothing, so keeping the draft because the user pressed
// Stop would store exactly the unreviewed output the policy exists to withhold.
func TestStoppedBufferedTurnIsNotStored(t *testing.T) {
	if storePartialReply(true, "unreviewed draft") {
		t.Error("a buffered turn's unreviewed text must not be persisted on a stop")
	}
	if !storePartialReply(false, "streamed text") {
		t.Error("a streamed turn's partial text was already shown, so it must be kept")
	}
	if storePartialReply(false, "   ") {
		t.Error("a stop before any text arrived has nothing to store")
	}
	if stoppedWithoutReply(true) == stoppedWithoutReply(false) {
		t.Error("a withheld review and an empty reply are different explanations")
	}
}
