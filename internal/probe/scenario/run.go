package scenario

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
	httpprobe "github.com/tonyamdfrost-cmd/Argus/internal/probe/http"
)

// Report is one run of a scenario against one target, as the API serves it.
// Extracted values are left out: a step may well extract a session token.
type Report struct {
	Target  string       `json:"target"`
	Started time.Time    `json:"started"`
	Seconds float64      `json:"duration_seconds"`
	Success bool         `json:"success"`
	Error   string       `json:"error,omitempty"`
	Steps   []StepReport `json:"steps"`
	Cleanup []StepReport `json:"cleanup,omitempty"`
}

// StepReport is how far one step got.
type StepReport struct {
	Name     string  `json:"name"`
	Status   string  `json:"status"`
	Code     int     `json:"http_status,omitempty"`
	Attempts int     `json:"attempts,omitempty"`
	Seconds  float64 `json:"duration_seconds"`
	Error    string  `json:"error,omitempty"`
}

// What became of a step.
const (
	StatusPassed  = "passed"
	StatusFailed  = "failed"
	StatusSkipped = "skipped" // a cleanup step whose variables no step set
	StatusNotRun  = "not_run"
)

// run is one walk through the scenario for one target.
type run struct {
	sc      *compiled
	rec     *metrics.Recorder
	scope   *scope
	session session
	report  Report
}

// steps walks the steps in order and stops at the first that fails.
func (r *run) steps(ctx context.Context, base *url.URL, res *probe.Result) error {
	for _, st := range r.sc.steps {
		start := time.Now()
		out, err := r.step(ctx, base, st)
		d := time.Since(start)
		res.Add(st.name, d)
		r.record(st, out, d, err)
		if err != nil {
			return stepError(st.name, err)
		}
	}
	return nil
}

// cleanup runs every cleanup step, whatever became of the one before: each
// removes something of its own, and one leftover is better than all of them.
func (r *run) cleanup(parent context.Context, base *url.URL) {
	if len(r.sc.cleanup) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), r.sc.cleanupTimeout)
	defer cancel()

	ok := true
	for _, st := range r.sc.cleanup {
		start := time.Now()
		out, err := r.step(ctx, base, st)
		var unset errUnset
		if errors.As(err, &unset) {
			r.report.Cleanup = append(r.report.Cleanup, StepReport{
				Name:   st.name,
				Status: StatusSkipped,
				Error:  fmt.Sprintf("needs %s, which no step got as far as setting", unset.ref),
			})
			continue
		}
		ok = ok && err == nil
		r.record(st, out, time.Since(start), err)
	}
	r.rec.Gauge(SeriesCleanup, metrics.Bool(ok))
}

// outcome is what a step saw, failed or not.
type outcome struct {
	code     int
	attempts int
}

// record publishes a step that ran and adds it to the report.
func (r *run) record(st *step, out outcome, d time.Duration, err error) {
	r.rec.Gauge(SeriesStepSuccess, metrics.Bool(err == nil), "step", st.name)
	if len(st.until) > 0 {
		r.rec.Gauge(SeriesStepAttempts, float64(out.attempts), "step", st.name)
	}
	if out.code > 0 {
		r.rec.Gauge(SeriesStepStatus, float64(out.code), "step", st.name)
	}
	sr := StepReport{Name: st.name, Status: StatusPassed, Code: out.code, Attempts: out.attempts, Seconds: d.Seconds()}
	if err != nil {
		sr.Status = StatusFailed
		sr.Error = bounded(r.scope.redact(probe.Message(err)))
	}
	if st.cleanup {
		r.report.Cleanup = append(r.report.Cleanup, sr)
	} else {
		r.report.Steps = append(r.report.Steps, sr)
	}
}

