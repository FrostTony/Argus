package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
	httpprobe "github.com/tonyamdfrost-cmd/Argus/internal/probe/http"
)

// userAgent tells the target's owners which traffic is synthetic.
const userAgent = "argus-scenario/0.1"

// build turns a step's request into an http.Request for one attempt. Every
// attempt gets a fresh body: a file is reopened, a template rendered again.
func (r *request) build(ctx context.Context, base *url.URL, s *scope) (*http.Request, error) {
	target, err := r.address(base, s)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", r.key, err)
	}
	body, err := r.payload(s)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, r.method, target.String(), body.reader)
	if err != nil {
		body.close()
		return nil, err
	}
	if body.reader != nil {
		// A stream of unknown length would go out chunked, and the upload
		// endpoints worth checking tend to refuse that.
		req.ContentLength = body.size
		if req.GetBody == nil {
			// A 307 or 308 sends the body again: the files are reopened.
			req.GetBody = func() (io.ReadCloser, error) {
				again, err := r.payload(s)
				if err != nil {
					return nil, err
				}
				return again.reader.(io.ReadCloser), nil
			}
		}
		if body.size == 0 {
			req.Body, req.GetBody = http.NoBody, nil
			body.close()
		}
	}
	req.Header.Set("User-Agent", userAgent)
	if body.contentType != "" {
		req.Header.Set("Content-Type", body.contentType)
	}
	for _, h := range r.headers {
		v, err := h.value.render(s, nil)
		if err != nil {
			body.close()
			return nil, fmt.Errorf("headers.%s: %w", h.key, err)
		}
		// net/http refuses such a value only when sending, as a failure to connect.
		if !httpguts.ValidHeaderFieldValue(v) {
			body.close()
			return nil, probe.Fail(probe.ReasonContent, "headers.%s: the value is not a valid header value", h.key)
		}
		// Host is no ordinary header: net/http reads it from the struct.
		if strings.EqualFold(h.key, "host") {
			req.Host = v
			continue
		}
		req.Header.Set(h.key, v)
	}
	return req, nil
}

