// Package probe defines the prober contract and runs checks. A prober checks one
// backend; scheduling, resolution, fan-out and shared metrics belong to the runner.
package probe

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/resolve"
)

// Target is what a check is about: a domain, not an address. Name is what the
// metrics carry, and it outlives the addresses behind it.
type Target struct {
	Name   string
	Host   string
	Port   int
	URL    *url.URL
	Labels metrics.Labels
}

// Backend is the concrete address being checked right now.
type Backend struct {
	Addr netip.Addr
	Port int
}

func (b Backend) String() string {
	if !b.Addr.IsValid() {
		return ""
	}
	if b.Port == 0 {
		return b.Addr.String()
	}
	return net.JoinHostPort(b.Addr.String(), strconv.Itoa(b.Port))
}

func (b Backend) Family() string { return string(resolve.FamilyOf(b.Addr)) }

// Valid reports whether an address is set; self-addressed probers get none.
func (b Backend) Valid() bool { return b.Addr.IsValid() }

// Request is one check of one backend: the prober dials Backend while Host, SNI
// and the URL stay on the target's own name.
type Request struct {
	Target  Target
	Backend Backend
	Buckets metrics.Buckets
	// SourceIP binds outgoing connections to one local address.
	SourceIP netip.Addr
	// Hostname overrides the name the probe presents — the Host header and the
	// SNI — without changing the address it dials. Empty means the target's own.
	Hostname string
}

// ServerName is the name to present to the target. Overriding it is how one
// virtual host is checked on a machine that serves many, and how blackbox's
// hostname= is honoured.
func (r Request) ServerName() string { return cmp.Or(r.Hostname, r.Target.Host) }

// Dialer returns a dialer bound to the request's source address. The local
// address must be of the network dialled, or the dial is refused as mismatched.
func (r Request) Dialer(network string) *net.Dialer {
	d := &net.Dialer{}
	if !r.SourceIP.IsValid() {
		return d
	}
	ip := net.IP(r.SourceIP.AsSlice())
	if strings.HasPrefix(network, "udp") {
		d.LocalAddr = &net.UDPAddr{IP: ip}
	} else {
		d.LocalAddr = &net.TCPAddr{IP: ip}
	}
	return d
}

// Address is what a prober dials: the backend the runner picked, or the target's
// own name when fan-out is off. The port is the first non-zero of those given.
func (r Request) Address(ports ...int) (string, error) {
	port := 0
	for _, p := range ports {
		if p > 0 {
			port = p
			break
		}
	}
	if port == 0 {
		return "", Fail(ReasonInternal, "no port: set one on the probe or as host:port on the target")
	}
	host := r.Target.Host
	if r.Backend.Valid() {
		host = r.Backend.Addr.String()
	}
	if host == "" {
		return "", Fail(ReasonInternal, "the target has no host to connect to")
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// LocalUDPAddr is the source for datagram and ICMP sockets.
func (r Request) LocalUDPAddr() string {
	if !r.SourceIP.IsValid() {
		return ""
	}
	return r.SourceIP.String()
}

// Result is what one check produced, beyond the metrics the prober records itself.
type Result struct {
	Err error
	// Phases is what the time was spent on; the runner turns each into a metric.
	Phases []Phase
	// Overhead is time spent elsewhere than the target, left out of probe_duration.
	Overhead time.Duration
}

// Phase is one named span of a check — connect, tls, ttfb.
type Phase struct {
	Name string
	D    time.Duration
}

func (r Result) OK() bool { return r.Err == nil }

// Add accounts time to a phase; a phase entered twice is one phase that took longer.
func (r *Result) Add(name string, d time.Duration) {
	if d <= 0 {
		return
	}
	for i := range r.Phases {
		if r.Phases[i].Name == name {
			r.Phases[i].D += d
			return
		}
	}
	r.Phases = append(r.Phases, Phase{name, d})
}

// Prober checks one backend of one target. It must talk to req.Backend rather
// than wherever the name resolves now; prober-specific metrics go into rec.
type Prober interface {
	Probe(ctx context.Context, req Request, rec *metrics.Recorder) Result
}

// Addressing is implemented by probers that address themselves, such as dns. The
// runner calls them once per target, with no resolution and no backend label.
type Addressing interface {
	SelfAddressed() bool
}

// FileBacked is implemented by probers built from files; a reload compares their
// contents, so rotating one takes effect.
type FileBacked interface {
	Files() []string
}

// FailureReason becomes a label on probe_failure_total.
type FailureReason string

const (
	ReasonDNS      FailureReason = "dns"
	ReasonConnect  FailureReason = "connect"
	ReasonTLS      FailureReason = "tls"
	ReasonTimeout  FailureReason = "timeout"
	ReasonStatus   FailureReason = "status"
	ReasonContent  FailureReason = "content"
	ReasonProtocol FailureReason = "protocol"
	ReasonInternal FailureReason = "internal"
	ReasonUnknown  FailureReason = "unknown"
)

// Error carries the reason alongside the cause.
type Error struct {
	Reason FailureReason
	Err    error
	// Regex marks a content failure a pattern decided, which blackbox reports
	// separately as probe_failed_due_to_regex. It is a finer grain than the
	// reason label, not a reason of its own: a body that does not match is a
	// content failure either way.
	Regex bool
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %v", e.Reason, e.Err) }
func (e *Error) Unwrap() error { return e.Err }

// Fail builds a classified failure from a message of the prober's own.
func Fail(reason FailureReason, format string, args ...any) error {
	return &Error{Reason: reason, Err: fmt.Errorf(format, args...)}
}

// FailRegex builds a content failure a pattern decided.
func FailRegex(format string, args ...any) error {
	return &Error{Reason: ReasonContent, Err: fmt.Errorf(format, args...), Regex: true}
}

// RegexFailure reports whether a pattern is what failed the check.
func RegexFailure(err error) bool {
	var pe *Error
	return errors.As(err, &pe) && pe.Regex
}

// Wrap classifies a Go error; a timeout is checked before a net error, which hides it.
func Wrap(fallback FailureReason, err error) error {
	if err == nil {
		return nil
	}
	var pe *Error
	if errors.As(err, &pe) {
		return err
	}
	reason := fallback
	if isTimeout(err) {
		reason = ReasonTimeout
	} else if isDNSError(err) {
		reason = ReasonDNS
	}
	return &Error{Reason: reason, Err: err}
}

// Message is the error without the reason it is already labelled with.
func Message(err error) string {
	if err == nil {
		return ""
	}
	var pe *Error
	if errors.As(err, &pe) {
		err = pe.Err
	}
	// net/http wraps everything in a *url.Error, whose prefix repeats the target.
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return err.Error()
}

// ReasonOf recovers the classification, or ReasonUnknown for an unclassified error.
func ReasonOf(err error) FailureReason {
	if err == nil {
		return ""
	}
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Reason
	}
	return ReasonUnknown
}

func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

func isDNSError(err error) bool {
	var de *net.DNSError
	return errors.As(err, &de)
}
