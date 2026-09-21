package server

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// gzipWriters are reused: a writer carries ~800 KB of compression state.
var gzipWriters = sync.Pool{New: func() any {
	zw, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
	return zw
}}

// compressed gzips the answer for a client that accepts it. The exposition and
// the status data repeat the same labels on every line and shrink ~50 times,
// which is the difference between a scrape that fits its timeout and one cut
// off halfway on a slow link.
func compressed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if r.Method == http.MethodHead || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next(w, r)
			return
		}
		zw := gzipWriters.Get().(*gzip.Writer)
		zw.Reset(w)
		gw := &gzipResponse{ResponseWriter: w, zw: zw}
		defer func() {
			if gw.wroteHeader {
				_ = zw.Close() // the footer: even an empty body must be valid gzip
			}
			gzipWriters.Put(zw)
		}()
		next(gw, r)
	}
}

// acceptsGzip reads Accept-Encoding; "gzip;q=0" is a refusal.
func acceptsGzip(header string) bool {
	for part := range strings.SplitSeq(header, ",") {
		coding, params, _ := strings.Cut(part, ";")
		if strings.TrimSpace(coding) != "gzip" {
			continue
		}
		q, ok := strings.CutPrefix(strings.TrimSpace(params), "q=")
		if !ok {
			return true
		}
		v, err := strconv.ParseFloat(q, 64)
		return err == nil && v > 0
	}
	return false
}

type gzipResponse struct {
	http.ResponseWriter
	zw          *gzip.Writer
	wroteHeader bool
}

func (g *gzipResponse) WriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	h := g.Header()
	// A length set by the handler counts the plain bytes, not what goes out.
	h.Del("Content-Length")
	h.Set("Content-Encoding", "gzip")
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponse) Write(b []byte) (int, error) {
	if !g.wroteHeader {
		if g.Header().Get("Content-Type") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(b))
		}
		g.WriteHeader(http.StatusOK)
	}
	return g.zw.Write(b)
}

// Flush pushes what is compressed so far, for a handler that streams.
func (g *gzipResponse) Flush() {
	if g.wroteHeader {
		_ = g.zw.Flush()
	}
	http.NewResponseController(g.ResponseWriter).Flush()
}

// Unwrap lets http.ResponseController reach the connection's deadlines.
func (g *gzipResponse) Unwrap() http.ResponseWriter { return g.ResponseWriter }
