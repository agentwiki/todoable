package domain

import "time"

// ScheduleCursor is a value snapshot for the storage transaction. Observation
// does not accept work: a successful intake must clear Pending and persist
// LastAccepted together with its submission and first Run.
type ScheduleCursor struct {
	ObservedAt   time.Time
	Pending      *time.Time
	LastAccepted *time.Time
	Skipped      uint64
}

// Observe keeps only the latest unaccepted occurrence and never moves backward.
// anchor is the interval activation timestamp; it is ignored for cron.
func (r ScheduleRule) Observe(cursor ScheduleCursor, now, anchor time.Time) ScheduleCursor {
	if !now.After(cursor.ObservedAt) {
		return cursor
	}
	after := cursor.ObservedAt
	if cursor.LastAccepted != nil && cursor.LastAccepted.After(after) {
		after = *cursor.LastAccepted
	}
	next, ok := r.Next(after, anchor)
	if r.every > 0 && ok && !next.After(now) {
		elapsed := now.Sub(next)
		if next.Add(elapsed).Equal(now) {
			count := uint64(elapsed/r.every) + 1
			latest := next.Add(time.Duration(count-1) * r.every)
			if cursor.Pending != nil {
				cursor.Skipped++
			}
			cursor.Skipped += count - 1
			cursor.Pending = &latest
			cursor.ObservedAt = now.UTC()
			return cursor
		}
	}
	for ok && !next.After(now) {
		if cursor.LastAccepted == nil || next.After(*cursor.LastAccepted) {
			if cursor.Pending != nil {
				cursor.Skipped++
			}
			pending := next
			cursor.Pending = &pending
		}
		next, ok = r.Next(next, anchor)
	}
	cursor.ObservedAt = now.UTC()
	return cursor
}
