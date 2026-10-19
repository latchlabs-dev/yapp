package webui

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
	"github.com/earlgrey/piproxy/internal/webauth"
	"github.com/earlgrey/piproxy/internal/weborigin"
)

// authConfigHandler tells the SPA whether login is available and what to call
// the identity provider on the button. It is the one route that must answer
// before the user has a session.
func authConfigHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":         true,
			"provider_name":   deps.Config.ProviderName,
			"api_key_enabled": deps.KeySessions != nil,
			"oidc_enabled":    deps.OIDC != nil,
		})
	}
}

// keyLoginHandler accepts only same-origin JSON. Login needs origin protection
// too: otherwise an attacker could log a victim into the attacker's account and
// collect anything the victim subsequently writes there.
func keyLoginHandler(deps *Dependencies) http.HandlerFunc {
	expectedOrigin, configErr := weborigin.Canonical(deps.Config.BaseURL)
	return func(w http.ResponseWriter, r *http.Request) {
		presentedOrigin, err := weborigin.Canonical(r.Header.Get("Origin"))
		if configErr != nil || err != nil || presentedOrigin != expectedOrigin || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeError(w, http.StatusForbidden, "forbidden", "same-origin sign-in required")
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "invalid_request", "JSON sign-in required")
			return
		}
		var input struct {
			Key string `json:"key"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid sign-in request")
			return
		}
		sess, token, err := deps.KeySessions.Login(r.Context(), strings.TrimSpace(input.Key))
		if err != nil {
			// Never log or echo the submitted key, cookie or parser errors.
			writeError(w, http.StatusUnauthorized, "unauthorized", "Use a valid personal proxy key linked to an active user.")
			return
		}
		webauth.SetSessionCookie(w, deps.Config.Cookie, token, time.Until(sess.ExpiresAt))
		deps.Logger.Info().Str("tenant_id", sess.TenantID).Str("user_id", sess.UserID).Str("key_id", sess.KeyID).Msg("web key sign-in established")
		writeJSON(w, http.StatusOK, map[string]string{"status": "signed_in"})
	}
}

// loginHandler redirects the browser into the OIDC authorization flow.
func loginHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authURL, err := deps.OIDC.StartLogin(w, safeReturnPath(r.URL.Query().Get("return")))
		if err != nil {
			deps.Logger.Error().Err(err).Msg("web login start failed")
			writeError(w, http.StatusInternalServerError, "login_failed", "could not start sign-in")
			return
		}
		http.Redirect(w, r, authURL, http.StatusFound)
	}
}

// callbackHandler completes the OIDC flow, provisions the identity, mints a
// session, and bounces the browser back into the SPA.
//
// Failures redirect to the app with an error code rather than rendering JSON:
// this is a top-level navigation, and the user needs a page, not a payload.
func callbackHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if errParam := r.URL.Query().Get("error"); errParam != "" {
			deps.Logger.Warn().Str("oidc_error", errParam).Msg("web login rejected by provider")
			redirectWithError(w, r, deps, "provider_denied")
			return
		}

		claims, returnTo, err := deps.OIDC.CompleteLogin(r.Context(), w, r)
		if err != nil {
			deps.Logger.Warn().Err(err).Msg("web login callback failed")
			redirectWithError(w, r, deps, loginErrorCode(err))
			return
		}

		sess, token, err := deps.Sessions.Login(r.Context(), claims, r)
		if err != nil {
			// Domain-not-mapped and inactive-user are policy refusals, not
			// faults: log at warn with the email domain only, never the address.
			deps.Logger.Warn().Err(err).
				Str("issuer", claims.Issuer).
				Str("email_domain", pgstore.DomainFromEmail(claims.Email)).
				Msg("web login not provisioned")
			redirectWithError(w, r, deps, loginErrorCode(err))
			return
		}

		webauth.SetSessionCookie(w, deps.Config.Cookie, token, deps.Sessions.TTL())
		deps.Logger.Info().
			Str("tenant_id", sess.TenantID).
			Str("user_id", sess.UserID).
			Msg("web session established")

		http.Redirect(w, r, deps.Config.BaseURL+safeReturnPath(returnTo), http.StatusFound)
	}
}

// logoutHandler revokes the session server-side and clears the cookie. It is a
// POST so it carries the CSRF check, and it always reports success: a caller
// asking to be signed out should never be told "you already were".
func logoutHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := deps.Sessions.Logout(r.Context(), webauth.ReadSessionCookie(r, deps.Config.Cookie)); err != nil {
			deps.Logger.Warn().Err(err).Msg("web logout failed to revoke session")
			writeError(w, http.StatusServiceUnavailable, "logout_failed", "Sign-out could not be confirmed. Please retry.")
			return
		}
		webauth.ClearSessionCookie(w, deps.Config.Cookie)
		writeJSON(w, http.StatusOK, map[string]string{"status": "signed_out"})
	}
}

// meHandler returns who the caller is, what they may do, and the CSRF token
// the SPA must echo on writes.
func meHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := middleware.GetWebSession(r.Context())
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "sign-in required")
			return
		}

		user, err := deps.Users.GetByID(r.Context(), sess.TenantID, sess.UserID)
		if err != nil {
			deps.Logger.Error().Err(err).Msg("web me: user lookup failed")
			writeError(w, http.StatusInternalServerError, "lookup_failed", "could not load your account")
			return
		}

		payload := map[string]any{
			"user": map[string]string{
				"id":           user.ID,
				"email":        user.Email,
				"display_name": user.DisplayName,
			},
			"tenant":     map[string]string{"id": sess.TenantID},
			"scopes":     sess.Scopes,
			"csrf_token": sess.CSRFToken,
			"expires_at": sess.ExpiresAt,
			"attachments": map[string]any{
				"enabled":        deps.Blobs != nil,
				"max_size_bytes": deps.Config.AttachmentMaxBytes,
			},
		}
		if deps.Tenants != nil {
			if tenant, err := deps.Tenants.GetByID(r.Context(), sess.TenantID); err == nil && tenant != nil {
				payload["tenant"] = map[string]string{"id": tenant.ID, "name": tenant.Name}
			}
		}
		writeJSON(w, http.StatusOK, payload)
	}
}

// loginErrorCode maps a login failure to a stable code for the SPA. The codes
// are deliberately coarse: the browser gets enough to show a useful message,
// and nothing that helps enumerate tenants or accounts.
func loginErrorCode(err error) string {
	switch {
	case errors.Is(err, pgstore.ErrDomainNotMapped):
		return "domain_not_allowed"
	case errors.Is(err, webauth.ErrUserInactive), errors.Is(err, webauth.ErrTenantInactive):
		return "account_inactive"
	case errors.Is(err, webauth.ErrEmailNotVerified):
		return "email_unverified"
	case errors.Is(err, webauth.ErrLoginStateMismatch):
		return "state_mismatch"
	default:
		return "login_failed"
	}
}

func redirectWithError(w http.ResponseWriter, r *http.Request, deps *Dependencies, code string) {
	target := deps.Config.BaseURL + "/login?error=" + url.QueryEscape(code)
	http.Redirect(w, r, target, http.StatusFound)
}

// safeReturnPath keeps post-login redirects inside the app. Anything that is
// not a single-slash-prefixed relative path — an absolute URL, a
// protocol-relative "//evil.example", a backslash variant — collapses to "/",
// which closes the open-redirect that login flows classically leak.
func safeReturnPath(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") {
		return "/"
	}
	if strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, "/\\") {
		return "/"
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.IsAbs() || parsed.Host != "" {
		return "/"
	}
	return parsed.RequestURI()
}
