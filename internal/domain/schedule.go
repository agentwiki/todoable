package domain

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Schedule is the versioned task definition. Timezone resolution belongs to an
// adapter; calculations receive the resolved location rather than read the host.
type Schedule struct {
	Every          string          `json:"every,omitempty"`
	Cron           string          `json:"cron,omitempty"`
	Timezone       string          `json:"timezone,omitempty"`
	InputKey       string          `json:"input_key"`
	ConcurrencyKey string          `json:"concurrency_key"`
	Input          json.RawMessage `json:"input"`
}

type ScheduleRule struct {
	every    time.Duration
	fields   [5]cronField
	location *time.Location
}
type cronField struct {
	bits     uint64
	wildcard bool
}

// CompileSchedule validates a definition at an explicitly supplied instant.
// For cron, location must be an adapter-resolved IANA zone matching Timezone.
func CompileSchedule(s Schedule, now time.Time, location *time.Location) (ScheduleRule, error) {
	var r ScheduleRule
	if (s.Every == "") == (s.Cron == "") {
		return r, Invalid("schedule requires exactly one of every or cron")
	}
	if !ValidKey(s.InputKey) || !ValidKey(s.ConcurrencyKey) {
		return r, Invalid("invalid schedule keys")
	}
	raw, err := Canonical(s.Input)
	if err != nil || len(raw) == 0 || raw[0] != '{' {
		return r, Invalid("schedule input must be a JSON object")
	}
	if s.Every != "" {
		r.every, err = time.ParseDuration(s.Every)
		if err != nil || r.every < time.Second || s.Timezone != "" {
			return ScheduleRule{}, Invalid("every must be at least one second and cannot have timezone")
		}
		return r, nil
	}
	zone := s.Timezone
	if zone == "" {
		zone = "UTC"
	}
	if location == nil || location.String() != zone || zone == "Local" {
		return r, Invalid("schedule requires a resolved IANA timezone")
	}
	r.location = location
	parts := strings.Fields(s.Cron)
	if len(parts) != 5 {
		return r, Invalid("cron requires five numeric fields")
	}
	bounds := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	for i, p := range parts {
		r.fields[i], err = parseCronField(p, bounds[i])
		if err != nil {
			return ScheduleRule{}, err
		}
	}
	if _, ok := r.Next(now, time.Time{}); !ok {
		return ScheduleRule{}, Invalid("cron has no occurrence within eight years")
	}
	return r, nil
}
func cronNumber(s string) (int, error) {
	if s == "" {
		return 0, Invalid("empty cron number")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, Invalid("cron accepts numbers only")
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, Invalid("invalid cron number")
	}
	return n, nil
}
func parseCronField(s string, bounds [2]int) (cronField, error) {
	f := cronField{wildcard: s == "*"}
	for _, item := range strings.Split(s, ",") {
		parts := strings.Split(item, "/")
		if len(parts) > 2 {
			return f, Invalid("invalid cron step")
		}
		step := 1
		if len(parts) == 2 {
			var err error
			step, err = cronNumber(parts[1])
			if err != nil || step < 1 {
				return f, Invalid("invalid cron step")
			}
		}
		lo, hi := bounds[0], bounds[1]
		if parts[0] != "*" {
			span := strings.Split(parts[0], "-")
			if len(span) > 2 {
				return f, Invalid("invalid cron range")
			}
			var err error
			lo, err = cronNumber(span[0])
			if err != nil {
				return f, err
			}
			hi = lo
			if len(span) == 2 {
				hi, err = cronNumber(span[1])
				if err != nil {
					return f, err
				}
			} else if len(parts) == 2 {
				hi = bounds[1]
			}
		}
		if lo < bounds[0] || hi > bounds[1] || hi < lo {
			return f, Invalid("cron field out of range")
		}
		for n := lo; n <= hi; {
			f.bits |= uint64(1) << n
			if step > hi-n {
				break
			}
			n += step
		}
	}
	return f, nil
}
func (f cronField) matches(n int) bool { return f.bits&(uint64(1)<<n) != 0 }
func (r ScheduleRule) cronMatches(at time.Time) bool {
	t := at.In(r.location)
	if t.Second() != 0 || t.Nanosecond() != 0 || !r.fields[0].matches(t.Minute()) || !r.fields[1].matches(t.Hour()) || !r.fields[3].matches(int(t.Month())) {
		return false
	}
	day, week := r.fields[2].matches(t.Day()), r.fields[4].matches(int(t.Weekday()))
	if r.fields[2].wildcard {
		return week
	}
	if r.fields[4].wildcard {
		return day
	}
	return day || week
}

