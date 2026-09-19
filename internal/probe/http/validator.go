package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// Validator applies every field that is set and passes only if all of them
// hold, emitting http_validator{validator="<name>"} as 0/1.
type Validator struct {
	Name         string   `yaml:"name"`
	StatusCode   []string `yaml:"status_code"`
	BodyRegex    string   `yaml:"body_regex"`
	BodyNotRegex string   `yaml:"body_not_regex"`
	Header       string   `yaml:"header"`
	HeaderRegex  string   `yaml:"header_regex"`
	MinSizeBytes int64    `yaml:"min_size_bytes"`
	MaxSizeBytes int64    `yaml:"max_size_bytes"`
	// HTTPVersion lists acceptable protocol versions ("HTTP/1.1", "HTTP/2.0").
	HTTPVersion []string `yaml:"http_version"`
	RequireTLS  bool     `yaml:"require_tls"`
	ForbidTLS   bool     `yaml:"forbid_tls"`
	// JSONPath is a dotted path into a JSON body, checked by JSONEquals and JSONRegex.
	JSONPath   string `yaml:"json_path"`
	JSONEquals string `yaml:"json_equals"`
	JSONRegex  string `yaml:"json_regex"`
	// CertMinDays fails the check this many days before the certificate expires.
	CertMinDays int `yaml:"cert_min_days"`

	codes     []codeRange
	bodyRe    *regexp.Regexp
	bodyNotRe *regexp.Regexp
	headerRe  *regexp.Regexp
	jsonRe    *regexp.Regexp
}

type codeRange struct{ lo, hi int }

func (c codeRange) has(v int) bool { return v >= c.lo && v <= c.hi }

func (v *Validator) checksSomething() bool {
	return len(v.StatusCode) > 0 || v.BodyRegex != "" || v.BodyNotRegex != "" ||
		v.HeaderRegex != "" || v.JSONPath != "" || v.MinSizeBytes > 0 || v.MaxSizeBytes > 0 ||
		len(v.HTTPVersion) > 0 || v.RequireTLS || v.ForbidTLS || v.CertMinDays > 0
}

func (v *Validator) readsBody() bool {
	return v.BodyRegex != "" || v.BodyNotRegex != "" || v.JSONPath != ""
}

func (v *Validator) compile() error {
	if v.Name == "" {
		v.Name = "validator"
	}
	for _, s := range v.StatusCode {
		r, err := parseCodeRange(s)
		if err != nil {
			return err
		}
		v.codes = append(v.codes, r)
	}
	var err error
	if v.bodyRe, err = compileRe(v.BodyRegex); err != nil {
		return fmt.Errorf("body_regex: %w", err)
	}
	if v.bodyNotRe, err = compileRe(v.BodyNotRegex); err != nil {
		return fmt.Errorf("body_not_regex: %w", err)
	}
	if v.headerRe, err = compileRe(v.HeaderRegex); err != nil {
		return fmt.Errorf("header_regex: %w", err)
	}
	if v.jsonRe, err = compileRe(v.JSONRegex); err != nil {
		return fmt.Errorf("json_regex: %w", err)
	}
	if v.headerRe != nil && v.Header == "" {
		return fmt.Errorf("header_regex set without header")
	}
	if (v.JSONEquals != "" || v.JSONRegex != "") && v.JSONPath == "" {
		return fmt.Errorf("json_equals/json_regex set without json_path")
	}
	if !v.checksSomething() {
		// A validator that checks nothing passes everything and replaces the default.
		return fmt.Errorf("checks nothing: set status_code, a body or header match, a json field, " +
			"a size bound, require_tls/forbid_tls or cert_min_days")
	}
	if v.RequireTLS && v.ForbidTLS {
		return fmt.Errorf("require_tls and forbid_tls are mutually exclusive")
	}
	return nil
}

func compileRe(s string) (*regexp.Regexp, error) {
	if s == "" {
		return nil, nil
	}
	return regexp.Compile(s)
}

// parseCodeRange reads "200", "200-399" or "2xx".
func parseCodeRange(s string) (codeRange, error) {
	s = strings.TrimSpace(s)
	if lo, hi, ok := strings.Cut(s, "-"); ok {
		l, err1 := strconv.Atoi(strings.TrimSpace(lo))
		h, err2 := strconv.Atoi(strings.TrimSpace(hi))
		if err1 != nil || err2 != nil || l > h {
			return codeRange{}, fmt.Errorf("invalid code range %q", s)
		}
		return codeRange{l, h}, nil
	}
	if len(s) == 3 && (s[1] == 'x' || s[1] == 'X') {
		d, err := strconv.Atoi(s[:1])
		if err != nil {
			return codeRange{}, fmt.Errorf("invalid code pattern %q", s)
		}
		return codeRange{d * 100, d*100 + 99}, nil
	}
	c, err := strconv.Atoi(s)
	if err != nil {
		return codeRange{}, fmt.Errorf("invalid code %q", s)
	}
	return codeRange{c, c}, nil
}

