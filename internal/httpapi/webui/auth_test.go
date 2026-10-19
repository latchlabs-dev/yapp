package webui

import (
	"errors"
	"testing"

	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
	"github.com/earlgrey/piproxy/internal/webauth"
)

// safeReturnPath closes the open redirect that login flows classically leak: a
// post-login `return` parameter is attacker-supplied and lands in a Location
// header, so anything that is not an in-app relative path must collapse to "/".
func TestSafeReturnPathRejectsOffSiteTargets(t *testing.T) {
	hostile := []string{
		"https://evil.example.com/steal",
		"//evil.example.com/steal",
		"/\\evil.example.com",
		"http://evil.example.com",
		"javascript:alert(1)",
		"evil.example.com",
		"",
	}
	for _, raw := range hostile {
		if got := safeReturnPath(raw); got != "/" {
			t.Errorf("safeReturnPath(%q) = %q, want / — this is an open redirect", raw, got)
		}
	}
}

func TestSafeReturnPathKeepsInAppPaths(t *testing.T) {
	cases := map[string]string{
		"/":                       "/",
		"/c/wcv_1":                "/c/wcv_1",
		"/projects":               "/projects",
		"/c/wcv_1?tab=memory":     "/c/wcv_1?tab=memory",
		"/projects/mprj_1/memory": "/projects/mprj_1/memory",
	}
	for input, want := range cases {
		if got := safeReturnPath(input); got != want {
			t.Errorf("safeReturnPath(%q) = %q, want %q", input, got, want)
		}
	}
}

// The codes the SPA renders are deliberately coarse. A user needs to know what
// to do next; nothing here should help someone enumerate tenants or accounts.
func TestLoginErrorCodesAreStableAndCoarse(t *testing.T) {
	cases := map[error]string{
		pgstore.ErrDomainNotMapped:          "domain_not_allowed",
		webauth.ErrUserInactive:             "account_inactive",
		webauth.ErrEmailNotVerified:         "email_unverified",
		webauth.ErrLoginStateMismatch:       "state_mismatch",
		errors.New("some internal failure"): "login_failed",
	}
	for err, want := range cases {
		if got := loginErrorCode(err); got != want {
			t.Errorf("loginErrorCode(%v) = %q, want %q", err, got, want)
		}
	}
}

// Wrapped errors must still classify: the session manager wraps store errors on
// the way out, and a login failure that degraded to the generic code would tell
// the user to "try again" when their domain simply is not mapped.
func TestLoginErrorCodeUnwraps(t *testing.T) {
	wrapped := errors.Join(errors.New("login: "), pgstore.ErrDomainNotMapped)

	if got := loginErrorCode(wrapped); got != "domain_not_allowed" {
		t.Errorf("loginErrorCode(wrapped) = %q, want domain_not_allowed", got)
	}
}
