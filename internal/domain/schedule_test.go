package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func scheduleTime(raw string) time.Time {
	t, e := time.Parse(time.RFC3339Nano, raw)
	if e != nil {
		panic(e)
	}
	return t
}
func scheduleRule(t *testing.T, every, cron, zone string) ScheduleRule {
	t.Helper()
	loc, e := time.LoadLocation(zone)
	if e != nil {
		t.Fatal(e)
	}
	r, e := CompileSchedule(Schedule{Every: every, Cron: cron, Timezone: func() string {
		if every != "" {
			return ""
		}
		return zone
	}(), InputKey: "daily", ConcurrencyKey: "report", Input: json.RawMessage(`{"report":"daily"}`)}, scheduleTime("2026-01-01T00:00:00Z"), loc)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func TestScheduleIntervalWindowAndCursor(t *testing.T) {
	r := scheduleRule(t, "10s", "", "UTC")
	anchor := scheduleTime("2026-01-01T00:00:00.25Z")
	next, ok := r.Next(anchor, anchor)
	if !ok || next.Format(time.RFC3339Nano) != "2026-01-01T00:00:10.25Z" {
		t.Fatalf("first %v %v", next, ok)
	}
	cursor := r.Observe(ScheduleCursor{ObservedAt: anchor}, anchor.Add(35*time.Second), anchor)
	if cursor.Pending == nil || cursor.Pending.Format(time.RFC3339Nano) != "2026-01-01T00:00:30.25Z" || cursor.Skipped != 2 {
		t.Fatalf("cursor %+v", cursor)
	}
	cursor = r.Observe(cursor, anchor.Add(15*time.Second), anchor)
	if !cursor.ObservedAt.Equal(anchor.Add(35*time.Second)) || cursor.Skipped != 2 {
		t.Fatalf("clock regression %+v", cursor)
	}
	cursor = r.Observe(cursor, anchor.Add(41*time.Second), anchor)
	if cursor.Skipped != 3 {
		t.Fatalf("replacement counted %d", cursor.Skipped)
	}
	end := anchor.Add(time.Minute)
	occurrence, e := r.Window(*cursor.Pending, []ActivationPeriod{{Start: anchor, End: &end}})
	if e != nil || occurrence.WindowStart != "2026-01-01T00:00:30.25Z" || occurrence.WindowEnd != "2026-01-01T00:00:40.25Z" {
		t.Fatalf("window %+v %v", occurrence, e)
	}
	for _, at := range []time.Time{anchor, anchor.Add(time.Second), end} {
		if _, e = r.Window(at, []ActivationPeriod{{Start: anchor, End: &end}}); e == nil {
			t.Fatalf("accepted %s", at)
		}
	}
	if _, e = r.Window(next, nil); e == nil {
		t.Fatal("accepted missing history")
	}
}
func TestScheduleCronDSTAndOR(t *testing.T) {
	tests := []struct{ cron, after, want string }{
		{"30 2 * * *", "2026-03-08T06:00:00Z", "2026-03-09T06:30:00Z"},
		{"30 1 * * *", "2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z"},
		{"0 0 31 2 0", "2026-02-01T05:00:00Z", "2026-02-08T05:00:00Z"},
	}
	for _, tt := range tests {
		r := scheduleRule(t, "", tt.cron, "America/New_York")
		got, ok := r.Next(scheduleTime(tt.after), time.Time{})
		if !ok || got.Format(time.RFC3339) != tt.want {
			t.Fatalf("%s got %v want %s", tt.cron, got, tt.want)
		}
	}
	r := scheduleRule(t, "", "30 1 * * *", "America/New_York")
	window, e := r.Window(scheduleTime("2026-11-01T06:30:00Z"), nil)
	if e != nil || window.WindowStart != "2026-11-01T05:30:00Z" {
		t.Fatalf("fall window %+v %v", window, e)
	}
	if _, e = r.Window(scheduleTime("2026-11-01T06:30:00.001Z"), nil); e == nil {
		t.Fatal("accepted subminute")
	}
}
func TestScheduleDefinitionValidation(t *testing.T) {
	base := Schedule{Cron: "0 0 * * *", InputKey: "i", ConcurrencyKey: "c", Input: json.RawMessage(`{}`)}
	now := scheduleTime("2026-01-01T00:00:00Z")
	for _, cron := range []string{"0 0 31 2 *", "* * * * * *", "@daily", "0 0 * JAN *", "0 0 * * 7", "*/0 * * * *", "-1 * * * *", "60 * * * *", "0 0 L * *", "0 0 * * ?", "0 0 * * 1#2"} {
		s := base
		s.Cron = cron
		if _, e := CompileSchedule(s, now, time.UTC); e == nil {
			t.Errorf("accepted %q", cron)
		}
	}
	for _, cron := range []string{"*/15 0-23/2 1,15 * 0-6", "0 0 29 2 *", "0 0 31 2 0"} {
		s := base
		s.Cron = cron
		if _, e := CompileSchedule(s, now, time.UTC); e != nil {
			t.Errorf("rejected %q: %v", cron, e)
		}
	}
	for _, every := range []string{"0s", "999ms", "-1s", "nope"} {
		s := base
		s.Cron = ""
		s.Every = every
		if _, e := CompileSchedule(s, now, time.UTC); e == nil {
			t.Errorf("accepted %q", every)
		}
	}
	for _, raw := range []string{"2026-01-01T00:00:00", "2026-01-01T00:00:60Z", "2026-01-01T00:00:00+24:00", "2026-01-01T00:00:00,123Z", "2026-01-01T00:00:00.0000000001Z"} {
		if _, e := ParseScheduledAt(raw); e == nil {
			t.Errorf("accepted time %s", raw)
		}
	}
	got, e := ParseScheduledAt("2026-01-01T09:00:00+09:00")
	if e != nil || got.Format(time.RFC3339) != "2026-01-01T00:00:00Z" {
		t.Fatalf("normalize %v %v", got, e)
	}
}
func FuzzScheduleCronField(f *testing.F) {
	for _, s := range []string{"*", "*/15", "0,30", "1-5/2", "-1", "*/0", "99999999999999999999999", "1//2"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		field, e := parseCronField(s, [2]int{0, 59})
		if e == nil && (field.bits == 0 || field.bits>>60 != 0) {
			t.Fatalf("invalid successful field %q %+v", s, field)
		}
	})
}

