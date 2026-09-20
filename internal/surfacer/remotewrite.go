package surfacer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/golang/snappy"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

// RemoteWrite periodically ships a store snapshot to a Prometheus-compatible
// backend; the snapshot, not the event stream, as a counter arrives accumulated.
type RemoteWrite struct {
	opts   RemoteWriteOptions
	client *http.Client
	stop   chan struct{}
	done   chan struct{}

	sent     atomic.Int64
	failures atomic.Int64
	lastOK   atomic.Int64

	// stale and cursor track removed series; only the loop touches them.
	stale  []metrics.Sample
	cursor uint64
}

// retiredLog is how many removed series the store remembers for this sink.
const retiredLog = 1 << 16

// staleNaN is the value Prometheus reads as "this series has ended".
var staleNaN = math.Float64frombits(0x7ff0000000000002)

// permanentError marks a rejection that retrying cannot fix.
type permanentError struct{ error }

type RemoteWriteOptions struct {
	URL string
	// Prefix is the same one /metrics uses, so both outputs name a metric alike.
	Prefix       string
	Interval     time.Duration
	Timeout      time.Duration
	Headers      map[string]string
	User         string
	Password     string
	MaxBatchSize int
	MaxRetries   int
	Store        *metrics.Store
	Log          *slog.Logger
}

func NewRemoteWrite(o RemoteWriteOptions) *RemoteWrite {
	if o.Interval <= 0 {
		o.Interval = 30 * time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.MaxBatchSize <= 0 {
		o.MaxBatchSize = 5000
	}
	if o.MaxRetries <= 0 {
		o.MaxRetries = 3
	}
	o.Store.KeepRetired(retiredLog)
	rw := &RemoteWrite{
		opts:   o,
		client: &http.Client{Timeout: o.Timeout},
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go rw.loop()
	return rw
}

// Write is a no-op: data is taken from the store on a schedule.
func (rw *RemoteWrite) Write(context.Context, metrics.Batch) {}

func (rw *RemoteWrite) loop() {
	defer close(rw.done)
	t := time.NewTicker(rw.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-rw.stop:
			rw.flush() // final send before exit
			return
		case <-t.C:
			rw.flush()
		}
	}
}

func (rw *RemoteWrite) flush() {
	gone, cursor := rw.opts.Store.RetiredSince(rw.cursor)
	rw.stale, rw.cursor = append(rw.stale, gone...), cursor
	// Bounded like the store's own log; what is dropped ages out at the far end.
	if over := len(rw.stale) - retiredLog; over > 0 {
		rw.stale = append(rw.stale[:0], rw.stale[over:]...)
	}
	samples := rw.opts.Store.Snapshot()
	if len(samples) == 0 && len(rw.stale) == 0 {
		return
	}
	now := time.Now().UnixMilli()

	// The endings go first and a millisecond earlier: two values for one
	// timestamp are rejected, and a rejected request is dropped whole.
	// Endings the receiver refuses outright are given up on, or they would be
	// refused again ahead of every later flush.
	var refused permanentError
	if err := rw.ship(rw.stale, now-1, flattenStale); err == nil || errors.As(err, &refused) {
		rw.stale = rw.stale[:0]
	}
	if rw.ship(samples, now, flatten) == nil {
		rw.lastOK.Store(time.Now().Unix())
	}
}

// ship sends samples in batches, stopping at the first that does not go out.
func (rw *RemoteWrite) ship(samples []metrics.Sample, ts int64, encode func([]metrics.Sample, string, int64) []timeSeries) error {
	for start := 0; start < len(samples); start += rw.opts.MaxBatchSize {
		end := min(start+rw.opts.MaxBatchSize, len(samples))
		if err := rw.sendWithRetries(encode(samples[start:end], rw.opts.Prefix, ts)); err != nil {
			rw.failures.Add(1)
			rw.opts.Log.Warn("remote-write failed", "url", rw.opts.URL, "err", err)
			return err
		}
		rw.sent.Add(int64(end - start))
	}
	return nil
}

// sendWithRetries retries with backoff; a lost batch returns on the next flush.
func (rw *RemoteWrite) sendWithRetries(series []timeSeries) error {
	// Never a reused buffer: Do can return while the transport still reads it.
	body := snappy.Encode(nil, marshalWriteRequest(series))

	var err error
	delay := time.Second
	for attempt := 0; attempt <= rw.opts.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(delay):
			case <-rw.stop:
				return err
			}
			delay *= 2
		}
		err = rw.send(body)
		if err == nil {
			return nil
		}
		var perm permanentError
		if errors.As(err, &perm) {
			return err
		}
	}
	return err
}

