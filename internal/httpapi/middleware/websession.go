package middleware

import (
	"context"
	"crypto/subtle"
	"net/http"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/errors"
	"github.com/earlgrey/piproxy/internal/logging"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
	"github.com/earlgrey/piproxy/internal/webauth"
)

type webSessionContextKey string

const webSessionCtxKey webSessionContextKey = "web_session"

// WebSessionResolver resolves a raw cookie token to a live browser session.
// *webauth.SessionManager satisfies it; tests inject a stub.
type WebSessionResolver interface {
	Resolve(ctx context.Context, token string) (*pgstore.WebSession, error)
}

// WebSession authenticates private web-console requests from the cookie.
// Key-cookie sessions preserve KeyID for per-key quotas and usage attribution;
// OIDC sessions have no key and enforce the user's and tenant's quotas.
//
// State-changing methods additionally require the CSRF token issued at login.
// The cookie is SameSite=Lax, so this is the second of two independent checks.
func WebSession(resolver WebSessionResolver, cookieOptions webauth.CookieOptions) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log := GetRequestLogger(r.Context())

			token := webauth.ReadSessionCookie(r, cookieOptions)
			if token == "" {
				logging.RequestEvent(log, "web.session_missing", logging.Fields{})
				errors.WriteErrorJSON(w, errors.ErrorUnauthorized, "sign-in required")
				return
			}

			sess, err := resolver.Resolve(r.Context(), token)
			if err != nil {
				// Every failure mode — unknown, revoked, expired, idle — is
				// reported identically so a caller cannot probe for live tokens.
				logging.RequestEvent(log, "web.session_invalid", logging.Fields{})
				errors.WriteErrorJSON(w, errors.ErrorUnauthorized, "sign-in required")
				return
			}

			if requiresCSRF(r.Method) {
				presented := r.Header.Get(webauth.CSRFHeader)
				if sess.CSRFToken == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(sess.CSRFToken)) != 1 {
					logging.RequestEvent(log, "web.csrf_rejected", logging.Fields{
						TenantID: sess.TenantID,
						UserID:   sess.UserID,
					})
					errors.WriteErrorJSON(w, errors.ErrorForbidden, "missing or invalid CSRF token")
					return
				}
			}

			authCtx := &auth.Context{
				TenantID:   sess.TenantID,
				UserID:     sess.UserID,
				KeyID:      sess.KeyID,
				KeyPrefix:  sess.KeyPrefix,
				Scopes:     sess.Scopes,
				KeyExpires: sess.ExpiresAt,
			}
			ctx := WithAuthContext(r.Context(), authCtx)
			ctx = context.WithValue(ctx, webSessionCtxKey, sess)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// GetWebSession retrieves the browser session from the request context.
func GetWebSession(ctx context.Context) *pgstore.WebSession {
	if s, ok := ctx.Value(webSessionCtxKey).(*pgstore.WebSession); ok {
		return s
	}
	return nil
}

// requiresCSRF reports whether a method mutates state. GET/HEAD/OPTIONS are
// exempt; every web route that changes something uses one of the others.
func requiresCSRF(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}
