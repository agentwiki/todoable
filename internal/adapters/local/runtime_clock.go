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

// RemainingRunTime rechecks the continuously consumed budget at actual exec,
// after any delay since reservation. A stale reservation cannot start work.
func (s *Store) RemainingRunTime(stepID string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = chargeClocks(tx, s.runtimeNow()); err != nil {
		return 0, err
	}
	var remaining int64
	err = tx.QueryRow(`SELECT rt.remaining_ns FROM runtime rt JOIN steps st ON st.run_id=rt.run_id JOIN runs r ON r.id=rt.run_id WHERE st.id=? AND st.result IS NULL AND st.owner_version=rt.owner_version AND st.stage=rt.stage AND r.state='running'`, stepID).Scan(&remaining)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return remaining, tx.Commit()
}