// step sends the request, again every st.every while until does not hold,
// checks the answer and takes the values the steps after it need.
func (r *run) step(ctx context.Context, base *url.URL, st *step) (outcome, error) {
	var (
		out      outcome
		deadline time.Time
	)
	if st.within > 0 {
		deadline = time.Now().Add(st.within)
	}
	for {
		resp, err := r.exchange(ctx, base, st)
		out.attempts++
		if err != nil {
			return out, err
		}
		out.code = resp.StatusCode
		if err := check(st.expect, resp); err != nil {
			return out, err
		}
		if len(st.until) == 0 {
			return out, r.extract(st, resp)
		}
		if name, held := anyHolds(st.abortIf, resp); held {
			what := "abort_if"
			if name != what {
				what += " " + name // a name of its own says which condition
			}
			return out, probe.Fail(probe.ReasonContent, "%s held: %s", what, r.scope.excerpt(resp.body))
		}
		pending := check(st.until, resp)
		if pending == nil {
			return out, r.extract(st, resp)
		}

		wait := st.every
		if !deadline.IsZero() && time.Now().Add(wait).After(deadline) {
			return out, probe.Fail(probe.ReasonTimeout, "until did not hold within %s (%d attempts): %s",
				st.within, out.attempts, probe.Message(pending))
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			if errors.Is(ctx.Err(), context.Canceled) {
				return out, probe.Fail(probe.ReasonUnknown, "the run was cancelled while until did not hold (%d attempts)", out.attempts)
			}
			return out, probe.Fail(probe.ReasonTimeout, "until did not hold before the scenario ran out of time (%d attempts): %s",
				out.attempts, probe.Message(pending))
		case <-t.C:
		}
	}
}

// exchange is one request and the answer read as far as the step keeps it.
func (r *run) exchange(ctx context.Context, base *url.URL, st *step) (*response, error) {
	if st.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, st.timeout)
		defer cancel()
	}
	req, err := st.req.build(ctx, base, r.scope)
	if err != nil {
		var unset errUnset
		if errors.As(err, &unset) {
			return nil, err
		}
		// A failure the request already classified keeps its reason.
		return nil, probe.Wrap(probe.ReasonInternal, err)
	}
	client := r.session.follow
	if !st.req.follow {
		client = r.session.stay
	}
	resp, err := client.Do(req)
	if err != nil {
		// Without the URL net/http wraps it in: the report names the step,
		// and the URL may carry a secret in its query.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, probe.Wrap(transportReason(err), err)
	}
	out, err := read(resp, st.req.maxBody, st.sizeLimit)
	if err != nil {
		return nil, probe.Wrap(probe.ReasonContent, err)
	}
	// The body stays as it came: checks and extracts read the answer, and a
	// message masks what it quotes (scope.excerpt).
	return out, nil
}

// transportReason tells a certificate from a connection: both reach the step
// as the client's error.
func transportReason(err error) probe.FailureReason {
	var cert *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	switch {
	case errors.As(err, &cert), errors.As(err, &record):
		return probe.ReasonTLS
	case errors.Is(err, context.Canceled):
		return probe.ReasonUnknown // a reload or a shutdown, not the target
	}
	return probe.ReasonConnect
}

// check holds an answer to every validator and returns the first failure.
func check(vs []httpprobe.Validator, resp *response) error {
	now := time.Now()
	for i := range vs {
		if err := vs[i].Check(resp.Response, resp.body, resp.size, now); err != nil {
			return err
		}
	}
	return nil
}

// anyHolds reports the first validator the answer passes.
func anyHolds(vs []httpprobe.Validator, resp *response) (string, bool) {
	now := time.Now()
	for i := range vs {
		if vs[i].Check(resp.Response, resp.body, resp.size, now) == nil {
			return vs[i].Name, true
		}
	}
	return "", false
}

// extract takes the step's values out of the answer, all or none: a step that
// cannot hand on what the next one needs has failed.
func (r *run) extract(st *step, resp *response) error {
	values := make(map[string]string, len(st.extract))
	for _, x := range st.extract {
		v, err := x.from(resp, r.scope)
		if err != nil {
			return err
		}
		if x.secret {
			r.scope.hide(v, ref{nsVar, x.name}.String())
		}
		if x.metric {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return probe.Fail(probe.ReasonContent, "extract %s: %q is not a number", x.name, probe.Excerpt(v))
			}
			r.rec.Gauge(SeriesValue, f, "step", st.name, "name", x.name)
		}
		values[x.name] = v
	}
	for k, v := range values {
		r.scope.vars[k] = v
	}
	return nil
}