func TestScheduleCronWindowBeforeActivationAndLeap(t *testing.T) {
	r := scheduleRule(t, "", "0 0 * * 1", "UTC")
	next, ok := r.Next(scheduleTime("2026-01-01T00:00:00Z"), time.Time{})
	if !ok || next.Format(time.RFC3339) != "2026-01-05T00:00:00Z" {
		t.Fatalf("weekday restriction %v", next)
	}
	window, e := r.Window(next, nil)
	if e != nil || window.WindowStart != "2025-12-29T00:00:00Z" {
		t.Fatalf("preceding window %+v %v", window, e)
	}
	leap := scheduleRule(t, "", "0 0 29 2 *", "UTC")
	next, ok = leap.Next(scheduleTime("2096-02-29T00:00:00Z"), time.Time{})
	if !ok || next.Format(time.RFC3339) != "2104-02-29T00:00:00Z" {
		t.Fatalf("eight-year century boundary %v %v", next, ok)
	}
}

func TestScheduleObservedAcceptanceAndNoAliasing(t *testing.T) {
	r := scheduleRule(t, "1s", "", "UTC")
	anchor := scheduleTime("2026-01-01T00:00:00Z")
	pending := anchor.Add(time.Second)
	before := ScheduleCursor{ObservedAt: pending, Pending: &pending}
	after := r.Observe(before, anchor.Add(365*24*time.Hour), anchor)
	if after.Skipped != 31535999 || after.Pending == nil || after.Pending.Format(time.RFC3339) != "2027-01-01T00:00:00Z" {
		t.Fatalf("long gap %+v", after)
	}
	if !before.Pending.Equal(pending) || before.Skipped != 0 {
		t.Fatal("modified source cursor")
	}
	accepted := anchor.Add(10 * time.Second)
	cursor := r.Observe(ScheduleCursor{ObservedAt: anchor, LastAccepted: &accepted}, anchor.Add(12*time.Second), anchor)
	if cursor.Skipped != 1 || cursor.Pending == nil || cursor.Pending.Format(time.RFC3339) != "2026-01-01T00:00:12Z" {
		t.Fatalf("accepted periods counted %+v", cursor)
	}
}

func TestScheduleHistoricalSecondOffset(t *testing.T) {
	r := scheduleRule(t, "", "0 0 * * *", "Africa/Monrovia")
	for _, tt := range []struct{ after, want string }{
		{"1972-01-05T00:44:30Z", "1972-01-06T00:44:30Z"},
		{"1972-01-06T00:44:29.999999999Z", "1972-01-06T00:44:30Z"},
		// The shift from -00:44:30 to UTC skips January 7's local midnight.
		{"1972-01-06T00:44:30Z", "1972-01-08T00:00:00Z"},
		{"1972-01-07T00:44:29Z", "1972-01-08T00:00:00Z"},
	} {
		got, ok := r.Next(scheduleTime(tt.after), time.Time{})
		if !ok || got.Format(time.RFC3339Nano) != tt.want {
			t.Fatalf("next after %s: %s %v; want %s", tt.after, got, ok, tt.want)
		}
	}
	for _, tt := range []struct{ at, start string }{
		{"1972-01-06T00:44:30Z", "1972-01-05T00:44:30Z"},
		{"1972-01-08T00:00:00Z", "1972-01-06T00:44:30Z"},
	} {
		got, e := r.Window(scheduleTime(tt.at), nil)
		if e != nil || got.WindowStart != tt.start || got.WindowEnd != tt.at {
			t.Fatalf("window at %s: %+v %v", tt.at, got, e)
		}
	}
	minute := scheduleRule(t, "", "* * * * *", "Africa/Monrovia")
	got, ok := minute.Next(scheduleTime("1972-01-07T00:44:30Z"), time.Time{})
	if !ok || got.Format(time.RFC3339) != "1972-01-07T00:45:00Z" {
		t.Fatalf("new grid %v %v", got, ok)
	}
	window, e := minute.Window(scheduleTime("1972-01-07T00:45:00Z"), nil)
	if e != nil || window.WindowStart != "1972-01-07T00:43:30Z" {
		t.Fatalf("previous grid %+v %v", window, e)
	}
}
