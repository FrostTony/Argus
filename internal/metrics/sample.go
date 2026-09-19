// Package metrics is the agent's measurement vocabulary: the values a prober
// records, the labels that identify them, the store that folds them into
// series, and the batches the sinks ship.
package metrics

import (
	"context"
	"time"
)

// Sample is one measurement with its identity; the name carries no prefix.
type Sample struct {
	Name   string `json:"name"`
	Labels Labels `json:"labels"`
	Value  Value  `json:"value"`
}

// Batch is one writer's output at one moment, shared unchanged with every sink.
type Batch struct {
	Time    time.Time `json:"time"`
	Samples []Sample  `json:"samples"`

	// Scope names what the batch is the whole truth about: a gauge or info of
	// that scope it does not repeat is dropped. Unset, nothing is dropped.
	Scope Labels `json:"-"`
	// Every is how often the writer comes back; a series lives that much longer.
	Every time.Duration `json:"-"`
}

// Retire is the batch that says a scope is gone: it repeats nothing.
func Retire(scope Labels, t time.Time) Batch { return Batch{Time: t, Scope: scope} }

// Sink receives batches. Implementations must not block the caller: the runner
// writes from the probe goroutine.
type Sink interface {
	Write(ctx context.Context, b Batch)
}

type SinkFunc func(context.Context, Batch)

func (f SinkFunc) Write(ctx context.Context, b Batch) { f(ctx, b) }

type Fanout []Sink

func (fs Fanout) Write(ctx context.Context, b Batch) {
	for _, s := range fs {
		s.Write(ctx, b)
	}
}

// Recorder collects one run's samples on top of a base label set. Not safe for
// concurrent writes: each goroutine takes its own and merges via Append.
type Recorder struct {
	base Labels
	buf  *[]Sample // shared with child recorders
}

// typicalSamples is what one backend of an HTTP probe emits.
const typicalSamples = 24

func NewRecorder(base Labels) *Recorder {
	buf := make([]Sample, 0, typicalSamples)
	return &Recorder{base: base, buf: &buf}
}

// With returns a child recorder writing into the same buffer.
func (r *Recorder) With(kv ...string) *Recorder {
	return &Recorder{base: r.base.Merge(L(kv...)), buf: r.buf}
}

func (r *Recorder) Base() Labels { return r.base }

func (r *Recorder) labels(extra []string) Labels {
	switch {
	case len(extra) < 2:
		return r.base
	case len(extra) == 2:
		return r.base.With(extra[0], extra[1])
	}
	return r.base.Merge(L(extra...))
}

func (r *Recorder) Count(name string, v float64, extra ...string) {
	r.add(name, Counter(v), extra)
}

func (r *Recorder) Gauge(name string, v float64, extra ...string) {
	r.add(name, Gauge(v), extra)
}

func (r *Recorder) Info(name, text string, extra ...string) {
	if text == "" {
		return
	}
	r.add(name, Info(text), extra)
}

func (r *Recorder) Observe(name string, b Buckets, v float64, extra ...string) {
	r.add(name, Observation{Buckets: b, Value: v}, extra)
}

// Timing is the pair of metric names one measurement produces.
type Timing struct{ hist, last string }

func NewTiming(prefix string) Timing {
	return Timing{hist: prefix + "_duration_seconds", last: prefix + "_last_duration_seconds"}
}

func (t Timing) Hist() string { return t.hist }
func (t Timing) Last() string { return t.last }

// Duration emits a histogram observation and a last-value gauge.
func (r *Recorder) Duration(t Timing, b Buckets, d time.Duration, extra ...string) {
	secs := d.Seconds()
	l := r.labels(extra)
	r.put(t.hist, Observation{Buckets: b, Value: secs}, l)
	r.put(t.last, Gauge(secs), l)
}

func (r *Recorder) add(name string, v Value, extra []string) {
	r.put(name, v, r.labels(extra))
}

func (r *Recorder) put(name string, v Value, l Labels) {
	*r.buf = append(*r.buf, Sample{Name: name, Labels: l, Value: v})
}

func (r *Recorder) Append(samples ...Sample) { *r.buf = append(*r.buf, samples...) }

func (r *Recorder) Samples() []Sample { return *r.buf }

func (r *Recorder) Batch(t time.Time) Batch { return Batch{Time: t, Samples: *r.buf} }
