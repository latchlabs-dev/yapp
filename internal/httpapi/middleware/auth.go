package middleware

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/errors"
	"github.com/earlgrey/piproxy/internal/logging"
	"github.com/earlgrey/piproxy/internal/metrics"
	"github.com/earlgrey/piproxy/internal/store/postgres"
	redisstore "github.com/earlgrey/piproxy/internal/store/redis"
)

type authContextKey string

const (
	authCtxKey     authContextKey = "auth_context"
	authHeaderKey  string         = "Authorization"
	proxyKeyHeader string         = "X-Corp-Proxy-Key"
	apiKeyHeader   string         = "x-api-key"
)

// lastUsedUpdater is the narrow seam the update worker depends on. *postgres.KeyStore
// satisfies it; tests inject a stub (e.g. one that panics) to exercise the
// goroutine's panic recovery without a real database.
type lastUsedUpdater interface {
	UpdateLastUsed(ctx context.Context, keyID string) error
}

// AuthMiddleware authenticates requests using proxy API keys.
type AuthMiddleware struct {
	store           *postgres.KeyStore
	lastUsed        lastUsedUpdater
	revoked         *sync.Map
	updateCh        chan string
	updateWg        sync.WaitGroup
	revocationCache *redisstore.RevocationCache
	authMetrics     *metrics.AuthMetrics
}

// NewAuthMiddleware creates a new auth middleware with optional pepper.
func NewAuthMiddleware(store *postgres.KeyStore, pepper string, revocationCache *redisstore.RevocationCache, authMetrics *metrics.AuthMetrics) *AuthMiddleware {
	auth.SetPepper(pepper)
	m := &AuthMiddleware{
		store:           store,
		lastUsed:        store,
		revoked:         &sync.Map{},
		updateCh:        make(chan string, 1000),
		revocationCache: revocationCache,
		authMetrics:     authMetrics,
	}
	m.updateWg.Add(1)
	go m.updateLoop()
	return m
}

// updateLoop processes last-used updates on a bounded goroutine.
func (m *AuthMiddleware) updateLoop() {
	defer m.updateWg.Done()
	for keyID := range m.updateCh {
		m.processUpdate(keyID)
	}
}

// processUpdate performs a single last-used update under a per-iteration panic
// guard. Recovering here (rather than in updateLoop) means one bad keyID or a
// driver/store panic skips only that update and the worker goroutine stays
// alive to process the rest of the channel; without this guard a single panic
// would kill the goroutine and silently drop all subsequent last-used updates
// for the lifetime of the process.
func (m *AuthMiddleware) processUpdate(keyID string) {
	defer func() {
		if rec := recover(); rec != nil {
			// AuthMiddleware has no injected logger; best-effort log on the
			// zerolog global so the dropped update is at least observable.
			log.Error().Interface("panic", rec).Str("key_id", keyID).
				Msg("auth.update_last_used panic recovered")
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = m.lastUsed.UpdateLastUsed(ctx, keyID)
}

// Authenticate extracts and validates the proxy API key, logging each step.
func (m *AuthMiddleware) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log := GetRequestLogger(r.Context())
		timing := logging.StartTiming()
		authSource := "none"

		rawKey := auth.ExtractKey(
			r.Header.Get(authHeaderKey),
			r.Header.Get(proxyKeyHeader),
			r.Header.Get(apiKeyHeader),
		)

		switch {
		case r.Header.Get(proxyKeyHeader) != "":
			authSource = "x-corp-proxy-key"
		case strings.HasPrefix(r.Header.Get(authHeaderKey), "Bearer "):
			authSource = "bearer"
		case r.Header.Get(apiKeyHeader) != "":
			authSource = "x-api-key"
		}

		if rawKey == "" {
			logging.RequestEvent(log, "auth.missing", logging.Fields{KeyPrefix: authSource})
			m.authMetrics.ObserveAuth("missing")
			errors.WriteErrorJSON(w, errors.ErrorUnauthorized, "missing API key")
			return
		}

		prefix := auth.PrefixFromKey(rawKey)
		if prefix == "" {
			logging.RequestEvent(log, "auth.invalid_format", logging.Fields{KeyPrefix: authSource})
			m.authMetrics.ObserveAuth("invalid_format")
			errors.WriteErrorJSON(w, errors.ErrorUnauthorized, "invalid API key format")
			return
		}

		// Fast path: check revocation cache before hitting Postgres.
		if m.revocationCache != nil && m.revocationCache.IsRevoked(r.Context(), prefix) {
			logging.RequestEvent(log, "auth.revoked_cache", logging.Fields{KeyPrefix: prefix})
			m.authMetrics.ObserveAuth("revoked_cache")
			errors.WriteErrorJSON(w, errors.ErrorForbidden, "API key has been revoked")
			return
		}

		keyRecord, err := m.store.GetByPrefix(r.Context(), prefix)
		if err != nil {
			logging.RequestEvent(log, "auth.not_found", logging.Fields{KeyPrefix: prefix})
			m.authMetrics.ObserveAuth("not_found")
			errors.WriteErrorJSON(w, errors.ErrorUnauthorized, "invalid API key")
			return
		}

		if !auth.VerifyKey(rawKey, keyRecord.KeyHash) {
			logging.RequestEvent(log, "auth.hash_mismatch", logging.Fields{
				KeyPrefix: prefix,
				UserID:    keyRecord.UserID,
				TenantID:  keyRecord.TenantID,
			})
			m.authMetrics.ObserveAuth("hash_mismatch")
			errors.WriteErrorJSON(w, errors.ErrorUnauthorized, "invalid API key")
			return
		}

		if keyRecord.Status == "revoked" || keyRecord.Status == "disabled" {
			logging.RequestEvent(log, "auth.revoked", logging.Fields{
				KeyPrefix: prefix,
				UserID:    keyRecord.UserID,
				TenantID:  keyRecord.TenantID,
			})
			m.authMetrics.ObserveAuth("revoked")
			errors.WriteErrorJSON(w, errors.ErrorForbidden, "API key is revoked or disabled")
			return
		}

		if keyRecord.ExpiresAt != nil && time.Now().After(*keyRecord.ExpiresAt) {
			logging.RequestEvent(log, "auth.expired", logging.Fields{
				KeyPrefix: prefix,
				UserID:    keyRecord.UserID,
				TenantID:  keyRecord.TenantID,
			})
			m.authMetrics.ObserveAuth("expired")
			errors.WriteErrorJSON(w, errors.ErrorUnauthorized, "API key has expired")
			return
		}

		authCtx := &auth.Context{
			TenantID:  keyRecord.TenantID,
			UserID:    keyRecord.UserID,
			KeyID:     keyRecord.ID,
			KeyPrefix: prefix,
			Scopes:    keyRecord.Scopes,
		}

		logging.RequestEvent(log, "auth.ok", logging.Fields{
			TenantID:   authCtx.TenantID,
			UserID:     authCtx.UserID,
			KeyPrefix:  authCtx.KeyPrefix,
			DurationMs: timing.Elapsed(),
		})
		m.authMetrics.ObserveAuth("ok")

		// Enqueue last-used update on bounded worker goroutine.
		select {
		case m.updateCh <- keyRecord.ID:
		default:
			// Channel full, skip update to avoid blocking the request.
		}

		reqCtx := context.WithValue(r.Context(), authCtxKey, authCtx)
		next.ServeHTTP(w, r.WithContext(reqCtx))
	})
}