func (x extractor) from(resp *response, s *scope) (string, error) {
	switch {
	case x.jsonPath != "":
		doc, err := resp.json()
		if err != nil {
			return "", probe.Fail(probe.ReasonContent, "extract %s: the body is not JSON: %v", x.name, err)
		}
		v, ok := httpprobe.JSONField(doc, x.jsonPath)
		if !ok {
			return "", probe.Fail(probe.ReasonContent, "extract %s: %s is missing from the body", x.name, x.jsonPath)
		}
		return v, nil
	case x.header != "":
		v := resp.Header.Get(x.header)
		if v == "" {
			return "", probe.Fail(probe.ReasonContent, "extract %s: the answer has no %s header", x.name, x.header)
		}
		// A relative Location is relative to the request that got it, as a
		// browser reads it — not to the target, which may be another host.
		if strings.EqualFold(x.header, "Location") {
			if u, err := resp.Location(); err == nil {
				v = u.String()
			}
		}
		return v, nil
	default:
		m := x.regex.FindStringSubmatch(resp.body)
		if m == nil {
			return "", probe.FailRegex("extract %s: %q does not match %q", x.name, s.excerpt(resp.body), x.regex)
		}
		return m[len(m)-1], nil
	}
}

// stepError names the step in a failure and keeps its classification.
func stepError(name string, err error) error {
	var pe *probe.Error
	if errors.As(err, &pe) {
		return &probe.Error{Reason: pe.Reason, Err: fmt.Errorf("step %s: %w", name, pe.Err), Regex: pe.Regex}
	}
	return probe.Wrap(probe.ReasonUnknown, fmt.Errorf("step %s: %w", name, err))
}

// redact keeps the secrets out of the failure the runner logs and publishes.
func (r *run) redact(err error) error {
	if err == nil || r.scope == nil {
		return err
	}
	var pe *probe.Error
	if !errors.As(err, &pe) {
		return errors.New(r.scope.redact(err.Error()))
	}
	return &probe.Error{Reason: pe.Reason, Err: errors.New(r.scope.redact(pe.Err.Error())), Regex: pe.Regex}
}

// finish completes the report: the steps a failure kept from running are
// listed too, so a report always shows the whole scenario. Every step's
// failure counter moves by its outcome, 0 included, so each series is there
// from the first run and increase() over it reads every run.
func (r *run) finish(err error) Report {
	rep := r.report
	rep.Seconds = time.Since(rep.Started).Seconds()
	rep.Success = err == nil
	if err != nil {
		rep.Error = bounded(probe.Message(err))
	}
	for _, st := range r.sc.steps[len(rep.Steps):] {
		rep.Steps = append(rep.Steps, StepReport{Name: st.name, Status: StatusNotRun})
	}
	for _, st := range r.sc.cleanup[len(rep.Cleanup):] {
		rep.Cleanup = append(rep.Cleanup, StepReport{Name: st.name, Status: StatusNotRun})
	}
	// A nil recorder is what made a run panic; finish still has a report
	// to keep.
	if r.rec != nil {
		for _, sr := range slices.Concat(rep.Steps, rep.Cleanup) {
			r.rec.Count(SeriesStepFailures, metrics.Bool(sr.Status == StatusFailed), "step", sr.Name)
		}
	}
	return rep
}

// reportMax bounds a failure kept in a report: it is shown on every status
// page until the next run, and the text in it is mostly the target's.
const reportMax = 1 << 10

func bounded(msg string) string {
	if len(msg) <= reportMax {
		return msg
	}
	n := reportMax
	for n > 0 && !utf8.RuneStart(msg[n]) {
		n--
	}
	return msg[:n] + "…"
}
