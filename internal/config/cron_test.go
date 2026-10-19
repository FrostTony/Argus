package config

import (
	"strings"
	"testing"
	"time"
)

func mustCron(t *testing.T, expr string, loc *time.Location) *Cron {
	t.Helper()
	c, err := ParseCron(expr, loc)
	if err != nil {
		t.Fatalf("ParseCron(%q): %v", expr, err)
	}
	return c
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestCronNext(t *testing.T) {
	cases := []struct {
		expr, after, want string
	}{
		{"0 * * * *", "2026-10-19T13:24:10Z", "2026-10-19T14:00:00Z"},
		// Strictly after: a time that is itself a run moves on to the next.
		{"0 * * * *", "2026-10-19T14:00:00Z", "2026-10-19T15:00:00Z"},
		{"*/15 * * * *", "2026-10-19T13:46:00Z", "2026-10-19T14:00:00Z"},
		{"30 9 * * mon-fri", "2026-10-23T10:00:00Z", "2026-10-26T09:30:00Z"}, // Friday past nine → Monday
		{"0 0 1 * *", "2026-12-15T00:00:00Z", "2027-01-01T00:00:00Z"},
		{"0 0 29 2 *", "2026-03-01T00:00:00Z", "2028-02-29T00:00:00Z"},
		// 7 is Sunday too.
		{"0 12 * * 7", "2026-10-19T00:00:00Z", "2026-10-25T12:00:00Z"},
		{"0 12 * * SUN", "2026-10-19T00:00:00Z", "2026-10-25T12:00:00Z"},
		// Both days restricted: either matches (the 1st, or any Friday).
		{"0 0 1 * fri", "2026-10-19T00:00:00Z", "2026-10-23T00:00:00Z"},
		// One of them a star: both must match (a Friday that is also the 13th).
		{"0 0 13 * *", "2026-10-19T00:00:00Z", "2026-11-13T00:00:00Z"},
		{"0 0 */2 * fri", "2026-10-19T00:00:00Z", "2026-10-23T00:00:00Z"}, // */2 counts as a star: day 23 and Friday
		{"5-10/5 3 * jan,jul *", "2026-10-19T00:00:00Z", "2027-01-01T03:05:00Z"},
		{"50/5 * * * *", "2026-10-19T00:56:00Z", "2026-10-19T01:50:00Z"}, // N/step runs to the end of the field
		{"@daily", "2026-10-19T13:00:00Z", "2026-10-20T00:00:00Z"},
		{"@hourly", "2026-10-19T13:00:00Z", "2026-10-19T14:00:00Z"},
		{"@weekly", "2026-10-19T13:00:00Z", "2026-10-25T00:00:00Z"},
	}
	for _, tc := range cases {
		c := mustCron(t, tc.expr, time.UTC)
		got := c.Next(utc(tc.after))
		if !got.Equal(utc(tc.want)) {
			t.Errorf("%q after %s = %s, want %s", tc.expr, tc.after, got.UTC().Format(time.RFC3339), tc.want)
		}
	}
}

func TestCronReadsItsTimezone(t *testing.T) {
	msk, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	c := mustCron(t, "0 9 * * *", msk)
	// Nine in Moscow is six in UTC.
	if got := c.Next(utc("2026-10-19T05:00:00Z")); !got.Equal(utc("2026-10-19T06:00:00Z")) {
		t.Fatalf("next = %s, want 06:00Z", got.UTC())
	}
}

func TestCronAcrossDaylightSaving(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	c := mustCron(t, "0 * * * *", berlin)
	// 29 March 2026: 02:00 local does not exist. The search must step over the
	// gap rather than stall in it.
	got := c.Next(utc("2026-03-29T00:30:00Z"))                 // 01:30 CET
	if want := utc("2026-03-29T01:00:00Z"); !got.Equal(want) { // 03:00 CEST
		t.Fatalf("next = %s, want %s", got.UTC(), want)
	}
	if runs := c.Runs(utc("2026-03-28T23:00:00Z"), utc("2026-03-29T22:00:00Z")); runs != 23 {
		t.Fatalf("a 23-hour day has %d hourly runs, want 23", runs)
	}
}

func TestCronRefusals(t *testing.T) {
	cases := map[string]string{
		"* * * *":        "want 5 fields",
		"* * * * * *":    "want 5 fields",
		"60 * * * *":     "outside 0-59",
		"* 24 * * *":     "outside 0-23",
		"* * 0 * *":      "outside 1-31",
		"* * * 13 *":     "outside 1-12",
		"* * * * 8":      "outside 0-7",
		"10-5 * * * *":   "backwards",
		"*/0 * * * *":    "positive",
		"a * * * *":      "not a number",
		"1,,2 * * * *":   "empty item",
		"@every 5m":      "interval:",
		"@sometimes":     "unknown shorthand",
		"0 0 30 2 *":     "never runs",
		"0 0 31 4,6,9 *": "never runs",
	}
	for expr, want := range cases {
		_, err := ParseCron(expr, time.UTC)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseCron(%q) = %v, want an error with %q", expr, err, want)
		}
	}
}

func TestCronMinGap(t *testing.T) {
	cases := map[string]time.Duration{
		"* * * * *":          time.Minute,
		"*/5 * * * *":        5 * time.Minute,
		"*/7 * * * *":        4 * time.Minute, // :56 to the next hour's :00
		"0 * * * *":          time.Hour,
		"0 23,0 * * *":       time.Hour, // 23:00 to 00:00 the next day
		"0 9 * * mon-fri":    24 * time.Hour,
		"0 9 * * mon,thu":    72 * time.Hour,
		"0 0 1,31 * *":       24 * time.Hour, // 31 January to 1 February
		"0 0 1 1 *":          365 * 24 * time.Hour,
		"0 9-18 * * mon-fri": time.Hour,
	}
	for expr, want := range cases {
		if got := mustCron(t, expr, time.UTC).MinGap(); got != want {
			t.Errorf("MinGap(%q) = %s, want %s", expr, got, want)
		}
	}
	if got := mustCron(t, "0 0 29 2 *", time.UTC).MinGap(); got < 4*365*24*time.Hour {
		t.Errorf("MinGap of a leap day = %s, want four years", got)
	}
}

func TestCronMinGapIsQuick(t *testing.T) {
	start := time.Now()
	for _, expr := range []string{"*/2 * * * *", "*/5 9-18 * * mon-fri", "0 0 29 2 *", "17 */3 1-7 * mon"} {
		mustCron(t, expr, time.UTC).MinGap()
	}
	// Validation runs it for every probe on a cron at every reload.
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("MinGap took %s", d)
	}
}

func TestCronRuns(t *testing.T) {
	// October 2026 has 22 weekdays.
	c := mustCron(t, "0 9 * * mon-fri", time.UTC)
	if got := c.Runs(utc("2026-10-01T00:00:00Z"), utc("2026-11-01T00:00:00Z")); got != 22 {
		t.Fatalf("runs in October = %d, want 22", got)
	}
	// The start is inclusive and the end is not.
	c = mustCron(t, "0 * * * *", time.UTC)
	if got := c.Runs(utc("2026-10-19T00:00:00Z"), utc("2026-10-20T00:00:00Z")); got != 24 {
		t.Fatalf("hourly runs in a day = %d, want 24", got)
	}
}