// Next returns the first opportunity strictly after after. Interval opportunities
// start one period after anchor. Cron searches at most eight calendar years.
func (r ScheduleRule) Next(after, anchor time.Time) (time.Time, bool) {
	if r.every > 0 {
		if after.Before(anchor) {
			return anchor.Add(r.every).UTC(), true
		}
		// Sub saturates beyond duration's range; reject rather than wrap silently.
		elapsed := after.Sub(anchor)
		if !anchor.Add(elapsed).Equal(after) {
			return time.Time{}, false
		}
		next := after.Add(r.every - elapsed%r.every).UTC()
		return next, next.After(after) && next.Year() <= 9999
	}
	if r.location == nil {
		return time.Time{}, false
	}
	limit := after.AddDate(8, 0, 0)
	for cursor := after.UTC().Add(time.Nanosecond); !cursor.After(limit); {
		local := cursor.In(r.location)
		_, zoneEnd := local.ZoneBounds()
		candidate := localMinuteFloor(local).UTC()
		if candidate.Before(cursor) {
			candidate = candidate.Add(time.Minute)
		}
		// A zone change can shift the local minute grid by seconds. Do not
		// carry the preceding zone's grid across that transition.
		if !zoneEnd.IsZero() && !candidate.Before(zoneEnd) {
			cursor = zoneEnd.UTC()
			continue
		}
		if candidate.After(limit) {
			break
		}
		if r.cronMatches(candidate) {
			return candidate, true
		}
		cursor = candidate.Add(time.Minute)
		if !zoneEnd.IsZero() && !cursor.Before(zoneEnd) {
			cursor = zoneEnd.UTC()
		}
	}
	return time.Time{}, false
}

// Previous returns the immediately preceding opportunity. For the first interval
// opportunity it returns the activation anchor, which is the window boundary.
func (r ScheduleRule) Previous(at, anchor time.Time) (time.Time, bool) {
	if r.every > 0 {
		delta := at.Sub(anchor)
		if delta < r.every || !anchor.Add(delta).Equal(at) || delta%r.every != 0 {
			return time.Time{}, false
		}
		return at.Add(-r.every).UTC(), true
	}
	if r.location == nil || !r.cronMatches(at) {
		return time.Time{}, false
	}
	limit := at.AddDate(-8, 0, 0)
	for cursor := at.UTC().Add(-time.Nanosecond); !cursor.Before(limit); {
		local := cursor.In(r.location)
		zoneStart, _ := local.ZoneBounds()
		candidate := localMinuteFloor(local).UTC()
		if !zoneStart.IsZero() && candidate.Before(zoneStart) {
			cursor = zoneStart.UTC().Add(-time.Nanosecond)
			continue
		}
		if candidate.Before(limit) {
			break
		}
		if r.cronMatches(candidate) {
			return candidate, true
		}
		cursor = candidate.Add(-time.Minute)
		if !zoneStart.IsZero() && cursor.Before(zoneStart) {
			cursor = zoneStart.UTC().Add(-time.Nanosecond)
		}
	}
	return time.Time{}, false
}

// Subtract the local seconds rather than truncating UTC: historical IANA
// offsets need not be multiples of a minute (for example Monrovia -00:44:30).
func localMinuteFloor(local time.Time) time.Time {
	return local.Add(-time.Duration(local.Second())*time.Second - time.Duration(local.Nanosecond()))
}

type ActivationPeriod struct {
	Start time.Time
	End   *time.Time
}
type Occurrence struct {
	ScheduledAt string `json:"scheduled_at"`
	WindowStart string `json:"window_start"`
	WindowEnd   string `json:"window_end"`
}

// Window checks manual submission against the selected version's activation
// history. Closed intervals are [Start, End); the anchor itself is not scheduled.
// Cron can reconstruct opportunities before the first activation.
func (r ScheduleRule) Window(at time.Time, history []ActivationPeriod) (Occurrence, error) {
	anchor := time.Time{}
	if r.every > 0 {
		found := false
		for _, p := range history {
			if at.After(p.Start) && (p.End == nil || at.Before(*p.End)) {
				anchor = p.Start
				found = true
				break
			}
		}
		if !found {
			return Occurrence{}, Invalid("interval occurrence is outside activation history")
		}
	}
	prev, ok := r.Previous(at, anchor)
	if !ok {
		return Occurrence{}, Invalid("time is not an exact scheduled occurrence")
	}
	stamp := at.UTC().Format(time.RFC3339Nano)
	return Occurrence{stamp, prev.UTC().Format(time.RFC3339Nano), stamp}, nil
}

var scheduledTimestamp = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

func ParseScheduledAt(raw string) (time.Time, error) {
	if !scheduledTimestamp.MatchString(raw) {
		return time.Time{}, Invalid("scheduled time must be RFC3339 with timezone and at most nanosecond precision")
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, Invalid("scheduled time must be RFC3339 with timezone and without leap seconds")
	}
	return t.UTC(), nil
}
