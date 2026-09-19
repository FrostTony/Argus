package config

import (
	"cmp"
	"fmt"
	"strings"
	"time"
)

// Schedule limits when a probe runs. It is the YAML shape and nothing else:
// the parsed configuration is shared across reloads, so nothing compiles in place.
type Schedule struct {
	// Windows are when the probe runs; outside all of them it does not.
	Windows []Window `yaml:"windows"`
	// Timezone as an IANA name; the node's local time by default.
	Timezone string `yaml:"timezone"`
}

// Window is a span of the week: days plus a time range.
type Window struct {
	// Days are names or prefixes: mon, tue, ..., or empty for every day.
	Days []string `yaml:"days"`
	From string   `yaml:"from"` // "09:00"
	To   string   `yaml:"to"`   // "18:00"
}

// Compiled is an immutable schedule ready to answer Active.
type Compiled struct {
	windows []compiledWindow
	loc     *time.Location
}

type compiledWindow struct {
	days     map[time.Weekday]bool
	from, to int // minutes since midnight
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// Compile validates the schedule; a nil schedule compiles to nil, always active.
func (s *Schedule) Compile() (*Compiled, error) {
	if s == nil {
		return nil, nil
	}
	if len(s.Windows) == 0 {
		return nil, fmt.Errorf("schedule needs at least one window")
	}

	out := &Compiled{loc: time.Local}
	if s.Timezone != "" {
		loc, err := time.LoadLocation(s.Timezone)
		if err != nil {
			return nil, fmt.Errorf("schedule.timezone: %w", err)
		}
		out.loc = loc
	}
	out.windows = make([]compiledWindow, len(s.Windows))
	for i, w := range s.Windows {
		cw, err := w.compile()
		if err != nil {
			return nil, fmt.Errorf("schedule.windows[%d]: %w", i, err)
		}
		out.windows[i] = cw
	}
	return out, nil
}

func (w Window) compile() (compiledWindow, error) {
	var out compiledWindow
	from, err := parseClock(cmp.Or(w.From, "00:00"))
	if err != nil {
		return out, err
	}
	to, err := parseClock(cmp.Or(w.To, "24:00"))
	if err != nil {
		return out, err
	}
	// Equal ends describe no time at all.
	if from == to {
		return out, fmt.Errorf("from and to are both %s, which is an empty window; leave both out for the whole day", cmp.Or(w.From, "00:00"))
	}
	out.from, out.to = from, to

	if len(w.Days) > 0 {
		out.days = make(map[time.Weekday]bool, len(w.Days))
		for _, d := range w.Days {
			key := strings.ToLower(strings.TrimSpace(d))
			if len(key) > 3 {
				key = key[:3]
			}
			day, ok := weekdays[key]
			if !ok {
				return out, fmt.Errorf("unknown day %q", d)
			}
			out.days[day] = true
		}
	}
	return out, nil
}

func parseClock(s string) (int, error) {
	if s == "24:00" {
		return 24 * 60, nil
	}
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("time %q: want HH:MM", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// Active reports whether the probe may run now; no windows means no restriction.
func (c *Compiled) Active(now time.Time) bool {
	if c == nil || len(c.windows) == 0 {
		return true
	}
	local := now.In(c.loc)
	for _, w := range c.windows {
		if w.contains(local) {
			return true
		}
	}
	return false
}

func (w compiledWindow) contains(t time.Time) bool {
	minutes := t.Hour()*60 + t.Minute()
	if w.from < w.to {
		return w.on(t.Weekday()) && minutes >= w.from && minutes < w.to
	}
	// A window wrapping past midnight belongs to the day it opens on: [fri]
	// 22:00-06:00 covers Saturday until six, not Friday's small hours.
	if minutes >= w.from {
		return w.on(t.Weekday())
	}
	return minutes < w.to && w.on((t.Weekday()+6)%7)
}

func (w compiledWindow) on(d time.Weekday) bool { return w.days == nil || w.days[d] }
