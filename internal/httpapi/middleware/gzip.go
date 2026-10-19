package middleware

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
)

// Gzip compresses JSON responses for clients that accept gzip. Admin payloads
// such as session transcripts are large, highly repetitive JSON; compressing
// them is the difference between megabytes and hundreds of kilobytes on the
// wire. Non-JSON responses (and anything already encoded) pass through
// untouched, so streaming and file downloads are unaffected.
func Gzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Accept-Encoding")
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	zw       *gzip.Writer
	decided  bool
	compress bool
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	if !g.decided {
		g.decided = true
		h := g.Header()
		g.compress = status >= http.StatusOK &&
			status != http.StatusNoContent &&
			status != http.StatusNotModified &&
			h.Get("Content-Encoding") == "" &&
			strings.HasPrefix(strings.ToLower(h.Get("Content-Type")), "application/json")
		if g.compress {
			h.Set("Content-Encoding", "gzip")
			h.Del("Content-Length")
			g.zw = gzip.NewWriter(g.ResponseWriter)
		}
	}
	g.ResponseWriter.WriteHeader(status)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.decided {
		g.WriteHeader(http.StatusOK)
	}
	if g.compress {
		return g.zw.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer so handlers that stream still work.
func (g *gzipResponseWriter) Flush() {
	if g.zw != nil {
		_ = g.zw.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipResponseWriter) close() {
	if g.zw != nil {
		_ = g.zw.Close()
	}
}

// acceptsGzip reports whether an Accept-Encoding header allows gzip
// (explicitly or via "*") with a non-zero quality.
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(part, ";")
		coding := strings.ToLower(strings.TrimSpace(fields[0]))
		if coding != "gzip" && coding != "*" {
			continue
		}
		quality := 1.0
		for _, param := range fields[1:] {
			if name, value, ok := strings.Cut(strings.TrimSpace(param), "="); ok && strings.EqualFold(name, "q") {
				if q, err := strconv.ParseFloat(value, 64); err == nil {
					quality = q
				}
			}
		}
		if quality > 0 {
			return true
		}
	}
	return false
}
