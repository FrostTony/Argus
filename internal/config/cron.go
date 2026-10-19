package config

import (
	"fmt"
	"math/bits"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Cron is a crontab(5) time specification compiled for a time zone: minute,
// hour, day of month, month and day of week, or one of the @ shorthands. A
// probe on a cron runs at the times it names, by the wall clock, rather than
// every interval from whenever it started: nodes that never talk to each other
// still check at the same minute, and a count of runs per day is exact.
//
// Day of month and day of week combine as in Vixie cron: when both are
// restricted — neither starts with "*" or "?" — a day matching either runs;
// otherwise a day must match both.
//
// A Cron may hold several specifications and runs at every time any of them
// names: "09:15 and 18:40" is two lines in a crontab, since one line runs at
// every hour it lists times every minute it lists.
type Cron struct {
	specs []*cronSpec
	loc   *time.Location
}

// cronSpec is one specification: one line of a crontab.
type cronSpec struct {
	expr string
	loc  *time.Location

	minute, hour, dom, month, dow uint64
	domStar, dowStar              bool
}

// cronHorizon bounds the search for the next run. Four years and a day reach
// the next 29 February from anywhere; a specification with nothing in that
// span names a date that does not exist.
const cronHorizon = 4*366 + 1

var cronMacros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

type cronField struct {
	name     string
	min, max int
	names    map[string]int
}

var (
	cronMonths = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	cronDays = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

	cronFields = [5]cronField{
		{name: "minute", min: 0, max: 59},
		{name: "hour", min: 0, max: 23},
		{name: "day of month", min: 1, max: 31},
		{name: "month", min: 1, max: 12, names: cronMonths},
		// 7 is Sunday as well as 0, as every cron takes it.
		{name: "day of week", min: 0, max: 7, names: cronDays},
	}
)

// ParseCron compiles a specification for loc; nil loc is the node's local time.
func ParseCron(expr string, loc *time.Location) (*Cron, error) {
	return ParseCrons([]string{expr}, loc)
}

// ParseCrons compiles several specifications into one schedule that runs at
// every time any of them names. A time two of them name runs once.
func ParseCrons(exprs []string, loc *time.Location) (*Cron, error) {
	if loc == nil {
		loc = time.Local
	}
	if len(exprs) == 0 {
		return nil, fmt.Errorf("cron: no specification")
	}
	c := &Cron{loc: loc}
	for _, expr := range exprs {
		spec, err := parseSpec(expr, loc)
		if err != nil {
			return nil, err
		}
		c.specs = append(c.specs, spec)
	}
	return c, nil
}

func parseSpec(expr string, loc *time.Location) (*cronSpec, error) {
	text := strings.TrimSpace(expr)
	if strings.HasPrefix(text, "@") {
		if strings.HasPrefix(text, "@every") {
			return nil, fmt.Errorf("cron %q: a fixed period is interval:, cron names wall-clock times", expr)
		}
		std, ok := cronMacros[strings.ToLower(text)]
		if !ok {
			return nil, fmt.Errorf("cron %q: unknown shorthand; known are @yearly, @monthly, @weekly, @daily, @hourly", expr)
		}
		text = std
	}
	parts := strings.Fields(text)
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields (minute hour day-of-month month day-of-week), got %d", expr, len(parts))
	}

	c := &cronSpec{expr: strings.TrimSpace(expr), loc: loc}
	sets := [5]*uint64{&c.minute, &c.hour, &c.dom, &c.month, &c.dow}
	for i, part := range parts {
		set, err := cronFields[i].parse(part)
		if err != nil {
			return nil, fmt.Errorf("cron %q: %s: %w", expr, cronFields[i].name, err)
		}
		*sets[i] = set
	}
	// Sunday has two numbers; the search knows one.
	if c.dow&(1<<7) != 0 {
		c.dow = c.dow&^(1<<7) | 1
	}
	// "?" is "*" in the day fields, as Quartz writes "no particular day".
	c.domStar = strings.HasPrefix(parts[2], "*") || strings.HasPrefix(parts[2], "?")
	c.dowStar = strings.HasPrefix(parts[4], "*") || strings.HasPrefix(parts[4], "?")

	if c.Next(time.Date(2000, 1, 1, 0, 0, 0, 0, loc)).IsZero() {
		return nil, fmt.Errorf("cron %q never runs: no such date", expr)
	}
	return c, nil
}