// GetAuthContext retrieves the auth context from the request.
func GetAuthContext(ctx context.Context) *auth.Context {
	if a, ok := ctx.Value(authCtxKey).(*auth.Context); ok {
		return a
	}
	return nil
}

// WithAuthContext attaches an already-validated auth context. It is intended
// for internal tests and non-HTTP call paths that reuse data-plane helpers.
func WithAuthContext(ctx context.Context, authCtx *auth.Context) context.Context {
	return context.WithValue(ctx, authCtxKey, authCtx)
}

// RequireScope returns a middleware that checks for a specific scope.
func RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authCtx := GetAuthContext(r.Context())
			if authCtx == nil {
				logging.RequestEvent(GetRequestLogger(r.Context()), "auth.scope_missing", logging.Fields{})
				errors.WriteErrorJSON(w, errors.ErrorUnauthorized, "authentication required")
				return
			}
			if !authCtx.HasScope(scope) {
				logging.RequestEvent(GetRequestLogger(r.Context()), "auth.scope_denied", logging.Fields{
					TenantID: authCtx.TenantID,
					UserID:   authCtx.UserID,
				})
				errors.WriteErrorJSON(w, errors.ErrorForbidden, "insufficient permissions")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAnyScope returns a middleware that allows requests with any listed scope.
func RequireAnyScope(scopes ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authCtx := GetAuthContext(r.Context())
			if authCtx == nil {
				logging.RequestEvent(GetRequestLogger(r.Context()), "auth.scope_missing", logging.Fields{})
				errors.WriteErrorJSON(w, errors.ErrorUnauthorized, "authentication required")
				return
			}
			if !authCtx.HasAnyScope(scopes...) {
				logging.RequestEvent(GetRequestLogger(r.Context()), "auth.scope_denied", logging.Fields{
					TenantID: authCtx.TenantID,
					UserID:   authCtx.UserID,
				})
				errors.WriteErrorJSON(w, errors.ErrorForbidden, "insufficient permissions")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (m *AuthMiddleware) String() string {
	return "auth-middleware"
}

// HasAuthHeader checks if the request contains any auth headers.
func HasAuthHeader(r *http.Request) bool {
	return r.Header.Get(authHeaderKey) != "" ||
		r.Header.Get(proxyKeyHeader) != "" ||
		r.Header.Get(apiKeyHeader) != ""
}

// StripAuthHeaders removes all incoming auth headers from the request.
func StripAuthHeaders(r *http.Request) {
	r.Header.Del(authHeaderKey)
	r.Header.Del(proxyKeyHeader)
	r.Header.Del(apiKeyHeader)
	for key := range r.Header {
		lower := strings.ToLower(key)
		// Exact-match known auth headers to avoid false positives
		// on things like X-Request-Token-Count, X-Idempotency-Token, etc.
		switch lower {
		case "authorization",
			"proxy-authorization",
			"x-corp-proxy-key",
			"x-api-key",
			"api-key",
			"x-auth-token",
			"cookie",
			"set-cookie":
			r.Header.Del(key)
		}
	}
}