func (rw *RemoteWrite) Sent() int64     { return rw.sent.Load() }
func (rw *RemoteWrite) Failures() int64 { return rw.failures.Load() }

func (rw *RemoteWrite) send(body []byte) error {
	if len(body) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), rw.opts.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rw.opts.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	for k, v := range rw.opts.Headers {
		req.Header.Set(k, v)
	}
	if rw.opts.User != "" {
		req.SetBasicAuth(rw.opts.User, rw.opts.Password)
	}

	resp, err := rw.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		err := fmt.Errorf("status %d", resp.StatusCode)
		// 4xx other than 429 means the request itself is wrong.
		if resp.StatusCode/100 == 4 && resp.StatusCode != http.StatusTooManyRequests {
			return permanentError{err}
		}
		return err
	}
	return nil
}

// Close flushes once more and stops.
func (rw *RemoteWrite) Close() error {
	close(rw.stop)
	<-rw.done
	return nil
}

type timeSeries struct {
	labels []metrics.Label
	value  float64
	ts     int64
}

// flatten expands distributions into _bucket/_sum/_count and Info into a val
// label.
func flatten(samples []metrics.Sample, prefix string, ts int64) []timeSeries {
	return expand(samples, prefix, ts, func(v float64) float64 { return v })
}

// flattenStale expands the same series with the value that ends them.
func flattenStale(samples []metrics.Sample, prefix string, ts int64) []timeSeries {
	return expand(samples, prefix, ts, func(float64) float64 { return staleNaN })
}

func expand(samples []metrics.Sample, prefix string, ts int64, value func(float64) float64) []timeSeries {
	series, labels := sizeOf(samples)
	f := flattener{
		prefix: prefix,
		ts:     ts,
		value:  value,
		out:    make([]timeSeries, 0, series),
		arena:  make([]metrics.Label, 0, labels),
		names:  make(map[nameKey]string),
		les:    make(map[float64]string),
	}
	for _, s := range samples {
		switch v := s.Value.(type) {
		case metrics.Counter:
			f.add(s.Name, "", s.Labels, metrics.Label{}, float64(v))
		case metrics.Gauge:
			f.add(s.Name, "", s.Labels, metrics.Label{}, float64(v))
		case metrics.Info:
			f.add(s.Name, "", s.Labels, metrics.Label{Name: "val", Value: string(v)}, 1)
		case *metrics.Dist:
			if v.HasBuckets() {
				var cum uint64
				for i, b := range v.Buckets {
					cum += v.Counts[i]
					f.add(s.Name, "_bucket", s.Labels, metrics.Label{Name: "le", Value: f.le(b)}, float64(cum))
				}
				f.add(s.Name, "_bucket", s.Labels, metrics.Label{Name: "le", Value: "+Inf"}, float64(v.Count))
			}
			f.add(s.Name, "_sum", s.Labels, metrics.Label{}, v.Sum)
			f.add(s.Name, "_count", s.Labels, metrics.Label{}, float64(v.Count))
		}
	}
	return f.out
}

type flattener struct {
	prefix string
	ts     int64
	value  func(float64) float64
	out    []timeSeries
	arena  []metrics.Label
	names  map[nameKey]string
	les    map[float64]string
}

type nameKey struct{ base, suffix string }

