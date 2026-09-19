package config

import (
	"reflect"
	"testing"
	"time"
)

func mustCompile(t *testing.T, s *Schedule) *Compiled {
	t.Helper()
	c, err := s.Compile()
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return c
}

// at builds a time in a known week; 2026-01-05 is a Monday.
func at(t *testing.T, day string, hour, minute int) time.Time {
	t.Helper()
	offset := map[string]int{"mon": 0, "tue": 1, "wed": 2, "thu": 3, "fri": 4, "sat": 5, "sun": 6}[day]
	return time.Date(2026, 1, 5+offset, hour, minute, 0, 0, time.UTC)
}

func TestScheduleActiveWindow(t *testing.T) {
	s := mustCompile(t, &Schedule{
		Timezone: "UTC",
		Windows:  []Window{{Days: []string{"mon", "tue"}, From: "09:00", To: "18:00"}},
	})

	for _, c := range []struct {
		day       string
		hour, min int
		want      bool
	}{
		{"mon", 9, 0, true},
		{"mon", 17, 59, true},
		{"mon", 18, 0, false},
		{"mon", 8, 59, false},
		{"wed", 12, 0, false},
	} {
		if got := s.Active(at(t, c.day, c.hour, c.min)); got != c.want {
			t.Errorf("%s %02d:%02d: active=%v, want %v", c.day, c.hour, c.min, got, c.want)
		}
	}
}

func TestScheduleWrapsPastMidnight(t *testing.T) {
	s := mustCompile(t, &Schedule{Timezone: "UTC", Windows: []Window{{From: "22:00", To: "06:00"}}})

	for _, c := range []struct {
		hour int
		want bool
	}{
		{23, true},
		{2, true},
		{12, false},
	} {
		if got := s.Active(at(t, "mon", c.hour, 0)); got != c.want {
			t.Errorf("%02d:00: active=%v, want %v", c.hour, got, c.want)
		}
	}
}

func TestScheduleHonoursTimezone(t *testing.T) {
	s := mustCompile(t, &Schedule{
		Timezone: "Europe/Moscow",
		Windows:  []Window{{From: "09:00", To: "18:00"}},
	})
	noonUTC := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC) // 15:00 in Moscow
	if !s.Active(noonUTC) {
		t.Error("15:00 Moscow was outside 09:00-18:00")
	}
	sixUTC := time.Date(2026, 1, 5, 6, 0, 0, 0, time.UTC) // 09:00 Moscow...
	if !s.Active(sixUTC) {
		t.Error("09:00 Moscow was outside its own window")
	}
	fiveUTC := time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC) // 08:00 Moscow
	if s.Active(fiveUTC) {
		t.Error("08:00 Moscow was inside 09:00-18:00")
	}
}

func TestNoScheduleAlwaysRuns(t *testing.T) {
	var s *Schedule
	c, err := s.Compile()
	if err != nil || c != nil {
		t.Fatalf("a nil schedule compiled to %v, %v", c, err)
	}
	if !c.Active(time.Now()) {
		t.Fatal("a probe without a schedule stopped running")
	}
}

func TestCompileDoesNotTouchTheParsedSchedule(t *testing.T) {
	s := &Schedule{
		Timezone: "UTC",
		Windows:  []Window{{Days: []string{"MONDAY"}, From: "09:00", To: "18:00"}},
	}
	before := *s
	if _, err := s.Compile(); err != nil {
		t.Fatal(err)
	}
	if s.Timezone != before.Timezone {
		t.Fatalf("Compile rewrote the parsed schedule: %+v", s)
	}
	if !reflect.DeepEqual(s.Windows, before.Windows) {
		t.Fatalf("Compile rewrote the parsed windows: %+v", s.Windows)
	}
}

func TestScheduleRejectsNonsense(t *testing.T) {
	cases := map[string]*Schedule{
		"no windows":   {},
		"bad day":      {Windows: []Window{{Days: []string{"caturday"}}}},
		"bad time":     {Windows: []Window{{From: "9am", To: "18:00"}}},
		"bad timezone": {Timezone: "Mars/Olympus", Windows: []Window{{From: "01:00", To: "02:00"}}},
	}
	for name, s := range cases {
		if _, err := s.Compile(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestWindowPastMidnightBelongsToTheDayItOpens(t *testing.T) {
	s := mustCompile(t, &Schedule{
		Timezone: "UTC",
		Windows:  []Window{{Days: []string{"fri"}, From: "22:00", To: "06:00"}},
	})
	if !s.Active(at(t, "fri", 23, 0)) {
		t.Error("fri 23:00 should be active")
	}
	if !s.Active(at(t, "sat", 2, 0)) {
		t.Error("sat 02:00 is the tail of Friday's window but is inactive")
	}
	if s.Active(at(t, "fri", 2, 0)) {
		t.Error("fri 02:00 belongs to Thursday night, yet is active")
	}
}

func TestEmptyWindowIsRejected(t *testing.T) {
	_, err := (&Schedule{Windows: []Window{{From: "00:00", To: "00:00"}}}).Compile()
	if err == nil {
		t.Fatal("a window with equal ends was accepted")
	}
}
