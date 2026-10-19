package admin

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/sessionarchive"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type fakeSessionArchive struct {
	states     []*sessionarchive.State
	tenants    []pgstore.ConversationTenant
	turns      []sessionarchive.Turn
	costs      map[string]float64
	ips        []pgstore.SessionIP
	page       *sessionarchive.TranscriptPage
	pageArgs   []any
	rebuilds   []string
	rebuilding bool
	pageErr    error
}

const fakeObjectsLocation = "gcs://bucket/prod"

func (f *fakeSessionArchive) ObjectsLocation() string { return fakeObjectsLocation }

func (f *fakeSessionArchive) FindTranscripts(context.Context, string) ([]*sessionarchive.State, error) {
	return f.states, nil
}
func (f *fakeSessionArchive) ConversationTenants(context.Context, string) ([]pgstore.ConversationTenant, error) {
	return f.tenants, nil
}
func (f *fakeSessionArchive) ListTurns(context.Context, string, int) ([]sessionarchive.Turn, error) {
	return f.turns, nil
}
func (f *fakeSessionArchive) TurnCosts(context.Context, string, int) (map[string]float64, error) {
	return f.costs, nil
}
func (f *fakeSessionArchive) TurnIPs(context.Context, string, int) ([]pgstore.SessionIP, error) {
	return f.ips, nil
}
func (f *fakeSessionArchive) OwnerNames(_ context.Context, tenantID, userID string) (pgstore.OwnerNames, error) {
	return pgstore.OwnerNames{TenantName: "Name of " + tenantID, UserEmail: userID + "@example.com"}, nil
}
func (f *fakeSessionArchive) TranscriptPage(_ context.Context, tenantID, conversationID string, fromSeq, limit int) (*sessionarchive.TranscriptPage, *sessionarchive.State, error) {
	f.pageArgs = []any{tenantID, conversationID, fromSeq, limit}
	if f.pageErr != nil {
		return nil, nil, f.pageErr
	}
	if f.page == nil {
		return nil, nil, sessionarchive.ErrTranscriptNotFound
	}
	return f.page, &sessionarchive.State{Generation: 3}, nil
}
func (f *fakeSessionArchive) StartRebuild(tenantID, conversationID string) bool {
	f.rebuilds = append(f.rebuilds, tenantID+"/"+conversationID)
	return !f.rebuilding
}
func (f *fakeSessionArchive) Rebuilding(string, string) bool { return f.rebuilding }

