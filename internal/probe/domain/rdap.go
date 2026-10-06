package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// rdapServer asks an RDAP service (RFC 9083).
type rdapServer struct {
	url    string
	client *http.Client
}

func (s *rdapServer) String() string { return "rdap " + s.url }

func (s *rdapServer) lookup(ctx context.Context, name string) (*record, error) {
	endpoint := strings.TrimSuffix(s.url, "/") + "/domain/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, probe.Fail(probe.ReasonInternal, "%v", err)
	}
	req.Header.Set("Accept", "application/rdap+json")

	resp, err := s.client.Do(req)
	if err != nil {
		// Wrapped so the deadline stays in the chain and classifies as a timeout.
		return nil, probe.Wrap(probe.ReasonConnect, fmt.Errorf("rdap %s: %w", endpoint, unwrapURL(err)))
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// A registry's 404 is an unregistered name; rdap.org's is also a TLD it cannot route.
		return nil, probe.Fail(probe.ReasonContent,
			"%s is not registered, or its registry has no RDAP (map the suffix in domain.whois)", name)
	}
	if resp.StatusCode/100 != 2 {
		return nil, probe.Fail(probe.ReasonStatus, "rdap %s: got %d", endpoint, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, probe.Wrap(probe.ReasonProtocol, err)
	}
	var doc rdapDomain
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, probe.Fail(probe.ReasonProtocol, "rdap %s: %v", endpoint, err)
	}
	return doc.record(), nil
}

// rdapDomain is the part of an RFC 9083 domain object this check needs.
type rdapDomain struct {
	LDHName string `json:"ldhName"`
	Status  []string
	Events  []struct {
		Action string `json:"eventAction"`
		Date   string `json:"eventDate"`
	}
	Entities []struct {
		Roles []string `json:"roles"`
		// vCard is nested untyped arrays, so it is walked rather than decoded.
		VCard []any `json:"vcardArray"`
	}
}

func (d *rdapDomain) record() *record {
	return &record{
		expiry:     d.event("expiration"),
		registered: d.event("registration"),
		registrar:  d.registrar(),
		status:     d.Status,
		lockable:   true,
	}
}

// event finds a date by its action, matched case-insensitively.
func (d *rdapDomain) event(action string) time.Time {
	for _, e := range d.Events {
		if t, ok := parseTime(e.Date); ok && strings.EqualFold(e.Action, action) {
			return t
		}
	}
	return time.Time{}
}

// registrar reads the "fn" property of the registrar entity's vCard:
// ["vcard", [["version", {}, "text", "4.0"], ["fn", {}, "text", "Example Inc"]]].
func (d *rdapDomain) registrar() string {
	for _, e := range d.Entities {
		if !has(e.Roles, "registrar") || len(e.VCard) < 2 {
			continue
		}
		props, ok := e.VCard[1].([]any)
		if !ok {
			continue
		}
		for _, p := range props {
			field, ok := p.([]any)
			if !ok || len(field) < 4 {
				continue
			}
			if key, ok := field[0].(string); !ok || key != "fn" {
				continue
			}
			if value, ok := field[3].(string); ok {
				return value
			}
		}
	}
	return ""
}

// unwrapURL drops the "Get \"...\":" prefix url.Error prints.
func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
