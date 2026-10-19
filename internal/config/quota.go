package config

import (
	"fmt"
	"time"
)

// Quota caps how many requests a probe may send its targets in a calendar
// period: for a target that costs money per call, or that limits its callers.
// A run that would not fit is not started, and the probe waits for the next
// period; on-demand checks of the probe draw on the same quota.
//
// A request is one check of one address, each repeat of requests_per_probe
// included. Redirect hops and a scenario's steps are counted as they happen.
type Quota struct {
	Requests int `yaml:"requests"`
	// Per is the period: hour, day, week (from Monday) or month, in the probe's
	// timezone.
	Per string `yaml:"per"`
}

// Quota periods.
const (
	PerHour  = "hour"
	PerDay   = "day"
	PerWeek  = "week"
	PerMonth = "month"
)

func (q *Quota) validate() error {
	if q == nil {
		return nil
	}
	if q.Requests <= 0 {
		return fmt.Errorf("quota.requests: want a positive number, got %d", q.Requests)
	}
	switch q.Per {
	case PerHour, PerDay, PerWeek, PerMonth:
	case "":
		return fmt.Errorf("quota.per is missing: hour, day, week or month")
	default:
		return fmt.Errorf("quota.per: want hour, day, week or month, got %q", q.Per)
	}
	return nil
}

// Window is the period that holds t, in loc: [start, end).
func (q *Quota) Window(t time.Time, loc *time.Location) (start, end time.Time) {
	return PeriodWindow(q.Per, t, loc)
}

// PeriodWindow is the calendar period of the given kind holding t, in loc.
func PeriodWindow(per string, t time.Time, loc *time.Location) (start, end time.Time) {
	if loc == nil {
		loc = time.Local
	}
	t = t.In(loc)
	y, m, d := t.Date()
	switch per {
	case PerHour:
		start = time.Date(y, m, d, t.Hour(), 0, 0, 0, loc)
		// Added: an hour a clock change repeats is still an hour long.
		return start, start.Add(time.Hour)
	case PerWeek:
		back := (int(t.Weekday()) + 6) % 7 // days since Monday
		start = time.Date(y, m, d-back, 0, 0, 0, 0, loc)
		return start, time.Date(y, m, d-back+7, 0, 0, 0, 0, loc)
	case PerMonth:
		return time.Date(y, m, 1, 0, 0, 0, 0, loc), time.Date(y, m+1, 1, 0, 0, 0, 0, loc)
	default:
		return time.Date(y, m, d, 0, 0, 0, 0, loc), time.Date(y, m, d+1, 0, 0, 0, 0, loc)
	}
}