// parse reads one field: a comma-separated list of *, N, N-M, each with an
// optional /step; N/step runs from N to the field's maximum.
func (f cronField) parse(s string) (uint64, error) {
	var set uint64
	for _, item := range strings.Split(s, ",") {
		if item == "" {
			return 0, fmt.Errorf("empty item in %q", s)
		}
		span, stepText, stepped := strings.Cut(item, "/")
		step := 1
		if stepped {
			n, err := strconv.Atoi(stepText)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("step %q: want a positive number", stepText)
			}
			step = n
		}
		lo, hi := f.min, f.max
		switch {
		case span == "*" || span == "?":
			if f.max == 7 {
				hi = 6 // * is every day once, not Sunday twice
			}
		default:
			from, to, ranged := strings.Cut(span, "-")
			var err error
			if lo, err = f.value(from); err != nil {
				return 0, err
			}
			switch {
			case ranged:
				if hi, err = f.value(to); err != nil {
					return 0, err
				}
				if hi < lo {
					return 0, fmt.Errorf("range %q runs backwards", span)
				}
			case stepped:
				// N/step means N-max/step.
			default:
				hi = lo
			}
		}
		for v := lo; v <= hi; v += step {
			set |= 1 << v
		}
	}
	return set, nil
}

func (f cronField) value(s string) (int, error) {
	if v, ok := f.names[strings.ToLower(s)]; ok {
		return v, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	if v < f.min || v > f.max {
		return 0, fmt.Errorf("%d is outside %d-%d", v, f.min, f.max)
	}
	return v, nil
}

// String is the specifications as written, joined by "; ".
func (c *Cron) String() string { return strings.Join(c.Exprs(), "; ") }

// Exprs is the specifications as written, in the order given.
func (c *Cron) Exprs() []string {
	out := make([]string, len(c.specs))
	for i, s := range c.specs {
		out[i] = s.expr
	}
	return out
}

// Location is the time zone the specification is read in.
func (c *Cron) Location() *time.Location { return c.loc }

// Next is the first time strictly after t any of the specifications names,
// or the zero time when there is none within the horizon.
func (c *Cron) Next(after time.Time) time.Time {
	var best time.Time
	for _, s := range c.specs {
		if t := s.Next(after); !t.IsZero() && (best.IsZero() || t.Before(best)) {
			best = t
		}
	}
	return best
}

// Next is the first time strictly after t the specification names, or the
// zero time when there is none within the horizon.
//
// Clock changes: a time the clock skips forward over does not run that day
// (Vixie cron would run it late, after a jump of under three hours). When the
// clock goes back and repeats an hour, a specification that names hours runs
// once, at the first of the two, as in Vixie; one on all 24 hours ("*/5 * * * *")
// keeps its pace through both.
func (c *cronSpec) Next(after time.Time) time.Time {
	// Truncated on the instant, never rebuilt from the wall clock: in an hour
	// the clock repeats, time.Date names the first of the two, and a search
	// started there walks back before after and round the same hour forever.
	t := after.In(c.loc).Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(0, 0, cronHorizon)
	everyHour := bits.OnesCount64(c.hour) == 24

	for t.Before(limit) {
		switch {
		case c.month&(1<<uint(t.Month())) == 0:
			t = dayStart(t.Year(), t.Month()+1, 1, c.loc)
		case !c.dayMatches(t):
			t = dayStart(t.Year(), t.Month(), t.Day()+1, c.loc)
		case c.hour&(1<<uint(t.Hour())) == 0:
			// To the top of the next hour by the instant, so a skipped hour is
			// passed over and a repeated one is walked through once.
			t = t.Add(time.Duration(60-t.Minute()) * time.Minute)
		case c.minute&(1<<uint(t.Minute())) == 0,
			// A wall-clock time the clock already showed this day is not run
			// again: whatever the search started from — a time of another
			// specification of the same cron among them.
			!everyHour && repeated(t):
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

// repeated reports whether t's wall-clock reading was already shown earlier, in
// an hour the clock went back over: by half an hour on Lord Howe Island, by
// three hours at Casey station, by one or two anywhere else.
func repeated(t time.Time) bool {
	wall := wallClock(t)
	for _, back := range [...]time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, 3 * time.Hour} {
		if wallClock(t.Add(-back)).Equal(wall) {
			return true
		}
	}
	return false
}

// dayStart is the first instant of the date in loc; the date is normalised as
// time.Date does. A midnight the clock skips is read by time.Date with either
// offset, and one of them lands back on the day before: the day starts then
// with the first minute that reads as it.
func dayStart(year int, month time.Month, day int, loc *time.Location) time.Time {
	t := time.Date(year, month, day, 0, 0, 0, 0, loc)
	want := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	for wallClock(t).Before(want) {
		t = t.Add(time.Minute)
	}
	return t
}

// wallClock is t's reading on the clock, without the zone: two instants of an
// hour the clock repeats read the same.
func wallClock(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
}

func (c *cronSpec) dayMatches(t time.Time) bool {
	dom := c.dom&(1<<uint(t.Day())) != 0
	dow := c.dow&(1<<uint(t.Weekday())) != 0
	if c.domStar || c.dowStar {
		return dom && dow
	}
	return dom || dow
}

// MinGap is the shortest time between two runs: what a run, its timeout and
// its repeats must fit into, as they fit into an interval.
//
// It is measured over a whole turn of the calendar — 28 years from 2025, every
// date on every weekday, every kind of year — so the answer does not depend on
// when it is asked, and no run is missed however rarely it comes: a sparse
// specification among dense ones, the 29th of February. A day the clock does
// not change on runs at its specifications' times on the clock, so such days
// are measured once per set of specifications that match them; a day it
// changes on is walked run by run, which is where a clock going back by half
// an hour brings two runs closer than their times on the clock say.
func (c *Cron) MinGap() time.Duration {
	// A gap is never shorter than a minute; every minute of an hour is that.
	for _, s := range c.specs {
		if bits.OnesCount64(s.minute) == 60 {
			return time.Minute
		}
	}
	key := c.loc.String() + "\x00" + strings.Join(c.Exprs(), "\x00")
	gapCache.Lock()
	gap, ok := gapCache.m[key]
	gapCache.Unlock()
	if ok {
		return gap
	}
	gap = c.measureGap()
	gapCache.Lock()
	// Bounded: a node's configuration names a few hundred specifications, so a
	// full cache is a config churning through new ones; start over.
	if len(gapCache.m) >= 4096 {
		clear(gapCache.m)
	}
	gapCache.m[key] = gap
	gapCache.Unlock()
	return gap
}

// gapCache keeps MinGap by zone and specifications: validation asks it for
// every cron probe of every configuration a node is given, and a reload asks
// again for the probes that changed.
var gapCache = struct {
	sync.Mutex
	m map[string]time.Duration
}{m: map[string]time.Duration{}}

// dayShape is a day without a clock change, in minutes of the day: its first
// and last run and the shortest gap between its runs, zero for a single run.
type dayShape struct{ first, last, gap int }

func (c *Cron) measureGap() time.Duration { return c.gapOver(gapSpanStart, gapSpanEnd) }

// gapOver measures MinGap over the years [from, to) of the span.
func (c *Cron) gapOver(from, to int) time.Duration {
	var (
		gap     time.Duration
		prev    time.Time // the last run so far
		shapes  = map[string]dayShape{}
		match   = make([]byte, len(c.specs))
		pending []time.Time
	)
	note := func(t time.Time) {
		if !prev.IsZero() {
			if d := t.Sub(prev); gap == 0 || d < gap {
				gap = d
			}
		}
		prev = t
	}
	// The runs of days the clock changes on are taken in the order of time, not of
	// dates: a clock going back across midnight (Casey: 02:00 to 23:00 the day
	// before) shows yesterday's last hours again after today's first runs.
	flush := func() {
		slices.SortFunc(pending, time.Time.Compare)
		for i, t := range pending {
			if i == 0 || !t.Equal(pending[i-1]) {
				note(t)
			}
		}
		pending = pending[:0]
	}
	zs := spanOf(c.loc)
	end := time.Date(to, 1, 1, 0, 0, 0, 0, time.UTC)
	for date := time.Date(from, 1, 1, 0, 0, 0, 0, time.UTC); date.Before(end); date = date.AddDate(0, 0, 1) {
		matched := false
		for i, s := range c.specs {
			match[i] = 0
			if s.month&(1<<uint(date.Month())) != 0 && s.dayMatches(date) {
				match[i], matched = 1, true
			}
		}
		// date is the day's wall clock read as UTC: an instant is it less the offset.
		off, steady := zs.steady(date)
		if !steady {
			if matched {
				for i, s := range c.specs {
					if match[i] != 0 {
						pending = s.dayRuns(date, zs, pending)
					}
				}
			}
			continue
		}
		if len(pending) > 0 {
			flush()
		}
		if !matched {
			continue
		}
		sh, ok := shapes[string(match)]
		if !ok {
			sh = c.shape(match)
			shapes[string(match)] = sh
		}
		start := date.Add(-off)
		note(start.Add(time.Duration(sh.first) * time.Minute))
		if sh.gap > 0 && (gap == 0 || time.Duration(sh.gap)*time.Minute < gap) {
			gap = time.Duration(sh.gap) * time.Minute
		}
		prev = start.Add(time.Duration(sh.last) * time.Minute)
	}
	flush()
	return gap
}

// dayRuns appends the specification's runs on a day the clock changes on, as
// Next finds them. A time on the clock is each offset of the day taken from it,
// where that offset is in force then: none in a gap the clock skips, two in an
// hour it repeats - of which a specification that names hours keeps the first.
func (c *cronSpec) dayRuns(date time.Time, zs *zoneSpan, out []time.Time) []time.Time {
	offsets := zs.around(date)
	everyHour := bits.OnesCount64(c.hour) == 24
	for h := 0; h < 24; h++ {
		if c.hour&(1<<uint(h)) == 0 {
			continue
		}
		for m := 0; m < 60; m++ {
			if c.minute&(1<<uint(m)) == 0 {
				continue
			}
			wall := date.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
			var found [4]time.Time
			n := 0
			for _, o := range offsets {
				if t := wall.Add(-o); zs.offsetAt(t) == o && n < len(found) {
					found[n], n = t, n+1
				}
			}
			slices.SortFunc(found[:n], time.Time.Compare)
			if n > 1 && !everyHour {
				n = 1
			}
			out = append(out, found[:n]...)
		}
	}
	return out
}

// zoneSpan is a zone's offset changes over the span MinGap walks, found once
// per zone: past the zone's table, Go reads its rule afresh on every lookup,
// and a lookup per run of every day of 28 years is most of the cost.
type zoneSpan struct {
	first   time.Duration // the offset at the start
	changes []zoneChange  // in order
}

type zoneChange struct {
	at            time.Time // the first instant of the new offset
	before, after time.Duration
}

var zoneSpans sync.Map // zone name -> *zoneSpan

func spanOf(loc *time.Location) *zoneSpan {
	if v, ok := zoneSpans.Load(loc.String()); ok {
		return v.(*zoneSpan)
	}
	offsetOf := func(t time.Time) time.Duration {
		_, s := t.In(loc).Zone()
		return time.Duration(s) * time.Second
	}
	// A day before the span and a day after it: the days at its edges are judged
	// by the clock either side of them.
	t := time.Date(gapSpanStart, 1, 1, 0, 0, 0, 0, time.UTC).Add(-48 * time.Hour)
	end := time.Date(gapSpanEnd, 1, 1, 0, 0, 0, 0, time.UTC).Add(48 * time.Hour)
	zs := &zoneSpan{first: offsetOf(t)}
	cur := zs.first
	// Clocks change at most once in a few hours, never twice in twelve: a look
	// every twelve hours sees each change, and a halving search finds its minute.
	for step := 12 * time.Hour; t.Before(end); t = t.Add(step) {
		next := offsetOf(t.Add(step))
		if next == cur {
			continue
		}
		// In whole seconds: clocks change on one, and an instant half a second
		// late would lose a time on the clock that starts right at the change.
		lo, hi := t.Unix(), t.Add(step).Unix()
		for hi-lo > 1 {
			mid := lo + (hi-lo)/2
			if offsetOf(time.Unix(mid, 0)) == cur {
				lo = mid
			} else {
				hi = mid
			}
		}
		zs.changes = append(zs.changes, zoneChange{at: time.Unix(hi, 0).UTC(), before: cur, after: next})
		cur = next
	}
	v, _ := zoneSpans.LoadOrStore(loc.String(), zs)
	return v.(*zoneSpan)
}

// offsetAt is the offset in force at t.
func (zs *zoneSpan) offsetAt(t time.Time) time.Duration {
	i, _ := slices.BinarySearchFunc(zs.changes, t, func(c zoneChange, t time.Time) int {
		if c.at.After(t) {
			return 1
		}
		return -1
	})
	if i == 0 {
		return zs.first
	}
	return zs.changes[i-1].after
}

// window is the instants a wall-clock day can fall on, whatever the offset
// (they reach -12 and +14 hours), widened by three hours either side - the
// furthest a clock has gone back, so a day whose midnight came round twice is
// seen as one the clock changed on.
func window(date time.Time) (time.Time, time.Time) {
	return date.Add(-17 * time.Hour), date.Add(24*time.Hour + 15*time.Hour)
}

// steady reports the day's offset when the clock keeps it all day.
func (zs *zoneSpan) steady(date time.Time) (time.Duration, bool) {
	from, to := window(date)
	i, _ := slices.BinarySearchFunc(zs.changes, from, func(c zoneChange, t time.Time) int {
		if c.at.Before(t) {
			return -1
		}
		return 1
	})
	if i < len(zs.changes) && zs.changes[i].at.Before(to) {
		return 0, false
	}
	return zs.offsetAt(from), true
}

// around is the offsets the clock shows over a day that changes.
func (zs *zoneSpan) around(date time.Time) []time.Duration {
	from, to := window(date)
	out := []time.Duration{zs.offsetAt(from)}
	for _, c := range zs.changes {
		if !c.at.Before(from) && c.at.Before(to) && !slices.Contains(out, c.after) {
			out = append(out, c.after)
		}
	}
	return out
}

// The span MinGap walks: a whole turn of the Gregorian calendar within a
// century, which needs no century rule. Years to come, which a zone keeps by
// its rules of today; past ones keep the rules of their day, and Moscow, which
// changed its clocks until 2011, would shorten a gap no run will see again.
const gapSpanStart, gapSpanEnd = 2025, 2053

// shape is the day the matching specifications make together, by the clock.
func (c *Cron) shape(match []byte) dayShape {
	var on [24 * 60]bool
	for i, s := range c.specs {
		if match[i] == 0 {
			continue
		}
		for h := 0; h < 24; h++ {
			if s.hour&(1<<uint(h)) == 0 {
				continue
			}
			for m := 0; m < 60; m++ {
				if s.minute&(1<<uint(m)) != 0 {
					on[h*60+m] = true
				}
			}
		}
	}
	sh, last := dayShape{first: -1}, -1
	for m, run := range on {
		if !run {
			continue
		}
		if sh.first < 0 {
			sh.first = m
		}
		if last >= 0 && (sh.gap == 0 || m-last < sh.gap) {
			sh.gap = m - last
		}
		last = m
	}
	sh.last = last
	return sh
}

// Runs counts the runs in [from, to).
func (c *Cron) Runs(from, to time.Time) int {
	n := 0
	for t := c.Next(from.Add(-time.Nanosecond)); !t.IsZero() && t.Before(to); t = c.Next(t) {
		n++
	}
	return n
}