// add appends one series with __name__ folded in, sorted as remote-write
// requires.
func (f *flattener) add(base, suffix string, l metrics.Labels, extra metrics.Label, v float64) {
	start := len(f.arena)
	f.arena = append(f.arena, metrics.Label{Name: "__name__", Value: f.name(base, suffix)})
	f.arena = append(f.arena, l...)
	if extra.Name != "" {
		f.arena = append(f.arena, extra)
	}
	// Capped, so a later append cannot reach into this series' labels.
	out := f.arena[start:len(f.arena):len(f.arena)]
	slices.SortFunc(out, func(a, b metrics.Label) int { return strings.Compare(a.Name, b.Name) })
	f.out = append(f.out, timeSeries{labels: out, value: f.value(v), ts: f.ts})
}

func (f *flattener) name(base, suffix string) string {
	k := nameKey{base, suffix}
	if s, ok := f.names[k]; ok {
		return s
	}
	s := metrics.Prefixed(f.prefix, base) + suffix
	f.names[k] = s
	return s
}

func (f *flattener) le(b float64) string {
	if s, ok := f.les[b]; ok {
		return s
	}
	s := strconv.FormatFloat(b, 'g', -1, 64)
	f.les[b] = s
	return s
}

// sizeOf counts what flatten is about to produce, so it allocates once.
func sizeOf(samples []metrics.Sample) (series, labels int) {
	for _, s := range samples {
		base := len(s.Labels) + 1 // __name__
		switch v := s.Value.(type) {
		case metrics.Counter, metrics.Gauge:
			series++
			labels += base
		case metrics.Info:
			series++
			labels += base + 1 // val
		case *metrics.Dist:
			if v.HasBuckets() {
				n := len(v.Buckets) + 1 // one per bound, plus +Inf
				series += n
				labels += n * (base + 1) // le
			}
			series += 2 // _sum and _count
			labels += 2 * base
		}
	}
	return series, labels
}

// Encoded by hand against the remote-write schema:
//
//	WriteRequest { repeated TimeSeries timeseries = 1; }
//	TimeSeries   { repeated Label labels = 1; repeated Sample samples = 2; }
//	Label        { string name = 1; string value = 2; }
//	Sample       { double value = 1; int64 timestamp = 2; }

// Every field number here is below 16, so its tag is a single byte and the
// sizes below can count it as one.

func marshalWriteRequest(series []timeSeries) []byte {
	n := 0
	for _, s := range series {
		n += nested(timeSeriesSize(s))
	}
	buf := make([]byte, 0, n)
	for _, s := range series {
		buf = appendVarint(buf, key(1, 2))
		buf = appendVarint(buf, uint64(timeSeriesSize(s)))
		buf = appendTimeSeries(buf, s)
	}
	return buf
}

func appendTimeSeries(dst []byte, s timeSeries) []byte {
	for _, l := range s.labels {
		dst = appendVarint(dst, key(1, 2))
		dst = appendVarint(dst, uint64(labelSize(l)))
		dst = appendString(dst, 1, l.Name)
		dst = appendString(dst, 2, l.Value)
	}
	dst = appendVarint(dst, key(2, 2))
	dst = appendVarint(dst, uint64(sampleSize(s.ts)))
	dst = appendVarint(dst, key(1, 1)) // fixed64
	dst = binary.LittleEndian.AppendUint64(dst, math.Float64bits(s.value))
	dst = appendVarint(dst, key(2, 0)) // varint
	return appendVarint(dst, uint64(s.ts))
}

func timeSeriesSize(s timeSeries) int {
	n := nested(sampleSize(s.ts))
	for _, l := range s.labels {
		n += nested(labelSize(l))
	}
	return n
}

func labelSize(l metrics.Label) int {
	return stringSize(l.Name) + stringSize(l.Value)
}

func sampleSize(ts int64) int {
	return 1 + 8 + 1 + varintLen(uint64(ts))
}

// nested is what a submessage of this size costs: tag, length, message.
func nested(size int) int { return 1 + varintLen(uint64(size)) + size }

func stringSize(s string) int { return nested(len(s)) }

func appendString(dst []byte, field int, s string) []byte {
	dst = appendVarint(dst, key(field, 2))
	dst = appendVarint(dst, uint64(len(s)))
	return append(dst, s...)
}

func key(field, wire int) uint64 { return uint64(field)<<3 | uint64(wire) }

func appendVarint(dst []byte, v uint64) []byte {
	return binary.AppendUvarint(dst, v)
}

func varintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}
