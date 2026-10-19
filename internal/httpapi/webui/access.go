package webui

import (
	"context"
	"net/http"
	"time"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/providers"
)

// requireScope guards a route with the session's snapshotted scopes. The
// console mounts outside the API-key auth group, so the CLI routes'
// RequireAnyScope middleware never runs here; without an explicit check the
// scope set an admin grants to a domain would be decorative, and a session
// minted with the read-only baseline could still run turns and write memory.
func requireScope(scopes ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authCtx := middleware.GetAuthContext(r.Context())
			if authCtx == nil || !authCtx.HasAnyScope(scopes...) {
				writeError(w, http.StatusForbidden, "forbidden", "your account is not permitted to use this feature")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// turnScopes follows the resolved provider family, independently of its wire
// protocol: DeepSeek and OpenAI both use chat completions, but have different
// permissions.
func turnScopes(family providers.ProviderType) []string {
	switch family {
	case providers.ProviderAnthropic:
		return []string{auth.ScopeLLMInvoke, auth.ScopeLLMInvokeAnthropic}
	case providers.ProviderOpenAI:
		return []string{auth.ScopeLLMInvoke, auth.ScopeLLMInvokeOpenAI}
	case providers.ProviderDeepSeek:
		return []string{auth.ScopeLLMInvoke, auth.ScopeLLMInvokeDeepSeek}
	default:
		return nil
	}
}

// allTurnScopes is the union across protocols, for routes that run before the
// model (and therefore the protocol) is known.
func allTurnScopes() []string {
	scopes := turnScopes(providers.ProviderAnthropic)
	return append(scopes, auth.ScopeLLMInvokeOpenAI, auth.ScopeLLMInvokeDeepSeek)
}

// hasTurnScope reports whether the session may invoke the resolved provider
// family. The family is only known after route resolution, so this is
// checked in the handler rather than as route middleware.
func hasTurnScope(authCtx *auth.Context, family providers.ProviderType) bool {
	return authCtx != nil && authCtx.HasAnyScope(turnScopes(family)...)
}

// checkWriteBudget bounds how many console writes one user may make per
// process per minute. It exists because the user's message is persisted before
// the delegated handler checks quota, so an exhausted or abusive account could
// otherwise keep growing web_messages one denied turn at a time. A limiter or
// limit that is absent degrades open: this is a growth guard, not a billing
// control.
func (deps *Dependencies) checkWriteBudget(ctx context.Context, authCtx *auth.Context) bool {
	if deps.WriteLimiter == nil || deps.Config.WriteRequestsPerMinute <= 0 || authCtx == nil {
		return true
	}
	key := "web:write:" + authCtx.TenantID + ":" + authCtx.UserID
	count, err := deps.WriteLimiter.Increment(ctx, key, time.Minute)
	if err != nil {
		return true
	}
	return count <= deps.Config.WriteRequestsPerMinute
}
