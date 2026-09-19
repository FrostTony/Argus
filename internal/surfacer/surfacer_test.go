package surfacer

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang/snappy"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

func storeWith(samples ...metrics.Sample) *metrics.Store {
	s := metrics.NewStore(0, 0)
	s.Write(context.Background(), metrics.Batch{Time: time.Now(), Samples: samples})
	return s
}

func TestPrometheusRendersEveryKind(t *testing.T) {
	d := metrics.NewDist(metrics.Buckets{0.1, 1})
	d.Observe(0.05)
	d.Observe(5)

	p := &Prometheus{Prefix: "argus_", Store: storeWith(
		metrics.Sample{Name: "probe_total", Labels: metrics.L("probe", "a"), Value: metrics.Counter(3)},
		metrics.Sample{Name: "probe_up", Labels: metrics.L("probe", "a"), Value: metrics.Gauge(1)},
		metrics.Sample{Name: "tls_version_info", Labels: metrics.L("probe", "a"), Value: metrics.Info("TLS1.3")},
		metrics.Sample{Name: "probe_duration_seconds", Labels: metrics.L("probe", "a"), Value: d},
	)}
	out := p.String()

	for _, want := range []string{
		"# TYPE argus_probe_total counter",
		`argus_probe_total{probe="a"} 3`,
		"# TYPE argus_probe_up gauge",
		`argus_tls_version_info{probe="a",val="TLS1.3"} 1`,
		"# TYPE argus_probe_duration_seconds histogram",
		`argus_probe_duration_seconds_bucket{probe="a",le="0.1"} 1`,
		`argus_probe_duration_seconds_bucket{probe="a",le="+Inf"} 2`,
		`argus_probe_duration_seconds_count{probe="a"} 2`,
		"# HELP argus_probe_total Probe attempts.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing from the exposition: %s\n---\n%s", want, out)
		}
	}
}

func TestPrometheusRendersBucketlessDistAsSummary(t *testing.T) {
	d := metrics.NewDist(nil)
	d.Observe(0.25)
	p := &Prometheus{Prefix: "argus_", Store: storeWith(
		metrics.Sample{Name: "probe_duration_seconds", Value: d},
	)}
	out := p.String()

	if !strings.Contains(out, "# TYPE argus_probe_duration_seconds summary") {
		t.Errorf("want a summary:\n%s", out)
	}
	if strings.Contains(out, "_bucket") {
		t.Errorf("a bucketless distribution emitted buckets:\n%s", out)
	}
}

func TestPrometheusEscapesLabelValues(t *testing.T) {
	p := &Prometheus{Prefix: "argus_", Store: storeWith(
		metrics.Sample{Name: "probe_up", Labels: metrics.L("target", `a"b\c`+"\n"), Value: metrics.Gauge(1)},
	)}
	if got := p.String(); !strings.Contains(got, `target="a\"b\\c\n"`) {
		t.Fatalf("label value not escaped: %s", got)
	}
}

// A skipped name must not take the rest of the page with it.
func TestPrometheusSkipsUnusableNames(t *testing.T) {
	p := &Prometheus{Prefix: "argus_", Store: storeWith(
		metrics.Sample{Name: "bad-name", Value: metrics.Gauge(1)},
		metrics.Sample{Name: "good_name", Value: metrics.Gauge(1)},
	)}
	out := p.String()

	if strings.Contains(out, "bad-name") {
		t.Errorf("an unusable name reached the exposition:\n%s", out)
	}
	if !strings.Contains(out, "argus_good_name") {
		t.Errorf("a valid series was dropped with it:\n%s", out)
	}
	if p.Skipped() != 1 {
		t.Errorf("skipped %d series, want 1", p.Skipped())
	}
}

