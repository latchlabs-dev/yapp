package webui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/httpapi/openai"
	"github.com/earlgrey/piproxy/internal/id"
	"github.com/earlgrey/piproxy/internal/quota"
	"github.com/earlgrey/piproxy/internal/rewrite"
	"github.com/earlgrey/piproxy/internal/secrets"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
	redisstore "github.com/earlgrey/piproxy/internal/store/redis"
	"github.com/earlgrey/piproxy/internal/upstream"
	"github.com/earlgrey/piproxy/internal/usage"
	"github.com/earlgrey/piproxy/internal/webauth"
)

// This gate uses real Postgres + Redis, the cookie resolver, the complete web
// router and the delegated provider handler. A second router proves that a key's
// limit survives replica changes, while another key for the SAME user still works.
func TestKeyCookieDelegatedTurnQuotaAndPersistedUsage(t *testing.T) {
	address := os.Getenv("PIPROXY_TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("PIPROXY_TEST_REDIS_ADDRESS is not set")
	}
	var calls atomic.Int64
	rig := newWebChatRig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("browser credential forwarded upstream")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"complete\"}}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":4,\"total_tokens\":16}}\n\ndata: [DONE]\n\n")
	}), nil)
	ctx := context.Background()
	counter, err := redisstore.New(ctx, redisstore.Config{Address: address})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = counter.Close() })
	if _, err := rig.pool.Exec(ctx, `INSERT INTO provider_accounts (id, provider, name) VALUES ($1, 'openai', 'Key-cookie integration test')`, rig.conv.ProviderID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, query := range []string{`DELETE FROM usage_events WHERE tenant_id=$1`, `DELETE FROM api_keys WHERE tenant_id=$1`} {
			if _, err := rig.pool.Exec(ctx, query, rig.conv.TenantID); err != nil {
				t.Error(err)
			}
		}
		if _, err := rig.pool.Exec(ctx, `DELETE FROM provider_accounts WHERE id=$1`, rig.conv.ProviderID); err != nil {
			t.Error(err)
		}
	})
	keyStore := pgstore.NewKeyStore(rig.pool)
	var raws, keyIDs []string
	for i := 0; i < 2; i++ {
		raw, prefix, hash, err := auth.GenerateKey("")
		if err != nil {
			t.Fatal(err)
		}
		record := &pgstore.KeyRecord{ID: id.New("key_webchat"), TenantID: rig.conv.TenantID, UserID: rig.conv.UserID, Prefix: prefix, KeyHash: hash, Status: "active", Scopes: []string{auth.ScopeLLMInvoke}, CreatedAt: time.Now()}
		if err := keyStore.Insert(ctx, record); err != nil {
			t.Fatal(err)
		}
		raws, keyIDs = append(raws, raw), append(keyIDs, record.ID)
	}
	manager := quota.NewManager()
	manager.SetPolicies([]*quota.Policy{{ID: id.New("pol_webchat"), TenantID: rig.conv.TenantID, ScopeType: quota.ScopeKey, ScopeID: keyIDs[0], Enabled: true, Limits: map[quota.LimitType]int64{quota.LimitRequestsPerMinute: 1}}})
	recorder := usage.NewRecorder(usage.NewCostTable(), nil, nil, 16, zerolog.Nop())
	t.Cleanup(recorder.Close)
	cipher, err := secrets.NewEncryptor([]byte(strings.Repeat("x", 32)))
	if err != nil {
		t.Fatal(err)
	}
	tenants, users := pgstore.NewTenantStore(rig.pool), pgstore.NewUserStore(rig.pool)
	keySessions := webauth.NewKeySessions(keyStore, tenants, users, redisstore.NewRevocationCache(counter), cipher, time.Hour)
	sessions := webauth.NewSessionManager(nil, tenants, users, nil, time.Hour, 0)
	sessions.EnableKeyLogin(keySessions)
	routers := make([]http.Handler, 0, len(rig.deps))
	for _, deps := range rig.deps {
		client := upstream.NewClient(upstream.Config{Timeout: 5 * time.Second})
		t.Cleanup(client.CloseIdleConnections)
		deps.Config.Enabled, deps.Config.BaseURL = true, "https://console.test"
		deps.Sessions, deps.KeySessions, deps.Tenants, deps.Users = sessions, keySessions, tenants, users
		deps.ChatHandler = openai.NewChatHandler(client, rewrite.NewRequestRewriter(deps.Registry), deps.Registry,
			dataplane.Services{Quota: quota.NewEnforcer(manager, counter, nil), Usage: recorder, CostTable: usage.NewCostTable()})
		router := chi.NewRouter()
		router.Use(middleware.RequestID)
		router.Mount("/web/v1", NewRouter(deps))
		routers = append(routers, router)
	}
	do := func(replica int, method, path, body, csrf string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://console.test/web/v1"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "https://console.test")
		r.Header.Set(webauth.CSRFHeader, csrf)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		routers[replica].ServeHTTP(w, r)
		return w
	}
	login := func(raw string) (*http.Cookie, string) {
		body, _ := json.Marshal(map[string]string{"key": raw})
		w := do(0, "POST", "/auth/key", string(body), "", nil)
		if w.Code != 200 {
			t.Fatalf("key login status %d", w.Code)
		}
		var cookie *http.Cookie
		for _, c := range w.Result().Cookies() {
			if c.Name == webauth.SessionCookie {
				cookie = c
			}
		}
		if cookie == nil {
			t.Fatal("no protected session cookie")
		}
		w = do(1, "GET", "/me", "", "", cookie)
		var me struct {
			CSRF string `json:"csrf_token"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &me) != nil || me.CSRF == "" {
			t.Fatalf("session restore failed: %d", w.Code)
		}
		return cookie, me.CSRF
	}
	path, body := "/conversations/"+rig.conv.ID+"/messages", `{"content":"please answer"}`
	cookie, csrf := login(raws[0])
	first := do(0, "POST", path, body, csrf, cookie)
	if first.Code != 200 || !strings.Contains(first.Body.String(), `"status":"ok"`) || calls.Load() != 1 {
		t.Fatalf("first delegated turn: %d %s", first.Code, first.Body)
	}
	denied := do(1, "POST", path, body, csrf, cookie)
	if !strings.Contains(denied.Body.String(), "quota_exceeded") || !strings.Contains(denied.Body.String(), `"status":"error"`) || calls.Load() != 1 {
		t.Fatalf("per-key quota not enforced across replicas: %d %s", denied.Code, denied.Body)
	}
	cookie, csrf = login(raws[1])
	second := do(1, "POST", path, body, csrf, cookie)
	if !strings.Contains(second.Body.String(), `"status":"ok"`) || calls.Load() != 2 {
		t.Fatalf("another key inherited the first key's limit: %d %s", second.Code, second.Body)
	}
	recorder.Close()
	store := pgstore.NewUsageStore(rig.pool)
	for event := range recorder.Events() {
		if err := store.InsertUsageEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := rig.pool.Query(ctx, `SELECT request_id, tenant_id, user_id, key_id, input_tokens, output_tokens FROM usage_events WHERE tenant_id=$1 ORDER BY created_at`, rig.conv.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	expected := map[string]string{first.Header().Get("X-Request-ID"): keyIDs[0], second.Header().Get("X-Request-ID"): keyIDs[1]}
	if len(expected) != 2 || expected[""] != "" {
		t.Fatal("successful turns did not have distinct request IDs")
	}
	seen := make(map[string]int)
	for rows.Next() {
		var requestID, tenant, user, key string
		var input, output int64
		if err := rows.Scan(&requestID, &tenant, &user, &key, &input, &output); err != nil {
			t.Fatal(err)
		}
		if expected[requestID] != key || tenant != rig.conv.TenantID || user != rig.conv.UserID || input != 12 || output != 4 {
			t.Fatalf("wrong persisted attribution/tokens for %s: %q %q %q %d %d", requestID, tenant, user, key, input, output)
		}
		seen[requestID]++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for requestID := range expected {
		if seen[requestID] != 1 {
			t.Fatalf("request %s has %d persisted usage events, want exactly one", requestID, seen[requestID])
		}
	}
	if len(seen) != 2 {
		t.Fatal("expected exactly two successful usage events and none for the denied turn")
	}
}