// address renders where the request goes, taken against the target. A value
// put into the path is one segment of it, whatever it holds; an address that
// is one reference is taken whole, being one the target handed out — a
// presigned URL, a Location.
func (r *request) address(base *url.URL, s *scope) (*url.URL, error) {
	var escape escaper = segment
	if r.url.whole() {
		escape = nil
	}
	raw, err := r.url.render(s, escape)
	if err != nil {
		return nil, err
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	// An absolute address is sent as it is: resolving it would clean its
	// path, and a presigned URL's signature covers the path as it was signed.
	u := ref
	if !ref.IsAbs() {
		u = base.ResolveReference(ref)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%q is not an http(s) address", raw)
	}
	// An address a response handed out goes where that response says; the
	// step's secrets go only to the target, and not over a weaker scheme.
	if r.url.whole() && r.sensitive && !sameOrigin(u, base) {
		return nil, fmt.Errorf("%s leads to %s://%s, not the target: a step that sends a secret stays on the target",
			r.url.refs[0], u.Scheme, u.Host)
	}
	if len(r.query) > 0 {
		q := u.Query()
		for _, p := range r.query {
			v, err := p.value.render(s, nil)
			if err != nil {
				return nil, fmt.Errorf("query.%s: %w", p.key, err)
			}
			q.Set(p.key, v)
		}
		u.RawQuery = q.Encode()
	}
	return u, nil
}

// payload is a request body with what has to be said about it.
type payload struct {
	reader      io.Reader
	size        int64
	contentType string
	closers     []io.Closer
}

func (p payload) close() {
	for _, c := range p.closers {
		_ = c.Close()
	}
}

// Close lets the transport release the files once the body is sent.
func (p payload) Close() error {
	p.close()
	return nil
}

func (r *request) payload(s *scope) (payload, error) {
	switch {
	case r.body != nil:
		text, err := r.body.render(s, nil)
		if err != nil {
			return payload{}, fmt.Errorf("body: %w", err)
		}
		return payload{reader: strings.NewReader(text), size: int64(len(text))}, nil

	case r.bodyFile != "":
		f, size, err := open(r.bodyFile)
		if err != nil {
			return payload{}, fmt.Errorf("body_file: %w", err)
		}
		p := payload{size: size, contentType: contentType(r.bodyFile), closers: []io.Closer{f}}
		p.reader = withClose(io.LimitReader(f, size), p)
		return p, nil

	case r.json != nil:
		doc, err := renderJSON(r.json, s)
		if err != nil {
			return payload{}, fmt.Errorf("json: %w", err)
		}
		data, err := json.Marshal(doc)
		if err != nil {
			return payload{}, fmt.Errorf("json: %w", err)
		}
		return payload{reader: bytes.NewReader(data), size: int64(len(data)), contentType: "application/json"}, nil

	case len(r.form) > 0:
		form := url.Values{}
		for _, p := range r.form {
			v, err := p.value.render(s, nil)
			if err != nil {
				return payload{}, fmt.Errorf("form.%s: %w", p.key, err)
			}
			form.Set(p.key, v)
		}
		text := form.Encode()
		return payload{
			reader:      strings.NewReader(text),
			size:        int64(len(text)),
			contentType: "application/x-www-form-urlencoded",
		}, nil

	case len(r.fields) > 0 || len(r.files) > 0:
		return r.multipart(s)
	}
	return payload{}, nil
}

// multipart lays the body out as the text the writer produces around the files
// and the files themselves, read from disk as they are sent. Its length is
// known before a byte goes out.
func (r *request) multipart(s *scope) (payload, error) {
	var (
		buf   bytes.Buffer
		parts []io.Reader
		p     payload
	)
	mw := multipart.NewWriter(&buf)
	cut := func() {
		chunk := bytes.Clone(buf.Bytes())
		buf.Reset()
		parts = append(parts, bytes.NewReader(chunk))
		p.size += int64(len(chunk))
	}
	fail := func(err error) (payload, error) {
		p.close()
		return payload{}, err
	}

	for _, f := range r.fields {
		v, err := f.value.render(s, nil)
		if err != nil {
			return fail(fmt.Errorf("multipart.fields.%s: %w", f.key, err))
		}
		if err := mw.WriteField(f.key, v); err != nil {
			return fail(err)
		}
	}
	for _, f := range r.files {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
			quote(f.field), quote(f.filename)))
		h.Set("Content-Type", f.contentType)
		if _, err := mw.CreatePart(h); err != nil {
			return fail(err)
		}
		cut()
		if f.path == "" {
			parts = append(parts, bytes.NewReader(f.data))
			p.size += int64(len(f.data))
			continue
		}
		file, size, err := open(f.path)
		if err != nil {
			return fail(fmt.Errorf("multipart.files.%s: %w", f.field, err))
		}
		p.closers = append(p.closers, file)
		// Bounded by the size announced: a file that grows mid-upload must not
		// send more than Content-Length said.
		parts = append(parts, io.LimitReader(file, size))
		p.size += size
	}
	if err := mw.Close(); err != nil {
		return fail(err)
	}
	cut()
	p.contentType = mw.FormDataContentType()
	p.reader = withClose(io.MultiReader(parts...), p)
	return p, nil
}

var quoter = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

func quote(s string) string { return quoter.Replace(s) }

func open(path string) (*os.File, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// withClose gives a reader the payload's Close, which net/http calls once the
// body is sent or abandoned.
func withClose(r io.Reader, p payload) io.ReadCloser {
	return struct {
		io.Reader
		io.Closer
	}{r, p}
}

// renderJSON fills a compiled document in; the encoder escapes the values.
func renderJSON(v any, s *scope) (any, error) {
	switch t := v.(type) {
	case *template:
		return t.render(s, nil)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			r, err := renderJSON(item, s)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			r, err := renderJSON(item, s)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

// response is what a step reads from an answer: the body as far as it keeps it
// and how much there was.
type response struct {
	*http.Response
	body string
	size int64
	doc  any
	// parsed records that doc was attempted, so a body that is not JSON is
	// decoded once per answer however many extractors ask.
	parsed  bool
	jsonErr error
}

// read keeps up to keep bytes of the body and counts on to limit, so a size
// bound can fail past what is kept and the connection can be reused.
func read(resp *http.Response, keep, limit int64) (*response, error) {
	defer resp.Body.Close()
	var sb strings.Builder
	if resp.ContentLength > 0 {
		sb.Grow(int(min(resp.ContentLength, keep, 4<<20)))
	}
	n, err := io.Copy(&sb, io.LimitReader(resp.Body, keep))
	if err != nil {
		return nil, err
	}
	extra, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, limit-n))
	return &response{Response: resp, body: sb.String(), size: n + extra}, nil
}

func (r *response) json() (any, error) {
	if !r.parsed {
		r.parsed = true
		r.doc, r.jsonErr = httpprobe.DecodeJSON([]byte(r.body))
	}
	return r.doc, r.jsonErr
}
