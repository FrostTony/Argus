package probe

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// Conns holds the connections one check opened, to close them all when it
// ends: a stream cut off by a deadline leaves a connection the transport does
// not count as idle, and so never closes.
type Conns struct {
	mu    sync.Mutex
	conns []net.Conn
}

// Dial dials through d and remembers the connection.
func (s *Conns) Dial(ctx context.Context, d *net.Dialer, network, addr string) (net.Conn, error) {
	c, err := d.DialContext(ctx, network, addr)
	if err == nil {
		s.mu.Lock()
		s.conns = append(s.conns, c)
		s.mu.Unlock()
	}
	return c, err
}

// CloseAll closes every connection dialled so far.
func (s *Conns) CloseAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.conns = nil
}

// HTTPURL is the address an HTTP client takes the target at: its URL, or
// https on its host — http on port 80 — when it was written as host[:port].
func (t Target) HTTPURL() (*url.URL, error) {
	if t.URL != nil {
		u := *t.URL
		return &u, nil
	}
	if t.Host == "" {
		return nil, fmt.Errorf("target has neither url nor host")
	}
	u := &url.URL{Scheme: "https", Host: t.Host, Path: "/"}
	if strings.Contains(t.Host, ":") {
		u.Host = "[" + t.Host + "]" // an IPv6 literal
	}
	switch {
	case t.Port == 80:
		u.Scheme = "http"
	case t.Port != 0 && t.Port != 443:
		u.Host = net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	}
	return u, nil
}
