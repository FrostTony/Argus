// Package surfacer holds the metric sinks.
package surfacer

import (
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

// Prometheus renders the store in the text exposition format.
type Prometheus struct {
	Store  *metrics.Store
	Prefix string
	// Timestamps is off by default: past timestamps leave gaps in a graph.
	Timestamps bool

	// skipped counts series left out for an unusable name, over all scrapes.
	skipped atomic.Int64
}

// Skipped reports how many series have been left out since start.
func (p *Prometheus) Skipped() int64 { return p.skipped.Load() }

// WriteTo renders the exposition.
func (p *Prometheus) WriteTo(w io.Writer) (int64, error) {
	samples := p.Store.Snapshot()
	// One timestamp for the whole page: a scrape is a moment, not a span.
	now := time.Now().UnixMilli()
	cw := &countingWriter{w: w}
	buf := make([]byte, 0, 4096)

	// Series of one metric arrive together, so its names are built once.
	var (
		lastRaw   string
		names     metricNames
		newMetric bool
	)
	for _, s := range samples {
		newMetric = s.Name != lastRaw
		if newMetric {
			names = newMetricNames(metrics.Prefixed(p.Prefix, s.Name))
			lastRaw = s.Name
		}
		name := names.base
		// One unusable name would make the whole page unparseable.
		if !metrics.ValidMetricName(name) {
			p.skipped.Add(1)
			continue
		}

		buf = buf[:0]
		if newMetric {
			if h, ok := help[s.Name]; ok {
				buf = append(buf, "# HELP "...)
				buf = append(buf, name...)
				buf = append(buf, ' ')
				buf = append(buf, h...)
				buf = append(buf, '\n')
			}
			buf = append(buf, "# TYPE "...)
			buf = append(buf, name...)
			buf = append(buf, ' ')
			buf = append(buf, promType(s.Value)...)
			buf = append(buf, '\n')
		}

		buf = p.appendSample(buf, now, names, s)
		if _, err := cw.Write(buf); err != nil {
			return cw.n, err
		}
	}
	return cw.n, cw.err
}

type metricNames struct {
	base, bucket, sum, count string
}

func newMetricNames(base string) metricNames {
	return metricNames{
		base:   base,
		bucket: base + "_bucket",
		sum:    base + "_sum",
		count:  base + "_count",
	}
}

func (p *Prometheus) appendSample(buf []byte, now int64, n metricNames, s metrics.Sample) []byte {
	switch v := s.Value.(type) {
	case metrics.Counter:
		return p.appendLine(buf, now, n.base, s.Labels, "", "", float64(v))
	case metrics.Gauge:
		return p.appendLine(buf, now, n.base, s.Labels, "", "", float64(v))
	case metrics.Info:
		// A string value becomes the `val` label at 1, the usual *_info shape.
		return p.appendLine(buf, now, n.base, s.Labels, "val", string(v), 1)
	case *metrics.Dist:
		return p.appendDist(buf, now, n, s.Labels, v)
	}
	return buf
}

func (p *Prometheus) appendDist(buf []byte, now int64, n metricNames, l metrics.Labels, d *metrics.Dist) []byte {
	if d.HasBuckets() {
		bucket := n.bucket
		var cum uint64
		for i, b := range d.Buckets {
			cum += d.Counts[i]
			buf = p.appendBucket(buf, now, bucket, l, b, false, float64(cum))
		}
		buf = p.appendBucket(buf, now, bucket, l, 0, true, float64(d.Count))
	}
	buf = p.appendLine(buf, now, n.sum, l, "", "", d.Sum)
	return p.appendLine(buf, now, n.count, l, "", "", float64(d.Count))
}

func (p *Prometheus) appendBucket(buf []byte, now int64, name string, l metrics.Labels, bound float64, inf bool, v float64) []byte {
	buf = append(buf, name...)
	buf = appendLabelsOpen(buf, l)
	buf = append(buf, `le="`...)
	if inf {
		buf = append(buf, "+Inf"...)
	} else {
		buf = strconv.AppendFloat(buf, bound, 'g', -1, 64)
	}
	buf = append(buf, '"', '}', ' ')
	buf = strconv.AppendFloat(buf, v, 'g', -1, 64)
	if p.Timestamps {
		buf = append(buf, ' ')
		buf = strconv.AppendInt(buf, now, 10)
	}
	return append(buf, '\n')
}

// appendLabelsOpen writes the label set leaving the brace open for one more.
func appendLabelsOpen(buf []byte, l metrics.Labels) []byte {
	buf = append(buf, '{')
	for _, lb := range l {
		buf = appendLabel(buf, lb.Name, lb.Value)
		buf = append(buf, ',')
	}
	return buf
}

// appendLine writes one series; the extra label is appended, not merged, as
// the format does not care about label order.
func (p *Prometheus) appendLine(buf []byte, now int64, name string, l metrics.Labels, extraName, extraValue string, v float64) []byte {
	buf = append(buf, name...)
	buf = appendLabels(buf, l, extraName, extraValue)
	buf = append(buf, ' ')
	buf = strconv.AppendFloat(buf, v, 'g', -1, 64)
	if p.Timestamps {
		buf = append(buf, ' ')
		buf = strconv.AppendInt(buf, now, 10)
	}
	return append(buf, '\n')
}

func appendLabels(buf []byte, l metrics.Labels, extraName, extraValue string) []byte {
	if len(l) == 0 && extraName == "" {
		return buf
	}
	buf = append(buf, '{')
	for i, lb := range l {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendLabel(buf, lb.Name, lb.Value)
	}
	if extraName != "" {
		if len(l) > 0 {
			buf = append(buf, ',')
		}
		buf = appendLabel(buf, extraName, extraValue)
	}
	return append(buf, '}')
}

func appendLabel(buf []byte, name, value string) []byte {
	buf = append(buf, name...)
	buf = append(buf, '=', '"')
	buf = appendEscaped(buf, value)
	return append(buf, '"')
}

func appendEscaped(buf []byte, s string) []byte {
	// Most values need no escaping, so scan before copying byte by byte.
	if !strings.ContainsAny(s, "\\\"\n") {
		return append(buf, s...)
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			buf = append(buf, '\\', '\\')
		case '"':
			buf = append(buf, '\\', '"')
		case '\n':
			buf = append(buf, '\\', 'n')
		default:
			buf = append(buf, s[i])
		}
	}
	return buf
}

// promType maps a value kind to a type; a bucketless distribution is a summary.
func promType(v metrics.Value) string {
	switch d := v.(type) {
	case metrics.Counter:
		return "counter"
	case metrics.Gauge:
		return "gauge"
	case metrics.Info:
		return "gauge"
	case *metrics.Dist:
		if d.HasBuckets() {
			return "histogram"
		}
		return "summary"
	}
	return "untyped"
}

// String renders the whole exposition.
func (p *Prometheus) String() string {
	var sb strings.Builder
	_, _ = p.WriteTo(&sb)
	return sb.String()
}

type countingWriter struct {
	w   io.Writer
	n   int64
	err error
}

func (c *countingWriter) Write(b []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n, err := c.w.Write(b)
	c.n += int64(n)
	c.err = err
	return n, err
}
