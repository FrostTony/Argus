package surfacer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

func batchOf(samples ...metrics.Sample) metrics.Batch {
	return metrics.Batch{Time: time.Now(), Samples: samples}
}

var upBatch = batchOf(metrics.Sample{Name: "probe_up", Value: metrics.Gauge(1)})

// A value taken from a header or a certificate can be any bytes, and one
// invalid sequence makes Prometheus reject the whole scrape.
func TestPrometheusRepairsInvalidUTF8(t *testing.T) {
	out := (&Prometheus{Store: storeWith(metrics.Sample{
		Name:   "websocket_subprotocol_info",
		Labels: metrics.L("probe", "p\xff\"q"),
		Value:  metrics.Info("\xff\xfe"),
	})}).String()
	if !utf8.ValidString(out) {
		t.Fatalf("exposition is not valid UTF-8:\n%q", out)
	}
	want := `websocket_subprotocol_info{probe="p�\"q",val="�"} 1`
	if !strings.Contains(out, want) {
		t.Fatalf("missing %s in:\n%s", want, out)
	}
}

func TestRemoteWriteRepairsInvalidUTF8(t *testing.T) {
	series := flatten([]metrics.Sample{
		{Name: "info\xff", Labels: metrics.L("probe", "p\xff"), Value: metrics.Info("\xff\xfe")},
	}, "", 0)
	got := decodeWriteRequest(t, marshalWriteRequest(series))
	if len(got) != 1 {
		t.Fatalf("decoded %d series, want 1", len(got))
	}
	if l := got[0].labels; l["probe"] != "p�" || l["val"] != "�" {
		t.Fatalf("labels not repaired: %q", l)
	}
}

// A reopen that fails, as when the directory is briefly gone during rotation,
// must leave the surfacer writing and able to reopen later.
func TestFileSurfacerRetriesAFailedReopen(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/logs/m.jsonl"
	mkdir(t, dir+"/logs")
	f, err := NewFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Write(context.Background(), upBatch)

	rename(t, dir+"/logs", dir+"/away")
	if err := f.Reopen(); err == nil {
		t.Fatal("reopen succeeded with the directory gone")
	}
	f.Write(context.Background(), upBatch)
	if n := strings.Count(readFile(t, dir+"/away/m.jsonl"), "probe_up"); n != 2 {
		t.Fatalf("the old file got %d lines, want 2", n)
	}

	mkdir(t, dir+"/logs")
	if err := f.Reopen(); err != nil {
		t.Fatalf("reopen with the directory back: %v", err)
	}
	f.Write(context.Background(), upBatch)
	if !strings.Contains(readFile(t, path), "probe_up") {
		t.Fatal("nothing was written after the reopen")
	}
}

// failOnce fails its first write, like a disk that was briefly full.
type failOnce struct {
	failed bool
	got    strings.Builder
}

var errNoSpace = errors.New("no space left on device")

func (w *failOnce) Write(p []byte) (int, error) {
	if !w.failed {
		w.failed = true
		return 0, errNoSpace
	}
	return w.got.Write(p)
}

func TestFileSurfacerRecoversFromATransientWriteError(t *testing.T) {
	w := &failOnce{}
	f := newFile("test", w, nil)
	f.Write(context.Background(), upBatch)
	f.Write(context.Background(), upBatch)

	if !strings.Contains(w.got.String(), "probe_up") {
		t.Fatal("nothing was written after a transient error")
	}
	if err := f.Reopen(); !errors.Is(err, errNoSpace) {
		t.Fatalf("reopen reported %v, want the write error", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("the write error was reported twice: %v", err)
	}
}

func TestFileSurfacerWritesNonFiniteValues(t *testing.T) {
	d := metrics.NewDist(nil)
	d.Observe(math.Inf(-1))
	w := &strings.Builder{}
	f := newFile("test", w, nil)
	f.Write(context.Background(), batchOf(
		metrics.Sample{Name: "g", Value: metrics.Gauge(math.NaN())},
		metrics.Sample{Name: "c", Value: metrics.Counter(math.Inf(1))},
		metrics.Sample{Name: "d", Value: d},
		metrics.Sample{Name: "o", Value: metrics.Observation{Value: 0.5}},
	))
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got := map[string]any{}
	sc := bufio.NewScanner(strings.NewReader(w.String()))
	for sc.Scan() {
		var e struct {
			Name  string
			Value any
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("%v in %s", err, sc.Bytes())
		}
		got[e.Name] = e.Value
	}
	want := map[string]any{"g": "NaN", "c": "+Inf", "d": "-Inf", "o": 0.5}
	for name, v := range want {
		if got[name] != v {
			t.Errorf("%s = %v, want %v", name, got[name], v)
		}
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
