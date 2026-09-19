package metrics

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestLabelsNormalizeAndOverride(t *testing.T) {
	l := L("b", "2", "a", "1").With("a", "9")
	if got := l.String(); got != `{a="9",b="2"}` {
		t.Fatalf("labels: %s", got)
	}
	if got := l.With("b", "").String(); got != `{a="9"}` {
		t.Fatalf("label removal: %s", got)
	}
}

func TestLabelsMergeOtherWins(t *testing.T) {
	base := L("probe", "web", "env", "prod")
	got := base.Merge(L("env", "stage")).Get("env")
	if got != "stage" {
		t.Fatalf("Merge: env=%q, want stage", got)
	}
	if base.Get("env") != "prod" {
		t.Fatal("Merge mutated the receiver")
	}
}

func TestStoreAccumulates(t *testing.T) {
	s := NewStore(0, 0)
	write := func(v Value) {
		s.Write(context.Background(), Batch{
			Time:    time.Now(),
			Samples: []Sample{{Name: "probe_total", Labels: L("probe", "x"), Value: v}},
		})
	}
	write(Counter(1))
	write(Counter(1))

	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("series: %d, want 1", len(snap))
	}
	if c, ok := snap[0].Value.(Counter); !ok || c != 2 {
		t.Fatalf("counter: %v, want 2", snap[0].Value)
	}
}

func TestStoreGaugeReplaced(t *testing.T) {
	s := NewStore(0, 0)
	for _, v := range []Gauge{1, 0} {
		s.Write(context.Background(), Batch{
			Time:    time.Now(),
			Samples: []Sample{{Name: "probe_up", Labels: L("probe", "x"), Value: v}},
		})
	}
	if g := s.Snapshot()[0].Value.(Gauge); g != 0 {
		t.Fatalf("gauge: %v, want 0", g)
	}
}

func TestStoreDropsStaleSeries(t *testing.T) {
	s := NewStore(time.Minute, 0)
	now := time.Now()
	s.now = func() time.Time { return now }

	s.Write(context.Background(), Batch{
		Time:    now.Add(-2 * time.Minute),
		Samples: []Sample{{Name: "probe_up", Labels: L("backend", "1.1.1.1"), Value: Gauge(1)}},
	})
	s.Write(context.Background(), Batch{
		Time:    now,
		Samples: []Sample{{Name: "probe_up", Labels: L("backend", "2.2.2.2"), Value: Gauge(1)}},
	})

	snap := s.Snapshot()
	if len(snap) != 1 || snap[0].Labels.Get("backend") != "2.2.2.2" {
		t.Fatalf("stale series still exposed: %+v", snap)
	}
	if n := s.Sweep(); n != 1 {
		t.Fatalf("Sweep removed %d series, want 1", n)
	}
}

func TestDistMerge(t *testing.T) {
	b := Buckets{0.1, 1}
	a, c := NewDist(b), NewDist(b)
	a.Observe(0.05)
	c.Observe(5)
	a.Merge(c)

	if a.Count != 2 {
		t.Fatalf("count=%d, want 2", a.Count)
	}
	if a.Counts[0] != 1 || a.Counts[2] != 1 {
		t.Fatalf("buckets: %v", a.Counts)
	}
}

func TestRecorderChildSharesBuffer(t *testing.T) {
	r := NewRecorder(L("probe", "x"))
	r.With("phase", "tls").Gauge("d", 1)
	if got := r.Samples(); len(got) != 1 || got[0].Labels.Get("phase") != "tls" {
		t.Fatalf("child recorder wrote elsewhere: %+v", got)
	}
}

// Past the cap new series are counted as dropped; existing ones keep updating.
func TestStoreSeriesCap(t *testing.T) {
	s := NewStore(0, 2)
	write := func(backend string) {
		s.Write(context.Background(), Batch{
			Time:    time.Now(),
			Samples: []Sample{{Name: "probe_total", Labels: L("backend", backend), Value: Counter(1)}},
		})
	}
	write("a")
	write("b")
	write("c")
	write("a")

	if got := s.Len(); got != 2 {
		t.Fatalf("series: %d, want 2", got)
	}
	if got := s.Dropped(); got != 1 {
		t.Fatalf("dropped: %d, want 1", got)
	}
	for _, sm := range s.Snapshot() {
		if sm.Labels.Get("backend") == "a" && sm.Value.(Counter) != 2 {
			t.Fatalf("an existing series stopped updating: %v", sm.Value)
		}
	}
}

func TestStoreDrop(t *testing.T) {
	s := NewStore(0, 0)
	s.Write(context.Background(), Batch{Time: time.Now(), Samples: []Sample{
		{Name: "probe_up", Labels: L("probe", "a"), Value: Gauge(1)},
		{Name: "probe_up", Labels: L("probe", "b"), Value: Gauge(1)},
	}})
	if n := s.Drop(func(sm Sample) bool { return sm.Labels.Get("probe") == "a" }); n != 1 {
		t.Fatalf("dropped %d series, want 1", n)
	}
	if got := s.Snapshot(); len(got) != 1 || got[0].Labels.Get("probe") != "b" {
		t.Fatalf("remaining: %+v", got)
	}
}

