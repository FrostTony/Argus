package domain

import (
	"context"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"

	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

const whoisPort = "43"

// whoisServer asks a WHOIS server (RFC 3912) and reads its "key: value" lines.
type whoisServer struct{ addr string }

// whoisKeys lists, per field, the lowercased keys that carry it, in order of preference:
// TCI's layout (.ru, .su, .рф) first, then the ICANN one most other servers copy.
var whoisKeys = struct{ expiry, registered, registrar, status []string }{
	expiry:     []string{"paid-till", "registry expiry date", "expiration date", "expiry date", "expires"},
	registered: []string{"created", "creation date", "registered"},
	registrar:  []string{"registrar"},
	status:     []string{"state", "domain status", "status"},
}

var whoisNotFound = regexp.MustCompile(`(?i)no entries found|no match|not found|no data found`)

// whoisGates holds one slot per server address: WHOIS servers limit connections per
// client, and TCI answers a burst with a refusal rather than data.
var whoisGates sync.Map // addr → chan struct{}

func (s *whoisServer) String() string { return "whois " + s.addr }

func (s *whoisServer) lookup(ctx context.Context, name string) (*record, error) {
	g, _ := whoisGates.LoadOrStore(s.addr, make(chan struct{}, 1))
	slot := g.(chan struct{})
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
	case <-ctx.Done():
		return nil, s.fail(ctx, ctx.Err())
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	defer conn.Close()
	defer context.AfterFunc(ctx, func() { conn.Close() })()

	if _, err := fmt.Fprintf(conn, "%s\r\n", name); err != nil {
		return nil, s.fail(ctx, err)
	}
	body, err := io.ReadAll(io.LimitReader(conn, maxBody))
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	return parseWHOIS(name, string(body))
}

// fail reports a broken exchange. The context closes the connection on expiry, so its
// error is put back in the chain for the deadline to classify as a timeout.
func (s *whoisServer) fail(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return probe.Wrap(probe.ReasonConnect, fmt.Errorf("whois %s: %w", s.addr, err))
}

func parseWHOIS(name, body string) (*record, error) {
	fields := map[string][]string{}
	for line := range strings.Lines(body) {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(line, "%") || strings.HasPrefix(line, "#") {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		fields[key] = append(fields[key], strings.ToValidUTF8(strings.TrimSpace(value), ""))
	}
	first := func(keys []string) []string {
		for _, k := range keys {
			if v := fields[k]; len(v) > 0 {
				return v
			}
		}
		return nil
	}

	r := &record{}
	if v := first(whoisKeys.expiry); v != nil {
		r.expiry, _ = parseTime(v[0])
	}
	if v := first(whoisKeys.registered); v != nil {
		r.registered, _ = parseTime(v[0])
	}
	if v := first(whoisKeys.registrar); v != nil {
		r.registrar = v[0]
	}
	// "REGISTERED, DELEGATED" (TCI) or one code per line with a link (ICANN).
	for _, v := range first(whoisKeys.status) {
		for s := range strings.SplitSeq(v, ",") {
			s, _, _ = strings.Cut(strings.TrimSpace(s), " http")
			if s != "" {
				r.status = append(r.status, s)
			}
		}
	}

	if r.expiry.IsZero() && r.registered.IsZero() && r.status == nil {
		if whoisNotFound.MatchString(body) {
			return nil, probe.Fail(probe.ReasonContent, "%s is not registered", name)
		}
		// Usually a rate limit, which the server words as it likes.
		return nil, probe.Fail(probe.ReasonProtocol, "whois: no registration data for %s: %q", name, excerpt(body))
	}
	return r, nil
}

// excerpt is the first line of an answer that is not a comment, cut to fit an error.
func excerpt(body string) string {
	for line := range strings.Lines(body) {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "%") && !strings.HasPrefix(line, "#") {
			return strings.ToValidUTF8(line[:min(len(line), 120)], "")
		}
	}
	return ""
}
