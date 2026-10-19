package sessionarchive

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestSelectivePagePreservesRenderingAndEchoAcrossBoundary(t *testing.T) {
	all := []SegmentTurn{
		{Seq: 1, RequestID: "r1", Kind: KindExtend, InputCount: 1, Outputs: []json.RawMessage{json.RawMessage(`{"role":"assistant","content":"echo"}`)}},
		{Seq: 2, RequestID: "r2", Kind: KindExtend, Base: 1, InputCount: 3, Inputs: []IndexedItem{
			{Index: 1, Raw: json.RawMessage(`{"role":"assistant","content":"echo"}`)},
			{Index: 2, Raw: json.RawMessage(`{"role":"user","content":"next"}`)},
		}},
		{Seq: 3, RequestID: "r3", Kind: KindReset},
	}
	var window bytes.Buffer
	for i := range all[:2] {
		compressed, err := encodeSegment(&all[i])
		if err != nil {
			t.Fatal(err)
		}
		zr, err := gzip.NewReader(bytes.NewReader(compressed))
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := io.ReadAll(zr)
		_ = zr.Close()
		if err != nil {
			t.Fatal(err)
		}
		window.Write(decoded)
	}
	got, err := RenderJSONLPage(window.Bytes(), 3, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := renderPage(all, 3, 2, 1)
	if !reflect.DeepEqual(got, want) || !got.Turns[0].Items[0].Echo || got.Turns[0].Items[1].Echo {
		t.Fatalf("selective rendering changed page: %+v", got)
	}
	if _, err := RenderJSONLPage(window.Bytes(), 3, 1, 3); err == nil {
		t.Fatal("incomplete coverage was accepted")
	}
	if _, err := RenderJSONLPage([]byte(`{"t":"turn","seq":2}`), 3, 2, 1); err == nil {
		t.Fatal("missing preceding turn was accepted")
	}
	if _, err := RenderJSONLPage([]byte(`{"t":"turn","v":2,"seq":1}`), 1, 1, 1); err == nil {
		t.Fatal("unknown format version was accepted")
	}
	empty, err := RenderJSONLPage(nil, 3, 4, 50)
	if err != nil || empty.NextSeq != nil || len(empty.Turns) != 0 {
		t.Fatalf("empty page: %+v %v", empty, err)
	}
}

func TestQueryClientRejectsWrongSnapshotAndIncompleteWindow(t *testing.T) {
	state := &State{TranscriptID: "transcript", TenantID: "tenant", Generation: 1, LastSeq: 2}
	hash := sha256.Sum256([]byte(state.TenantID))
	for _, scenario := range []string{"ok", "tenant", "generation", "last_seq", "coverage", "redirect", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			server := newQueryTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer token" || r.URL.Path != "/v1/page" {
					t.Error("missing auth or wrong endpoint")
				}
				w.Header().Set("X-Transcript-ID", state.TranscriptID)
				w.Header().Set("X-Transcript-Tenant", hex.EncodeToString(hash[:]))
				w.Header().Set("X-Transcript-Generation", "1")
				w.Header().Set("X-Transcript-Last-Seq", "2")
				switch scenario {
				case "tenant":
					w.Header().Set("X-Transcript-Tenant", "other")
				case "generation":
					w.Header().Set("X-Transcript-Generation", "2")
				case "last_seq":
					w.Header().Set("X-Transcript-Last-Seq", "3")
				case "redirect":
					w.Header().Set("Location", "/other")
					w.WriteHeader(http.StatusTemporaryRedirect)
					return
				case "unavailable":
					w.WriteHeader(http.StatusConflict)
					return
				}
				if scenario != "coverage" {
					_, _ = w.Write([]byte("{\"t\":\"turn\",\"seq\":1}\n"))
				}
				_, _ = w.Write([]byte("{\"t\":\"turn\",\"seq\":2}\n"))
			}))
			defer server.Close()
			page, err := NewQueryClient(server.URL, "token", time.Second).Page(context.Background(), state, 2, 1)
			if scenario == "ok" {
				if err != nil || len(page.Turns) != 1 || page.Turns[0].Seq != 2 {
					t.Fatalf("page = %+v, %v", page, err)
				}
			} else if err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
}

func TestQueryClientCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewQueryClient("http://127.0.0.1:1", "token", time.Second).Page(ctx, &State{}, 1, 50)
	if err == nil {
		t.Fatal("cancelled request accepted")
	}
}

func newQueryTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	return server
}