// validate runs every validator, recording all and returning the first failure.
func (p *Prober) validate(rec *metrics.Recorder, resp *http.Response, body string, size int64, now time.Time) error {
	var firstErr error
	for i := range p.validators {
		v := &p.validators[i]
		err := v.check(resp, body, size, now)
		rec.Gauge("http_validator", boolValue(err == nil), "validator", v.Name)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (v *Validator) check(resp *http.Response, body string, size int64, now time.Time) error {
	if len(v.codes) > 0 && !matchCode(v.codes, resp.StatusCode) {
		return probe.Fail(probe.ReasonStatus, "%s: got %d, want %s", v.Name, resp.StatusCode, strings.Join(v.StatusCode, ","))
	}
	if v.bodyRe != nil && !v.bodyRe.MatchString(body) {
		return probe.Fail(probe.ReasonContent, "%s: body does not match %q", v.Name, v.BodyRegex)
	}
	if v.bodyNotRe != nil && v.bodyNotRe.MatchString(body) {
		return probe.Fail(probe.ReasonContent, "%s: body matches forbidden %q", v.Name, v.BodyNotRegex)
	}
	if v.headerRe != nil && !v.headerRe.MatchString(resp.Header.Get(v.Header)) {
		return probe.Fail(probe.ReasonContent, "%s: header %s does not match %q", v.Name, v.Header, v.HeaderRegex)
	}
	if len(v.HTTPVersion) > 0 && !slices.Contains(v.HTTPVersion, resp.Proto) {
		return probe.Fail(probe.ReasonProtocol, "%s: %s is not one of %s",
			v.Name, resp.Proto, strings.Join(v.HTTPVersion, ","))
	}
	if v.RequireTLS && resp.TLS == nil {
		return probe.Fail(probe.ReasonTLS, "%s: the answer came without TLS", v.Name)
	}
	if v.ForbidTLS && resp.TLS != nil {
		return probe.Fail(probe.ReasonTLS, "%s: the answer came over TLS", v.Name)
	}
	if v.JSONPath != "" {
		if err := v.checkJSON(body); err != nil {
			return err
		}
	}
	if v.MinSizeBytes > 0 && size < v.MinSizeBytes {
		return probe.Fail(probe.ReasonContent, "%s: response %d bytes, below %d", v.Name, size, v.MinSizeBytes)
	}
	if v.MaxSizeBytes > 0 && size > v.MaxSizeBytes {
		return probe.Fail(probe.ReasonContent, "%s: response %d bytes, above %d", v.Name, size, v.MaxSizeBytes)
	}
	if v.CertMinDays > 0 {
		if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
			return probe.Fail(probe.ReasonTLS, "%s: no certificate to check expiry on", v.Name)
		}
		left := resp.TLS.PeerCertificates[0].NotAfter.Sub(now)
		if left < time.Duration(v.CertMinDays)*24*time.Hour {
			return probe.Fail(probe.ReasonTLS, "%s: certificate has %.1f days left, want %d",
				v.Name, left.Hours()/24, v.CertMinDays)
		}
	}
	return nil
}

func (v *Validator) checkJSON(body string) error {
	var doc any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return probe.Fail(probe.ReasonContent, "%s: the body is not JSON: %v", v.Name, err)
	}
	found, ok := jsonLookup(doc, strings.Split(v.JSONPath, "."))
	if !ok {
		return probe.Fail(probe.ReasonContent, "%s: %s is missing from the body", v.Name, v.JSONPath)
	}
	got := jsonString(found)
	if v.JSONEquals != "" && got != v.JSONEquals {
		return probe.Fail(probe.ReasonContent, "%s: %s = %q, want %q", v.Name, v.JSONPath, got, v.JSONEquals)
	}
	if v.jsonRe != nil && !v.jsonRe.MatchString(got) {
		return probe.Fail(probe.ReasonContent, "%s: %s = %q does not match %q", v.Name, v.JSONPath, got, v.JSONRegex)
	}
	return nil
}

// jsonLookup follows a dotted path, where a numeric element indexes an array.
func jsonLookup(doc any, path []string) (any, bool) {
	cur := doc
	for _, key := range path {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[key]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// jsonString renders a value as text; a number keeps its written form ("1", not "1e+00").
func jsonString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return "null"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func matchCode(ranges []codeRange, code int) bool {
	for _, r := range ranges {
		if r.has(code) {
			return true
		}
	}
	return false
}
