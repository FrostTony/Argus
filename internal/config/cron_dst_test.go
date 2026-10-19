package config

import (
	"testing"
	"time"
)

func loadZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	return loc
}

// within fails the test instead of hanging it when f does not return.
func within(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return: the search is stuck", what)
	}
}

func TestCronAcrossFallBack(t *testing.T) {
	ny := loadZone(t, "America/New_York")
	// 3 November 2024: 01:00-01:59 is read twice, first EDT (05:xxZ), then EST (06:xxZ).
	cases := []struct {
		expr, after string
		want        []string
	}{
		// An hour the cron does not name, walked through to the one it does.
		{"30 3 * * *", "2024-11-03T03:00:00Z", []string{"2024-11-03T08:30:00Z", "2024-11-04T08:30:00Z"}},
		// A time inside the repeated hour runs once, at its first reading.
		{"30 1 * * *", "2024-11-03T04:00:00Z", []string{"2024-11-03T05:30:00Z", "2024-11-04T06:30:00Z"}},
		// A cron on every hour keeps its pace through both readings.
		{"0 * * * *", "2024-11-03T04:30:00Z", []string{
			"2024-11-03T05:00:00Z", "2024-11-03T06:00:00Z", "2024-11-03T07:00:00Z"}},
	}
	for _, tc := range cases {
		c := mustCron(t, tc.expr, ny)
		within(t, tc.expr, func() {
			at := utc(tc.after)
			for i, want := range tc.want {
				got := c.Next(at)
				if !got.Equal(utc(want)) {
					t.Errorf("%q run %d after %s: %s, want %s", tc.expr, i+1, at.UTC(), got.UTC(), want)
					return
				}
				at = got
			}
		})
	}
}

func TestCronNextIsAfterAnInstantInTheRepeatedHour(t *testing.T) {
	ny := loadZone(t, "America/New_York")
	c := mustCron(t, "* * * * *", ny)
	// 01:30 EST, the second reading of 01:30: rebuilding it from the wall
	// clock names 01:30 EDT, an hour before.
	after := utc("2024-11-03T06:30:00Z")
	if got := c.Next(after); !got.Equal(utc("2024-11-03T06:31:00Z")) {
		t.Fatalf("next = %s, want 06:31Z", got.UTC())
	}
}

func TestCronParsesQuicklyInEveryZone(t *testing.T) {
	for _, zone := range []string{"America/New_York", "Europe/Berlin", "Australia/Lord_Howe",
		"America/Santiago", "Asia/Tehran", "Europe/Moscow", "Asia/Tashkent"} {
		loc := loadZone(t, zone)
		for _, expr := range []string{"0 9 * * *", "30 1 * * *", "15 2 * * 0", "*/7 9-18 * * *"} {
			within(t, zone+" "+expr, func() {
				c, err := ParseCron(expr, loc)
				if err != nil {
					t.Errorf("%s %q: %v", zone, expr, err)
					return
				}
				if c.MinGap() <= 0 {
					t.Errorf("%s %q: min gap %s", zone, expr, c.MinGap())
				}
				if n := c.Runs(utc("2024-01-01T00:00:00Z"), utc("2025-01-01T00:00:00Z")); n == 0 {
					t.Errorf("%s %q: no runs in 2024", zone, expr)
				}
			})
		}
	}
}

func TestCronDailyRunsOncePerDayAllYear(t *testing.T) {
	for _, zone := range []string{"America/New_York", "Europe/Berlin"} {
		loc := loadZone(t, zone)
		c := mustCron(t, "30 1 * * *", loc)
		from := time.Date(2024, 1, 1, 0, 0, 0, 0, loc)
		to := time.Date(2025, 1, 1, 0, 0, 0, 0, loc)
		within(t, zone, func() {
			// 366 days; none of them loses 01:30 to the clock, one reads it twice.
			if n := c.Runs(from, to); n != 366 {
				t.Errorf("%s: %d runs of a daily cron in 2024, want 366", zone, n)
			}
		})
	}
}

func TestCronQuestionMarkIsAnyDay(t *testing.T) {
	// Quartz writes "?" for the day field it leaves open: Mondays only, not
	// every day for matching "any day of month".
	c := mustCron(t, "0 9 ? * mon", time.UTC)
	if n := c.Runs(utc("2026-10-19T00:00:00Z"), utc("2026-10-26T00:00:00Z")); n != 1 {
		t.Fatalf("%d runs in a week, want 1", n)
	}
}

func TestCronSkippedMidnight(t *testing.T) {
	// 8 September 2024 in Santiago starts at 01:00: midnight is skipped, and
	// time.Date reads it as 23:00 the day before.
	scl := loadZone(t, "America/Santiago")
	c := mustCron(t, "15 2 * * 0", scl)
	within(t, "Santiago", func() {
		got := c.Next(utc("2024-09-02T00:00:00Z"))
		if want := utc("2024-09-08T05:15:00Z"); !got.Equal(want) { // 02:15 -03
			t.Errorf("next = %s, want %s", got.UTC(), want)
		}
	})
}

// Every run is after the one before, through a year of every clock change
// of zones that change at 02:00, at midnight, by half an hour and not at all.
func TestCronAlwaysMovesForward(t *testing.T) {
	zones := []string{"UTC", "Europe/Moscow", "America/New_York", "Europe/Berlin",
		"America/Santiago", "Australia/Lord_Howe", "America/Havana", "Asia/Gaza"}
	exprs := []string{"0 * * * *", "30 0,1,2 * * *",
		"0 0 * * *", "59 23 * * 6", "0 0 1 * *", "45 1 * 3,4,9,10,11 0"}
	for _, zone := range zones {
		loc := loadZone(t, zone)
		for _, expr := range exprs {
			c := mustCron(t, expr, loc)
			within(t, zone+" "+expr, func() {
				at := time.Date(2024, 1, 1, 0, 0, 0, 0, loc)
				end := at.AddDate(1, 0, 0)
				for at.Before(end) {
					next := c.Next(at)
					if !next.After(at) {
						t.Errorf("%s %q: next(%s) = %s, not after", zone, expr, at, next)
						return
					}
					at = next
				}
			})
		}
	}
}
