package config

import (
	"strings"
	"testing"
	"time"
)

func resolveErr(t *testing.T, doc string) error {
	t.Helper()
	var p Probes
	if err := decodeStrictBytes([]byte(doc), &p); err != nil {
		return err
	}
	_, err := p.Resolve()
	return err
}

func TestCronAndIntervalAreAlternatives(t *testing.T) {
	err := resolveErr(t, `
probes:
  - name: a
    type: http
    interval: 1m
    cron: "0 * * * *"
    targets: [https://a.example/]
`)
	if err == nil || !strings.Contains(err.Error(), "alternatives") {
		t.Fatalf("err = %v, want interval and cron refused together", err)
	}
}

func TestCronReplacesAnInheritedInterval(t *testing.T) {
	p := loadProbes(t, `
defaults:
  interval: 30s
  timeout: 5s
templates:
  hourly:
    type: http
    cron: "0 * * * *"
probes:
  - name: on-cron
    template: hourly
    targets: [https://a.example/]
  - name: back-to-interval
    template: hourly
    interval: 2m
    targets: [https://b.example/]
`)
	list, err := p.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Cron != "0 * * * *" || list[0].Interval != 0 {
		t.Fatalf("on-cron: cron %q interval %s; the template's cron must replace the default interval",
			list[0].Cron, list[0].Interval.D())
	}
	if list[1].Cron != "" || list[1].Interval.D() != 2*time.Minute {
		t.Fatalf("back-to-interval: cron %q interval %s; the probe's interval must replace the cron",
			list[1].Cron, list[1].Interval.D())
	}
}

func TestCronTimingIsCheckedAgainstTheGap(t *testing.T) {
	err := resolveErr(t, `
probes:
  - name: a
    type: http
    cron: "*/5 * * * *"
    timeout: 2m
    requests_per_probe: 3
    targets: [https://a.example/]
`)
	if err == nil || !strings.Contains(err.Error(), "shortest gap between cron runs (5m0s)") {
		t.Fatalf("err = %v, want 3 x 2m refused against a 5m gap", err)
	}
}

func TestCronAndTimezoneAreValidated(t *testing.T) {
	cases := map[string]string{
		`cron: "61 * * * *"`:                              "outside 0-59",
		"cron: \"0 9 * * *\"\n    timezone: Mars/Olympus": "timezone",
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

func TestProbeTimezoneIsTheScheduleDefault(t *testing.T) {
	p := loadProbes(t, `
probes:
  - name: a
    type: http
    timezone: Asia/Tokyo
    schedule:
      windows: [{from: "09:00", to: "10:00"}]
    targets: [https://a.example/]
`)
	list, err := p.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	loc, err := list[0].Location()
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	s, err := list[0].Schedule.CompileIn(loc)
	if err != nil {
		t.Fatal(err)
	}
	// 09:30 in Tokyo is 00:30 UTC.
	if !s.Active(utc("2026-10-19T00:30:00Z")) || s.Active(utc("2026-10-19T09:30:00Z")) {
		t.Fatal("the schedule was not read in the probe's timezone")
	}
}

func TestQuotaValidation(t *testing.T) {
	cases := map[string]string{
		"quota: {requests: 0, per: day}":            "positive",
		"quota: {requests: 10}":                     "quota.per is missing",
		"quota: {requests: 10, per: decade}":        "hour, day, week or month",
		"quota: {requests: 10, per: day, burst: 2}": "field burst not found",
		// Three targets, two requests each: a run takes six.
		"quota: {requests: 5, per: day}\n    requests_per_probe: 2": "less than one run takes",
	}
	for field, want := range cases {
		err := resolveErr(t, `
probes:
  - name: a
    type: http
    `+field+`
    targets: [https://a.example/, https://b.example/, https://c.example/]
`)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", field, err, want)
		}
	}
}

func TestQuotaIsInherited(t *testing.T) {
	p := loadProbes(t, `
defaults:
  quota: {requests: 100, per: month}
probes:
  - name: a
    type: http
    targets: [https://a.example/]
  - name: b
    type: http
    quota: {requests: 5, per: day}
    targets: [https://b.example/]
`)
	list, err := p.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if q := list[0].Quota; q == nil || q.Requests != 100 || q.Per != PerMonth {
		t.Fatalf("a: quota %+v, want the default", q)
	}
	if q := list[1].Quota; q == nil || q.Requests != 5 || q.Per != PerDay {
		t.Fatalf("b: quota %+v, want its own", q)
	}
}

func TestPeriodWindow(t *testing.T) {
	msk, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	now := utc("2026-10-21T22:30:00Z") // Thursday 01:30 in Moscow
	cases := map[string][2]string{
		PerHour:  {"2026-10-22T01:00:00+03:00", "2026-10-22T02:00:00+03:00"},
		PerDay:   {"2026-10-22T00:00:00+03:00", "2026-10-23T00:00:00+03:00"},
		PerWeek:  {"2026-10-19T00:00:00+03:00", "2026-10-26T00:00:00+03:00"},
		PerMonth: {"2026-10-01T00:00:00+03:00", "2026-11-01T00:00:00+03:00"},
	}
	for per, want := range cases {
		start, end := PeriodWindow(per, now, msk)
		if !start.Equal(utc(want[0])) || !end.Equal(utc(want[1])) {
			t.Errorf("%s: [%s, %s), want [%s, %s)", per, start, end, want[0], want[1])
		}
	}
}

func TestScenarioTakesCronAndQuota(t *testing.T) {
	p, err := ParseScenario([]byte(`
name: upload
cron: "0 3 * * *"
timezone: UTC
timeout: 5m
quota: {requests: 30, per: day}
targets: [https://a.example/]
steps:
  - name: home
    get: /
`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Cron != "0 3 * * *" || p.Quota == nil || p.Quota.Requests != 30 || p.Timezone != "UTC" {
		t.Fatalf("scenario head lost a field: %+v", p)
	}
	_, err = ParseScenario([]byte(`
name: upload
cron: "0 3 * * *"
interval: 1h
timeout: 5m
targets: [https://a.example/]
steps:
  - name: home
    get: /
`))
	if err == nil || !strings.Contains(err.Error(), "alternatives") {
		t.Fatalf("err = %v, want interval and cron refused together", err)
	}
}