// The exposition format needs every series of one metric adjacent.
func TestSnapshotGroupsByName(t *testing.T) {
	s := NewStore(0, 0)
	for _, name := range []string{"probe_total", "probe", "probe_up", "probe_total"} {
		for _, backend := range []string{"2.2.2.2", "1.1.1.1"} {
			s.Write(context.Background(), Batch{Time: time.Now(), Samples: []Sample{
				{Name: name, Labels: L("backend", backend), Value: Gauge(1)},
			}})
		}
	}

	var order []string
	seen := map[string]bool{}
	for _, sm := range s.Snapshot() {
		if len(order) == 0 || order[len(order)-1] != sm.Name {
			if seen[sm.Name] {
				t.Fatalf("series of %s are not adjacent: %v", sm.Name, order)
			}
			order = append(order, sm.Name)
			seen[sm.Name] = true
		}
	}
	if len(order) != 3 {
		t.Fatalf("metric names: %v", order)
	}
}

func TestSnapshotReordersOnlyWhenSeriesChange(t *testing.T) {
	s := NewStore(0, 0)
	write := func(backend string) {
		s.Write(context.Background(), Batch{Time: time.Now(), Samples: []Sample{
			{Name: "probe_up", Labels: L("backend", backend), Value: Gauge(1)},
		}})
	}
	write("1.1.1.1")
	_ = s.Snapshot()
	if s.dirty {
		t.Fatal("still dirty after a snapshot")
	}
	write("1.1.1.1")
	if s.dirty {
		t.Fatal("updating an existing series marked the order dirty")
	}
	write("2.2.2.2")
	if !s.dirty {
		t.Fatal("a new series did not mark the order dirty")
	}
	if got := len(s.Snapshot()); got != 2 {
		t.Fatalf("series: %d", got)
	}
}

func TestSnapshotIsIndependentOfLaterWrites(t *testing.T) {
	s := NewStore(time.Hour, 0)
	ctx := context.Background()
	buckets := Buckets{0.01, 0.1}
	write := func(v float64) {
		s.Write(ctx, Batch{Time: time.Now(), Samples: []Sample{
			{Name: "probe_duration_seconds", Labels: L("probe", "a"), Value: Observation{Buckets: buckets, Value: v}},
			{Name: "probe_total", Labels: L("probe", "a"), Value: Counter(1)},
		}})
	}
	write(0.005)
	before := s.Snapshot()

	for i := 0; i < 10; i++ {
		write(0.5)
	}

	d := before[0].Value.(*Dist)
	if d.Count != 1 || d.Sum != 0.005 {
		t.Fatalf("the snapshot changed under later writes: count=%d sum=%v", d.Count, d.Sum)
	}
	if got := before[1].Value.(Counter); got != 1 {
		t.Fatalf("counter in the snapshot changed: %v", got)
	}
}

func TestObservationsFoldIntoOneHistogram(t *testing.T) {
	s := NewStore(time.Hour, 0)
	ctx := context.Background()
	buckets := Buckets{0.01, 0.1}
	for _, v := range []float64{0.005, 0.05, 0.5} {
		s.Write(ctx, Batch{Time: time.Now(), Samples: []Sample{
			{Name: "d", Labels: L("p", "x"), Value: Observation{Buckets: buckets, Value: v}},
		}})
	}
	d := s.Snapshot()[0].Value.(*Dist)
	if d.Count != 3 {
		t.Fatalf("count = %d, want 3", d.Count)
	}
	if want := []uint64{1, 1, 1}; !reflect.DeepEqual(d.Counts, want) {
		t.Fatalf("counts = %v, want %v", d.Counts, want)
	}
}

func TestQuantileInterpolatesInsideTheBucket(t *testing.T) {
	d := NewDist(Buckets{1, 2, 3})
	for _, v := range []float64{0.5, 1.5, 2.5, 2.5} {
		d.Observe(v)
	}
	// Ranks 1..4 across buckets [0,1]=1, (1,2]=1, (2,3]=2.
	cases := map[float64]float64{0.25: 1, 0.5: 2, 1: 3}
	for q, want := range cases {
		if got := d.Quantile(q); got != want {
			t.Errorf("q%v = %v, want %v", q, got, want)
		}
	}
	if got := (&Dist{}).Quantile(0.5); got != 0 {
		t.Errorf("empty distribution: %v, want 0", got)
	}
}

func TestAppendKeyMatchesKey(t *testing.T) {
	for _, l := range []Labels{
		nil,
		L("a", "1"),
		L("probe", "web", "backend", "1.2.3.4:443", "family", "ipv4"),
	} {
		if got := string(l.AppendKey(nil)); got != l.Key() {
			t.Errorf("AppendKey(%v) = %q, Key = %q", l, got, l.Key())
		}
	}
}

func TestEmptyLabelValuesAreDropped(t *testing.T) {
	if got := L("a", "1", "b", "").Key(); got != L("a", "1").Key() {
		t.Errorf("L kept an empty value: %q", got)
	}
	if got := L("a", "1", "b", "2").With("b", ""); got.Get("b") != "" || len(got) != 1 {
		t.Errorf("With did not remove the label: %v", got)
	}
	base := L("probe", "web")
	if got := base.Merge(L("server", "")); len(got) != 1 {
		t.Errorf("Merge kept an empty value: %v", got)
	}
}

func TestReservedLabelsAreRejected(t *testing.T) {
	for _, name := range []string{"le", "val", "quantile"} {
		if err := ValidateLabels(L(name, "x")); err == nil {
			t.Errorf("label %q was accepted", name)
		}
	}
	if err := ValidateLabels(L("probe", "web", "env", "prod")); err != nil {
		t.Errorf("an ordinary label set was rejected: %v", err)
	}
}

func TestLabelValuesCannotForgeASeriesKey(t *testing.T) {
	forged := L("a", "x\x1fb\x1ey")
	if forged.Key() != L("a", "x", "b", "y").Key() {
		t.Skip("the key format changed; this test guarded the old one")
	}
	if err := ValidateLabels(forged); err == nil {
		t.Error("a value containing a key separator was accepted")
	}
}
