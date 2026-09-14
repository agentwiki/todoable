package local

import (
	"database/sql"
	"time"
)

// The store anchor retains Go's monotonic component. Persisted timestamps allow
// conservative charging across a daemon outage without resetting the budget.
func (s *Store) runtimeNow() int64 { return s.opened.UnixNano() + time.Since(s.opened).Nanoseconds() }
func chargeClocks(tx *sql.Tx, now int64) error {
	if _, err := tx.Exec(`UPDATE runtime SET remaining_ns=max(0,remaining_ns-max(0,?-coalesce((SELECT ticking_at FROM run_clocks WHERE run_id=runtime.run_id),?))) WHERE run_id IN(SELECT run_id FROM run_clocks WHERE ticking_at>0)`, now, now); err != nil {
		return err
	}
	_, err := tx.Exec("UPDATE run_clocks SET ticking_at=max(ticking_at,?) WHERE ticking_at>0", now)
	return err
}
func startClock(tx *sql.Tx, id string, now int64) error {
	_, err := tx.Exec("INSERT INTO run_clocks(run_id,ticking_at) VALUES(?,?) ON CONFLICT(run_id) DO UPDATE SET ticking_at=CASE WHEN ticking_at=0 THEN excluded.ticking_at ELSE ticking_at END", id, now)
	return err
}
func stopClock(tx *sql.Tx, id string) error {
	_, err := tx.Exec("UPDATE run_clocks SET ticking_at=0 WHERE run_id=?", id)
	return err
}
