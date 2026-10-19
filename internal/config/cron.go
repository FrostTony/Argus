package config

import (
	"fmt"
	"math/bits"
	"strconv"
	"strings"
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
type Cron struct {
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
	if loc == nil {
		loc = time.Local
	}
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

	c := &Cron{expr: strings.TrimSpace(expr), loc: loc}
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

// String is the specification as written.
func (c *Cron) String() string { return c.expr }

// Location is the time zone the specification is read in.
func (c *Cron) Location() *time.Location { return c.loc }

// Next is the first time strictly after t the specification names, or the
// zero time when there is none within the horizon.
//
// Clock changes go as in Vixie cron. A time the clock skips forward over does
// not run that day. When the clock goes back and repeats an hour, a
// specification that names hours runs once, at the first of the two; one on
// every hour ("*/5 * * * *") keeps its pace through both.
func (c *Cron) Next(after time.Time) time.Time {
	// Truncated on the instant, never rebuilt from the wall clock: in an hour
	// the clock repeats, time.Date names the first of the two, and a search
	// started there walks back before after and round the same hour forever.
	t := after.In(c.loc).Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(0, 0, cronHorizon)
	// A wall-clock time already passed is not run again in a repeated hour.
	seen := wallClock(after.In(c.loc))
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
			!everyHour && !wallClock(t).After(seen):
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
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

func (c *Cron) dayMatches(t time.Time) bool {
	dom := c.dom&(1<<uint(t.Day())) != 0
	dow := c.dow&(1<<uint(t.Weekday())) != 0
	if c.domStar || c.dowStar {
		return dom && dow
	}
	return dom || dow
}

// MinGap is the shortest time between two runs: what a run, its timeout and
// its repeats must fit into, as they fit into an interval. It is measured from
// a fixed day, so the answer does not depend on when it is asked: over a year
// of runs, or, for a schedule dense enough that a year is a long walk, over
// 2000 runs and at least eight days — every kind of day boundary, the week's
// included, is in there.
func (c *Cron) MinGap() time.Duration {
	// A gap is never shorter than a minute; every minute of an hour is that.
	if bits.OnesCount64(c.minute) == 60 {
		return time.Minute
	}
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, c.loc)
	end := start.AddDate(1, 0, 1)
	first := c.Next(start.Add(-time.Minute))
	gap := time.Duration(0)
	prev := first
	for runs := 1; !prev.IsZero(); runs++ {
		next := c.Next(prev)
		if next.IsZero() {
			break
		}
		if d := next.Sub(prev); gap == 0 || d < gap {
			gap = d
		}
		dense := runs >= 2000 && next.Sub(first) >= 8*24*time.Hour
		if gap == time.Minute || dense || next.After(end) {
			break
		}
		prev = next
	}
	return gap
}

// Runs counts the runs in [from, to).
func (c *Cron) Runs(from, to time.Time) int {
	n := 0
	for t := c.Next(from.Add(-time.Nanosecond)); !t.IsZero() && t.Before(to); t = c.Next(t) {
		n++
	}
	return n
}
