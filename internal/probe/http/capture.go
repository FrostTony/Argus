package http

import (
	"bytes"
	"maps"
	"net/http"
	"slices"

	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// exchangeLimit bounds the headers artifact: every redirect hop may carry a
// megabyte of headers, and max_redirects has no ceiling of its own.
const exchangeLimit = 64 << 10

// artifacts hands back what the target answered: the exchange, then the body as
// it came, undecoded.
func artifacts(resp *http.Response, body []byte, size int64) []probe.Artifact {
	final := resp.Request.URL.Redacted()
	headers, cut := exchange(resp)
	data := body
	if len(body) > probe.CaptureLimit {
		// A copy, so the rest of what a validator kept is not held with it.
		data = bytes.Clone(body[:probe.CaptureLimit])
	}
	return []probe.Artifact{
		{Name: "headers", MediaType: "text/plain; charset=utf-8", URL: final, Data: headers, Truncated: cut},
		{
			Name:      "body",
			MediaType: mediaType(resp.Header, data),
			URL:       final,
			Data:      data,
			Truncated: size > int64(len(data)),
		},
	}
}

// exchange writes every hop, redirects first: a block page is often a redirect
// away from the URL asked for. Request headers stay out and URLs are redacted —
// both carry the probe's credentials.
func exchange(resp *http.Response) ([]byte, bool) {
	var hops []*http.Response
	for r := resp; r != nil; {
		hops = append(hops, r)
		if r.Request == nil {
			break
		}
		r = r.Request.Response
	}
	var b capped
	for _, r := range slices.Backward(hops) {
		if r.Request != nil {
			b.line("> ", r.Request.Method, " ", r.Request.URL.Redacted())
		}
		b.line("< ", r.Proto, " ", r.Status)
		for _, k := range slices.Sorted(maps.Keys(r.Header)) {
			for _, v := range r.Header[k] {
				b.line(k, ": ", v)
			}
		}
		b.line()
	}
	return b.Bytes(), b.cut
}

// capped takes whole lines up to exchangeLimit and drops the rest.
type capped struct {
	bytes.Buffer
	cut bool
}

func (c *capped) line(parts ...string) {
	n := 1
	for _, p := range parts {
		n += len(p)
	}
	if c.cut || c.Len()+n > exchangeLimit {
		c.cut = true
		return
	}
	for _, p := range parts {
		c.WriteString(p)
	}
	c.WriteByte('\n')
}

// mediaType is what the server declared, or a guess when it declared nothing.
func mediaType(h http.Header, body []byte) string {
	if ct := h.Get("Content-Type"); ct != "" {
		return ct
	}
	if len(body) == 0 {
		return ""
	}
	return http.DetectContentType(body)
}
