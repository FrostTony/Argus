package config

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestCronListRunsAtEveryTimeItNames(t *testing.T) {
	// 09:15 and 18:40 every day: one crontab line would also run at 09:40 and 18:15.
	c, err := ParseCrons([]string{"15 9 * * *", "40 18 * * *"}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	from := utc("2026-10-19T00:00:00Z")
	var got []string
	for at := c.Next(from); len(got) < 4; at = c.Next(at) {
		got = append(got, at.Format("02 15:04"))
	}
	if want := "19 09:15,19 18:40,20 09:15,20 18:40"; strings.Join(got, ",") != want {
		t.Fatalf("runs %v, want %s", got, want)
	}
	if gap := c.MinGap(); gap != 9*time.Hour+25*time.Minute {
		t.Fatalf("MinGap = %s, want 9h25m", gap)
	}
	if runs := c.Runs(from, from.Add(7*24*time.Hour)); runs != 14 {
		t.Fatalf("runs in a week = %d, want 14", runs)
	}
	if c.String() != "15 9 * * *; 40 18 * * *" {
		t.Fatalf("String = %q", c.String())
	}
}

func TestCronListCountsASharedTimeOnce(t *testing.T) {
	c, err := ParseCrons([]string{"0 * * * *", "0 */2 * * *"}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	from := utc("2026-10-19T00:00:00Z")
	if runs := c.Runs(from, from.Add(24*time.Hour)); runs != 24 {
		t.Fatalf("runs = %d, want 24: a time both name runs once", runs)
	}
	if gap := c.MinGap(); gap != time.Hour {
		t.Fatalf("MinGap = %s, want 1h", gap)
	}
}

func TestCronListRefusesABadMember(t *testing.T) {
	_, err := ParseCrons([]string{"0 9 * * *", "61 * * * *"}, time.UTC)
	if err == nil || !strings.Contains(err.Error(), `"61 * * * *"`) {
		t.Fatalf("err = %v, want the bad specification named", err)
	}
	if _, err := ParseCrons(nil, time.UTC); err == nil {
		t.Fatal("an empty list was taken for a schedule")
	}
}

func TestCronIsAStringOrAList(t *testing.T) {
	p := loadProbes(t, `
probes:
  - name: one
    type: http
    cron: "0 * * * *"
    targets: [https://a.example/]
  - name: two
    type: http
    cron: ["15 9 * * *", "40 18 * * mon-fri"]
    timezone: UTC
    targets: [https://b.example/]
`)
	list, err := p.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(list[0].Cron) != 1 || len(list[1].Cron) != 2 || list[1].Cron[1] != "40 18 * * mon-fri" {
		t.Fatalf("crons: %q, %q", list[0].Cron, list[1].Cron)
	}
	// Written back, one specification is a string again and two are a list.
	out, err := yaml.Marshal(list[0])
	if err != nil || !strings.Contains(string(out), "cron: 0 * * * *") {
		t.Fatalf("marshalled one cron as %q (%v)", out, err)
	}
	out, _ = yaml.Marshal(list[1])
	if !strings.Contains(string(out), "- 15 9 * * *") {
		t.Fatalf("marshalled a list of crons as %q", out)
	}
}

func TestCronListRefusals(t *testing.T) {
	cases := map[string]string{
		`cron: ["0 9 * * *", ""]`:           "empty cron",
		`cron: [{at: "09:00"}]`:             "list of them",
		`cron: ["0 9 * * *", "0 0 30 2 *"]`: "never runs",
	}
	for field, want := range cases {
		err := resolveErr(t, `
probes:
  - name: a
    type: http
    `+field+`
    targets: [https://a.example/]
`)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", field, err, want)
		}
	}
}

func TestCronListRunsAFixedHourOnceWhenTheClockGoesBack(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	// 25 October 2026: 02:00-03:00 happens twice. The every-hour line keeps its
	// pace through both; 02:30 runs once, whichever line the search came from.
	c, err := ParseCrons([]string{"*/20 * * * *", "30 2 * * *"}, berlin)
	if err != nil {
		t.Fatal(err)
	}
	from := utc("2026-10-25T00:00:00Z")
	thirty := 0
	for at := c.Next(from); at.Before(utc("2026-10-25T03:00:00Z")); at = c.Next(at) {
		if at.In(berlin).Format("15:04") == "02:30" {
			thirty++
		}
	}
	if thirty != 1 {
		t.Fatalf("02:30 ran %d times, want once", thirty)
	}
	// Asked from inside the repeated hour, the fixed time is not run again.
	alone, _ := ParseCron("30 2 * * *", berlin)
	if next := alone.Next(utc("2026-10-25T01:10:00Z")); next.In(berlin).Format("02 15:04") != "26 02:30" {
		t.Fatalf("next = %s, want the next day's 02:30", next.In(berlin))
	}
}

// gapByWalking is MinGap the slow way: every run from Next over the span.
func gapByWalking(c *Cron, from, to time.Time) time.Duration {
	var gap time.Duration
	prev := c.Next(from.Add(-time.Nanosecond))
	for !prev.IsZero() && prev.Before(to) {
		next := c.Next(prev)
		if next.IsZero() {
			break
		}
		if d := next.Sub(prev); gap == 0 || d < gap {
			gap = d
		}
		prev = next
	}
	return gap
}

func TestMinGapSeesARareRunAmongFrequentOnes(t *testing.T) {
	// The 1st of June at 00:05 sits five minutes after a */10 run: a timeout of
	// nine minutes would let the 00:00 run swallow it.
	for _, tc := range []struct {
		exprs []string
		want  time.Duration
	}{
		{[]string{"*/10 * * * *", "5 0 1 6 *"}, 5 * time.Minute},
		{[]string{"*/15 * * * *", "7 12 * 7 *"}, 7 * time.Minute},
		{[]string{"0 9 * * mon-fri", "50 8 13 * *"}, 10 * time.Minute},
	} {
		c, err := ParseCrons(tc.exprs, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.MinGap(); got != tc.want {
			t.Errorf("MinGap(%v) = %s, want %s", tc.exprs, got, tc.want)
		}
	}
}

func TestMinGapSeesAHalfHourClockChange(t *testing.T) {
	// Lord Howe Island goes back by half an hour: 01:55 at +11 is followed by
	// 01:35 at +10:30, ten minutes later, though the clock says twenty apart.
	c, err := ParseCron("35,55 * * * *", mustZone(t, "Australia/Lord_Howe"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.MinGap(); got != 10*time.Minute {
		t.Fatalf("MinGap = %s, want 10m", got)
	}
}

func TestCronRunsAFixedTimeOnceWhenTheClockGoesBackThreeHours(t *testing.T) {
	// Casey station went from +11 back to +8 in March 2023: 01:30 came round twice.
	c, err := ParseCron("30 1 * * *", mustZone(t, "Antarctica/Casey"))
	if err != nil {
		t.Fatal(err)
	}
	from := utc("2023-03-08T12:00:00Z")
	if n := c.Runs(from, from.Add(48*time.Hour)); n != 2 {
		t.Fatalf("runs in two days = %d, want 2", n)
	}
}

func TestMinGapMatchesWalkingEveryRun(t *testing.T) {
	// Over the span MinGap measures, its day-by-day shortcut must find what
	// following every run finds. Zones with clock changes of every kind.
	sparse := [][]string{
		{"30 2 * * *"}, {"15 9 * * *", "40 18 * * mon-fri"}, {"0 0 29 2 *", "0 12 * * 0"},
		{"5,35 1-3 * * *"}, {"0 0 1 * fri"}, {"59 23 * * *", "1 0 * * *"}, {"45 1 * * sun", "15 2 * * sun"},
		{"0 0 * * *"}, {"30 0 * * *"},
	}
	dense := [][]string{{"0 2 * * *", "*/30 * * * *"}, {"0 */7 * * *"}, {"*/5 * * * *"}}
	// Four years of the span, a leap year among them: following every run of all
	// 28 takes the better part of a minute.
	const years = 4
	from := time.Date(gapSpanStart, 1, 1, 0, 0, 0, 0, time.UTC).Add(-24 * time.Hour)
	to := time.Date(gapSpanStart+years, 1, 1, 0, 0, 0, 0, time.UTC)
	// Gaza repeats its midnight; Santiago and Tehran change at midnight (or did).
	for _, z := range []string{"UTC", "Europe/Berlin", "America/New_York", "Australia/Lord_Howe", "America/Santiago",
		"Asia/Tehran", "Asia/Gaza", "Antarctica/Casey"} {
		loc := mustZone(t, z)
		for _, exprs := range append(sparse, dense...) {
			c, err := ParseCrons(exprs, loc)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := c.gapOver(gapSpanStart, gapSpanStart+years), gapByWalking(c, from, to); got != want {
				t.Errorf("%s %v: MinGap = %s, walking every run = %s", z, exprs, got, want)
			}
		}
	}
}

func TestMinGapOfManySpecificationsIsQuick(t *testing.T) {
	exprs := make([]string, 200)
	for i := range exprs {
		exprs[i] = strconv.Itoa(i%60) + " " + strconv.Itoa(i%24) + " * * *"
	}
	c, err := ParseCrons(exprs, mustZone(t, "Europe/Berlin"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	c.measureGap()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("MinGap of 200 specifications took %s", d)
	}
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("no zone %s: %v", name, err)
	}
	return loc
}
