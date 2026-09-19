package metrics

import (
	"fmt"
	"math"
	"slices"
	"sort"
)

type Kind uint8

const (
	KindCounter Kind = iota // probers emit a delta; Store accumulates
	KindGauge
	KindDist
	KindInfo // string value, exposed as a label on a *_info metric
)

func (k Kind) String() string {
	switch k {
	case KindCounter:
		return "counter"
	case KindGauge:
		return "gauge"
	case KindDist:
		return "distribution"
	case KindInfo:
		return "info"
	}
	return "unknown"
}

// Value is what a sample carries. Every kind but Dist is immutable.
type Value interface {
	Kind() Kind
}

// materialize returns a value the store may keep and accumulate into.
func materialize(v Value) Value {
	switch t := v.(type) {
	case Observation:
		d := NewDist(t.Buckets)
		d.Observe(t.Value)
		return d
	case *Dist:
		return t.clone()
	}
	return v
}

type Counter float64

func (Counter) Kind() Kind { return KindCounter }

type Gauge float64

func (Gauge) Kind() Kind { return KindGauge }

type Info string

func (Info) Kind() Kind { return KindInfo }

// Buckets holds upper bounds in ascending order, without +Inf.
type Buckets []float64

// Valid reports whether the bounds are usable: finite, ascending and distinct.
func (b Buckets) Valid() error {
	for i, v := range b {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("bucket %d: %v is not a finite bound", i, v)
		}
		if i > 0 && v <= b[i-1] {
			return fmt.Errorf("bucket %d: %v does not come after %v; bounds must ascend and be distinct", i, v, b[i-1])
		}
	}
	return nil
}

func ExponentialBuckets(start, factor float64, n int) Buckets {
	b := make(Buckets, 0, n)
	for v := start; len(b) < n; v *= factor {
		if len(b) > 0 && v <= b[len(b)-1] {
			break // factor 1 or a zero start repeats forever
		}
		b = append(b, v)
	}
	return b
}

// DefaultLatencyBuckets spans 1ms to ~32s.
var DefaultLatencyBuckets = ExponentialBuckets(0.001, 2, 16)

// Observation is a single measurement waiting to be folded into a histogram.
type Observation struct {
	Buckets Buckets `json:"buckets"`
	Value   float64 `json:"value"`
}

func (Observation) Kind() Kind { return KindDist }

// Dist is a histogram. Counts is one longer than Buckets; the last slot is +Inf.
type Dist struct {
	Buckets Buckets  `json:"buckets"`
	Counts  []uint64 `json:"counts"`
	Sum     float64  `json:"sum"`
	Count   uint64   `json:"count"`
}

// NewDist accepts empty bounds, which yields a summary: sum and count only.
func NewDist(b Buckets) *Dist {
	return &Dist{Buckets: b, Counts: make([]uint64, len(b)+1)}
}

func (*Dist) Kind() Kind { return KindDist }

func (d *Dist) clone() *Dist {
	c := &Dist{Buckets: d.Buckets, Sum: d.Sum, Count: d.Count}
	c.Counts = append([]uint64(nil), d.Counts...)
	return c
}

func (d *Dist) Observe(v float64) {
	d.Sum += v
	d.Count++
	d.Counts[sort.SearchFloat64s(d.Buckets, v)]++
}

// Merge combines two distributions; on a bounds mismatch the receiver's win.
func (d *Dist) Merge(o *Dist) {
	d.Sum += o.Sum
	d.Count += o.Count
	if !slices.Equal(d.Buckets, o.Buckets) {
		return
	}
	for i, c := range o.Counts {
		d.Counts[i] += c
	}
}

func (d *Dist) HasBuckets() bool { return len(d.Buckets) > 0 }

// Mean is the average observation, or 0 when nothing was observed.
func (d *Dist) Mean() float64 {
	if d.Count == 0 {
		return 0
	}
	return d.Sum / float64(d.Count)
}

// Quantile estimates a percentile the way Prometheus reads a histogram.
func (d *Dist) Quantile(q float64) float64 {
	if d.Count == 0 || len(d.Buckets) == 0 {
		return 0
	}
	rank := q * float64(d.Count)
	var cum, prevCum float64
	var lower float64
	for i, upper := range d.Buckets {
		cum += float64(d.Counts[i])
		if cum >= rank {
			if width := cum - prevCum; width > 0 {
				return lower + (upper-lower)*(rank-prevCum)/width
			}
			return upper
		}
		prevCum, lower = cum, upper
	}
	// Everything above the last bound: the histogram cannot say how far above.
	return d.Buckets[len(d.Buckets)-1]
}