func serveAdmin(t *testing.T, archive SessionArchive, method, target string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	router := NewRouter(&Dependencies{Logger: zerolog.Nop(), AdminKey: "admin-test-key", SessionArchive: archive})
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("X-Admin-Key", "admin-test-key")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestGetSessionArchiveServesMetadataTurnsAndCosts(t *testing.T) {
	now := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	archive := &fakeSessionArchive{
		states: []*sessionarchive.State{
			{TranscriptID: "strn_old", TenantID: "tn_other", ConversationID: "conv", LastActivityAt: now.Add(-48 * time.Hour), Generation: 1},
			{TranscriptID: "strn_1", TenantID: "tn_a", UserID: "usr_1", ConversationID: "conv", OpeningPrompt: "fix the parser",
				RequestCount: 2, UserTurnCount: 1, StartedAt: now.Add(-time.Hour), LastActivityAt: now, Generation: 2, LastSeq: 2,
				MergedBytes: 100, SegmentBytes: 20, ObjectsLocation: "gcs://bucket/prod"},
		},
		tenants: []pgstore.ConversationTenant{{TenantID: "tn_a", TenantName: "A", LastPayloadAt: now, RequestCount: 2}},
		turns: []sessionarchive.Turn{
			{Generation: 2, Seq: 1, RequestID: "req_1", Kind: sessionarchive.KindExtend, ToolNames: []string{"bash"}},
			{Generation: 2, Seq: 2, RequestID: "req_2", Kind: sessionarchive.KindReset, ToolNames: []string{}},
		},
		costs: map[string]float64{"req_1": 1.5, "req_2": 2.25},
		ips: []pgstore.SessionIP{
			{IP: "100.64.0.10", Requests: 2, FirstSeen: now.Add(-time.Hour), LastSeen: now},
			{IP: "192.0.2.9", Requests: 1, FirstSeen: now.Add(-30 * time.Minute), LastSeen: now.Add(-29 * time.Minute)},
		},
	}

	rec := serveAdmin(t, archive, http.MethodGet, "/sessions/conv", map[string]string{"Accept-Encoding": "gzip"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("admin JSON must be gzip-compressed for gzip clients")
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)

	var resp struct {
		TenantID   string `json:"tenant_id"`
		Rebuilding bool   `json:"rebuilding"`
		Session    struct {
			OpeningPrompt   string  `json:"opening_prompt"`
			TenantName      string  `json:"tenant_name"`
			UserEmail       string  `json:"user_email"`
			CostTotalCents  float64 `json:"cost_total_cents"`
			TranscriptBytes int64   `json:"transcript_bytes"`
		} `json:"session"`
		Turns []struct {
			Seq            int      `json:"seq"`
			Kind           string   `json:"kind"`
			CostTotalCents float64  `json:"cost_total_cents"`
			ToolNames      []string `json:"tool_names"`
		} `json:"turns"`
		Matches   []sessionMatch `json:"matches"`
		ClientIPs []struct {
			IP        string    `json:"ip"`
			Requests  int       `json:"requests"`
			FirstSeen time.Time `json:"first_seen"`
			LastSeen  time.Time `json:"last_seen"`
		} `json:"client_ips"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if resp.TenantID != "tn_a" || resp.Rebuilding || len(archive.rebuilds) != 0 {
		t.Fatalf("most recent tenant must be selected without a rebuild: %+v rebuilds=%v", resp, archive.rebuilds)
	}
	if resp.Session.OpeningPrompt != "fix the parser" || resp.Session.TenantName != "Name of tn_a" || resp.Session.UserEmail != "usr_1@example.com" ||
		resp.Session.CostTotalCents != 3.75 || resp.Session.TranscriptBytes != 120 {
		t.Fatalf("session view: %+v", resp.Session)
	}
	if len(resp.Turns) != 2 || resp.Turns[0].CostTotalCents != 1.5 || resp.Turns[1].Kind != "reset" || resp.Turns[0].ToolNames[0] != "bash" {
		t.Fatalf("turns: %+v", resp.Turns)
	}
	if len(resp.Matches) != 2 || !resp.Matches[0].HasTranscript || resp.Matches[0].RequestCount != 2 || resp.Matches[1].TenantID != "tn_other" {
		t.Fatalf("matches: %+v", resp.Matches)
	}
	if len(resp.ClientIPs) != 2 || resp.ClientIPs[0].IP != "100.64.0.10" || resp.ClientIPs[0].Requests != 2 ||
		resp.ClientIPs[1].IP != "192.0.2.9" || !resp.ClientIPs[0].LastSeen.Equal(now) {
		t.Fatalf("client ips: %+v", resp.ClientIPs)
	}

	other := serveAdmin(t, archive, http.MethodGet, "/sessions/conv?tenant_id=tn_other", nil)
	if other.Code != http.StatusOK || len(archive.rebuilds) != 0 {
		t.Fatalf("explicit tenant without captures: status %d rebuilds %v", other.Code, archive.rebuilds)
	}
	if missing := serveAdmin(t, archive, http.MethodGet, "/sessions/conv?tenant_id=tn_nobody", nil); missing.Code != http.StatusNotFound {
		t.Fatalf("unknown tenant status %d", missing.Code)
	}
}

func TestGetSessionArchiveStartsRebuildWhenMissingOrStale(t *testing.T) {
	now := time.Now()
	missing := &fakeSessionArchive{tenants: []pgstore.ConversationTenant{{TenantID: "tn_a", LastPayloadAt: now}}}
	rec := serveAdmin(t, missing, http.MethodGet, "/sessions/conv", nil)
	if rec.Code != http.StatusAccepted || len(missing.rebuilds) != 1 || missing.rebuilds[0] != "tn_a/conv" {
		t.Fatalf("missing transcript: status %d rebuilds %v body %s", rec.Code, missing.rebuilds, rec.Body)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["rebuilding"] != true || body["session"] != nil {
		t.Fatalf("202 body: %v", body)
	}

	stale := &fakeSessionArchive{
		states:  []*sessionarchive.State{{TranscriptID: "strn", TenantID: "tn_a", NeedsRebuild: true, LastActivityAt: now}},
		tenants: []pgstore.ConversationTenant{{TenantID: "tn_a", LastPayloadAt: now}},
		costs:   map[string]float64{},
	}
	rec = serveAdmin(t, stale, http.MethodGet, "/sessions/conv", nil)
	if rec.Code != http.StatusOK || len(stale.rebuilds) != 1 {
		t.Fatalf("stale transcript: status %d rebuilds %v", rec.Code, stale.rebuilds)
	}

	running := &fakeSessionArchive{tenants: missing.tenants, rebuilding: true}
	rec = serveAdmin(t, running, http.MethodGet, "/sessions/conv", nil)
	if rec.Code != http.StatusAccepted || len(running.rebuilds) != 0 {
		t.Fatalf("an in-flight rebuild must not be restarted: status %d rebuilds %v", rec.Code, running.rebuilds)
	}

	// A transcript written to another object store (before a backend switch)
	// is reported unreadable and rebuilt, rather than read and failing.
	foreign := &fakeSessionArchive{
		states:  []*sessionarchive.State{{TranscriptID: "strn", TenantID: "tn_a", ObjectsLocation: "postgres", LastActivityAt: now}},
		tenants: []pgstore.ConversationTenant{{TenantID: "tn_a", LastPayloadAt: now}},
		costs:   map[string]float64{},
	}
	rec = serveAdmin(t, foreign, http.MethodGet, "/sessions/conv", nil)
	var foreignBody map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &foreignBody)
	if rec.Code != http.StatusOK || len(foreign.rebuilds) != 1 || foreignBody["transcript_readable"] != false || foreignBody["rebuilding"] != true {
		t.Fatalf("foreign-store transcript: status %d rebuilds %v body %v", rec.Code, foreign.rebuilds, foreignBody)
	}
	current := &fakeSessionArchive{
		states:  []*sessionarchive.State{{TranscriptID: "strn", TenantID: "tn_a", ObjectsLocation: fakeObjectsLocation, LastActivityAt: now}},
		tenants: foreign.tenants,
		costs:   map[string]float64{},
	}
	rec = serveAdmin(t, current, http.MethodGet, "/sessions/conv", nil)
	var currentBody map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &currentBody)
	if currentBody["transcript_readable"] != true || len(current.rebuilds) != 0 {
		t.Fatalf("current-store transcript: body %v rebuilds %v", currentBody, current.rebuilds)
	}

	if rec := serveAdmin(t, &fakeSessionArchive{}, http.MethodGet, "/sessions/unknown", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown conversation status %d", rec.Code)
	}
	if rec := serveAdmin(t, nil, http.MethodGet, "/sessions/conv", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured archive status %d", rec.Code)
	}
}

func TestSessionTranscriptPageAndRebuildEndpoints(t *testing.T) {
	next := 51
	archive := &fakeSessionArchive{
		states: []*sessionarchive.State{{TenantID: "tn_a"}},
		page:   &sessionarchive.TranscriptPage{Turns: []sessionarchive.TranscriptTurn{{Seq: 1, Items: []sessionarchive.DisplayItem{}, Output: []sessionarchive.DisplayItem{}}}, NextSeq: &next, LastSeq: 80},
	}
	rec := serveAdmin(t, archive, http.MethodGet, "/sessions/conv/transcript?from_seq=1&limit=50", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var page struct {
		Turns      []map[string]any `json:"turns"`
		NextSeq    *int             `json:"next_seq"`
		LastSeq    int              `json:"last_seq"`
		Generation int              `json:"generation"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Turns) != 1 || page.NextSeq == nil || *page.NextSeq != 51 || page.LastSeq != 80 || page.Generation != 3 {
		t.Fatalf("page: %+v", page)
	}
	if archive.pageArgs[0] != "tn_a" || archive.pageArgs[2] != 1 || archive.pageArgs[3] != 50 {
		t.Fatalf("page args: %v", archive.pageArgs)
	}

	serveAdmin(t, archive, http.MethodGet, "/sessions/conv/transcript?tenant_id=&from_seq=bogus", nil)
	if archive.pageArgs[0] != "" || archive.pageArgs[2] != 1 || archive.pageArgs[3] != sessionarchive.DefaultPageTurns {
		t.Fatalf("defaults / explicit empty tenant: %v", archive.pageArgs)
	}

	if rec := serveAdmin(t, &fakeSessionArchive{states: archive.states}, http.MethodGet, "/sessions/conv/transcript", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("missing transcript status %d", rec.Code)
	}
	rebuildingPage := &fakeSessionArchive{states: archive.states, pageErr: sessionarchive.ErrTranscriptRebuilding}
	if rec := serveAdmin(t, rebuildingPage, http.MethodGet, "/sessions/conv/transcript", nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "transcript_rebuilding") {
		t.Fatalf("rebuilding transcript: status %d body %s", rec.Code, rec.Body)
	}

	rebuild := &fakeSessionArchive{tenants: []pgstore.ConversationTenant{{TenantID: "tn_a", LastPayloadAt: time.Now()}}}
	rec = serveAdmin(t, rebuild, http.MethodPost, "/sessions/conv/rebuild", nil)
	if rec.Code != http.StatusAccepted || len(rebuild.rebuilds) != 1 {
		t.Fatalf("rebuild: status %d rebuilds %v", rec.Code, rebuild.rebuilds)
	}
	if rec := serveAdmin(t, &fakeSessionArchive{}, http.MethodPost, "/sessions/conv/rebuild", nil); rec.Code != http.StatusConflict {
		t.Fatalf("rebuild without captures status %d", rec.Code)
	}
}
