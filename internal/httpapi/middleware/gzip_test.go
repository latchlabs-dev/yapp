package middleware

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGzipCompressesJSONOnlyWhenAccepted(t *testing.T) {
	body := `{"turns":[` + strings.Repeat(`{"kind":"assistant","text":"hello"},`, 200) + `{}]}`
	handler := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "999")
			_, _ = io.WriteString(w, body)
		case "/text":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "plain")
		case "/empty":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNoContent)
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/json", nil)
	req.Header.Set("Accept-Encoding", "br;q=1.0, gzip;q=0.8")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("Content-Length") != "" || rec.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("headers = %v", rec.Header())
	}
	if rec.Body.Len() >= len(body) {
		t.Fatalf("compressed %d bytes, original %d", rec.Body.Len(), len(body))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _ := io.ReadAll(zr)
	if string(decoded) != body {
		t.Fatal("gzip body does not round-trip")
	}

	for _, tc := range []struct {
		path, accept string
	}{
		{"/json", ""},
		{"/json", "gzip;q=0"},
		{"/json", "identity"},
		{"/text", "gzip"},
		{"/empty", "gzip"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("Accept-Encoding", tc.accept)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Header().Get("Content-Encoding") != "" {
			t.Errorf("%s with Accept-Encoding %q was compressed", tc.path, tc.accept)
		}
	}

	if !acceptsGzip("*") || acceptsGzip("deflate") || acceptsGzip("gzip; q=0.0") {
		t.Error("acceptsGzip parsing is wrong")
	}
}