func TestFileSurfacerWritesJSONLines(t *testing.T) {
	path := t.TempDir() + "/metrics.jsonl"
	f, err := NewFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(context.Background(), metrics.Batch{Time: time.Now(), Samples: []metrics.Sample{
		{Name: "probe_up", Labels: metrics.L("probe", "a"), Value: metrics.Gauge(1)},
	}})
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	body := readFile(t, path)
	for _, want := range []string{`"name":"probe_up"`, `"kind":"gauge"`, `"probe":"a"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in:\n%s", want, body)
		}
	}
}

func TestFileSurfacerReopens(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/metrics.jsonl"
	f, err := NewFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	sample := metrics.Batch{Time: time.Now(), Samples: []metrics.Sample{
		{Name: "probe_up", Value: metrics.Gauge(1)},
	}}
	f.Write(context.Background(), sample)
	rename(t, path, dir+"/metrics.jsonl.1")

	if err := f.Reopen(); err != nil {
		t.Fatal(err)
	}
	f.Write(context.Background(), sample)

	if body := readFile(t, path); !strings.Contains(body, "probe_up") {
		t.Fatalf("nothing was written to the new file:\n%s", body)
	}
}

// The protobuf is hand-rolled, so this decodes what actually goes on the wire.
func TestRemoteWriteSendsDecodableProtobuf(t *testing.T) {
	var (
		mu   sync.Mutex
		body []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = raw
		mu.Unlock()
		if got := r.Header.Get("Content-Encoding"); got != "snappy" {
			t.Errorf("Content-Encoding: %q", got)
		}
	}))
	defer srv.Close()

	store := storeWith(metrics.Sample{
		Name:   "probe_up",
		Labels: metrics.L("probe", "a"),
		Value:  metrics.Gauge(1),
	})
	rw := NewRemoteWrite(RemoteWriteOptions{
		URL:      srv.URL,
		Interval: time.Hour,
		Store:    store,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	_ = rw.Close() // flushes once on the way out

	mu.Lock()
	raw := body
	mu.Unlock()

	decoded, err := snappy.Decode(nil, raw)
	if err != nil {
		t.Fatalf("snappy: %v", err)
	}
	series := decodeWriteRequest(t, decoded)
	if len(series) != 1 {
		t.Fatalf("series: %d, want 1", len(series))
	}
	if series[0].labels["__name__"] != "probe_up" || series[0].labels["probe"] != "a" {
		t.Fatalf("labels: %v", series[0].labels)
	}
	if series[0].value != 1 {
		t.Fatalf("value: %v", series[0].value)
	}
	if rw.Sent() != 1 {
		t.Fatalf("sent counter: %d", rw.Sent())
	}
}

func TestRemoteWriteDoesNotRetryPermanentErrors(t *testing.T) {
	var attempts int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	rw := NewRemoteWrite(RemoteWriteOptions{
		URL:        srv.URL,
		Interval:   time.Hour,
		MaxRetries: 3,
		Store: storeWith(metrics.Sample{
			Name: "probe_up", Value: metrics.Gauge(1),
		}),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	_ = rw.Close()

	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Fatalf("attempts: %d, want 1", attempts)
	}
	if rw.Failures() != 1 {
		t.Fatalf("failures: %d, want 1", rw.Failures())
	}
}

type decodedSeries struct {
	labels map[string]string
	value  float64
}

// decodeWriteRequest is a minimal protobuf reader, enough for WriteRequest.
func decodeWriteRequest(t *testing.T, b []byte) []decodedSeries {
	t.Helper()
	var out []decodedSeries
	for len(b) > 0 {
		field, payload, rest := readField(t, b)
		b = rest
		if field != 1 {
			continue
		}
		out = append(out, decodeTimeSeries(t, payload))
	}
	return out
}

func decodeTimeSeries(t *testing.T, b []byte) decodedSeries {
	t.Helper()
	s := decodedSeries{labels: map[string]string{}}
	for len(b) > 0 {
		field, payload, rest := readField(t, b)
		b = rest
		switch field {
		case 1:
			name, value := decodeLabel(t, payload)
			s.labels[name] = value
		case 2:
			s.value = decodeSample(t, payload)
		}
	}
	return s
}

func decodeLabel(t *testing.T, b []byte) (string, string) {
	t.Helper()
	var name, value string
	for len(b) > 0 {
		field, payload, rest := readField(t, b)
		b = rest
		if field == 1 {
			name = string(payload)
		} else {
			value = string(payload)
		}
	}
	return name, value
}

func decodeSample(t *testing.T, b []byte) float64 {
	t.Helper()
	if len(b) < 9 || b[0] != 1<<3|1 {
		t.Fatalf("unexpected sample encoding: %v", b)
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(b[1:9]))
}

// readField reads one length-delimited field and returns the rest.
func readField(t *testing.T, b []byte) (int, []byte, []byte) {
	t.Helper()
	key, n := binary.Uvarint(b)
	if n <= 0 {
		t.Fatal("bad field key")
	}
	b = b[n:]
	if key&7 != 2 {
		t.Fatalf("wire type %d is not length-delimited", key&7)
	}
	size, n := binary.Uvarint(b)
	if n <= 0 || uint64(len(b[n:])) < size {
		t.Fatal("truncated field")
	}
	b = b[n:]
	return int(key >> 3), b[:size], b[size:]
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func rename(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkPrometheusWriteTo(b *testing.B) {
	s := metrics.NewStore(time.Hour, 0)
	for i := 0; i < 4000; i++ {
		s.Write(context.Background(), metrics.Batch{Time: time.Now(), Samples: []metrics.Sample{{
			Name:   "probe_duration_seconds",
			Labels: metrics.L("probe", "web", "target", "https://example.com/", "backend", itoa(i)),
			Value:  metrics.NewDist(metrics.DefaultLatencyBuckets),
		}}})
	}
	p := &Prometheus{Prefix: "argus_", Store: s}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.WriteTo(io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}

func itoa(n int) string { return fmt.Sprintf("10.0.%d.%d", n/256, n%256) }

func TestRemoteWriteUsesTheSameNamesAsTheExposition(t *testing.T) {
	series := flatten([]metrics.Sample{
		{Name: "probe_total", Labels: metrics.L("probe", "web"), Value: metrics.Counter(1)},
	}, "argus_", 0)
	if len(series) != 1 {
		t.Fatalf("flatten produced %d series", len(series))
	}
	for _, l := range series[0].labels {
		if l.Name == "__name__" && l.Value != "argus_probe_total" {
			t.Fatalf("__name__ = %q, want argus_probe_total", l.Value)
		}
	}
}

// Remote-write requires labels sorted by name, and "__name__" sorts after
// every uppercase letter.
func TestRemoteWriteSortsLabels(t *testing.T) {
	series := flatten([]metrics.Sample{
		{Name: "probe_total", Labels: metrics.L("Region", "eu", "zone", "z1", "probe", "web"),
			Value: metrics.Counter(1)},
	}, "", 0)
	for _, ts := range series {
		for i := 1; i < len(ts.labels); i++ {
			if ts.labels[i-1].Name > ts.labels[i].Name {
				t.Fatalf("labels are not sorted: %q comes before %q",
					ts.labels[i-1].Name, ts.labels[i].Name)
			}
		}
	}
}

func BenchmarkRemoteWriteEncode(b *testing.B) {
	s := metrics.NewStore(time.Hour, 0)
	for i := 0; i < 2000; i++ {
		s.Write(context.Background(), metrics.Batch{Time: time.Now(), Samples: []metrics.Sample{{
			Name:   "probe_duration_seconds",
			Labels: metrics.L("probe", "web", "target", "https://example.com/", "backend", itoa(i)),
			Value:  metrics.NewDist(metrics.DefaultLatencyBuckets),
		}}})
	}
	samples := s.Snapshot()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sink = snappy.Encode(sink[:0], marshalWriteRequest(flatten(samples, "argus_", 0)))
	}
}

var sink []byte

// A nested size that does not match what is written produces a body no
// receiver can parse.
func TestRemoteWriteEncodesEveryKind(t *testing.T) {
	d := metrics.NewDist(metrics.Buckets{0.1, 0.5})
	d.Observe(0.2)
	d.Observe(3)

	series := flatten([]metrics.Sample{
		{Name: "probe_total", Labels: metrics.L("probe", "web"), Value: metrics.Counter(7)},
		{Name: "build", Labels: metrics.L("node", "n1"), Value: metrics.Info("v1.2")},
		{Name: "probe_duration_seconds", Labels: metrics.L("probe", "web"), Value: d},
	}, "argus_", 1700000000000)

	got := map[string]float64{}
	for _, s := range decodeWriteRequest(t, marshalWriteRequest(series)) {
		key := s.labels["__name__"]
		if le, ok := s.labels["le"]; ok {
			key += "{le=" + le + "}"
		}
		if val, ok := s.labels["val"]; ok {
			key += "{val=" + val + "}"
		}
		got[key] = s.value
	}
	want := map[string]float64{
		"argus_probe_total":                            7,
		"argus_build{val=v1.2}":                        1,
		"argus_probe_duration_seconds_bucket{le=0.1}":  0,
		"argus_probe_duration_seconds_bucket{le=0.5}":  1,
		"argus_probe_duration_seconds_bucket{le=+Inf}": 2,
		"argus_probe_duration_seconds_sum":             3.2,
		"argus_probe_duration_seconds_count":           2,
	}
	if len(got) != len(want) {
		t.Fatalf("decoded %d series, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

// Every documented name has to be one the agent actually emits.
func TestHelpDocumentsOnlyLiveMetrics(t *testing.T) {
	known := map[string]bool{}
	for _, dir := range []string{"../probe", "../app", "../resolve"} {
		collectMetricNames(t, dir, known)
	}
	for name := range help {
		if !known[name] && !strings.HasPrefix(name, "tls_") && !strings.HasPrefix(name, "http_") &&
			!strings.HasPrefix(name, "dns_") && !strings.HasPrefix(name, "icmp_") {
			t.Errorf("help documents %q, which nothing records", name)
		}
	}
}

// collectMetricNames gathers the string literals the agent records under.
func collectMetricNames(t *testing.T, dir string, into map[string]bool) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range regexp.MustCompile(`"([a-z][a-z0-9_]{3,})"`).FindAllStringSubmatch(string(body), -1) {
			into[m[1]] = true
		}
		// Timings produce two names from one prefix.
		for _, m := range regexp.MustCompile(`NewTiming\("([a-z_]+)"\)`).FindAllStringSubmatch(string(body), -1) {
			into[m[1]+"_duration_seconds"] = true
			into[m[1]+"_last_duration_seconds"] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
