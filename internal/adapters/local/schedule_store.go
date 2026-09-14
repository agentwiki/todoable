package local

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/agentwiki/todoable/internal/domain"
)

// testControls is enabled only by the E2E binary's linker flag. Release binaries
// cannot replace wall time or stop at storage boundaries through environment.
var testControls = ""

func scheduleNow() (time.Time, error) {
	if testControls == "enabled" {
		if p := os.Getenv("TODOABLE_TEST_CLOCK"); p != "" {
			raw, e := os.ReadFile(p)
			if e != nil {
				return time.Time{}, e
			}
			return domain.ParseScheduledAt(string(raw))
		}
	}
	return time.Now().UTC(), nil
}
func scheduleBoundary(phase string) error {
	if testControls != "enabled" {
		return nil
	}
	root := os.Getenv("TODOABLE_TEST_SCHEDULE_BARRIER")
	if root == "" {
		return nil
	}
	if _, e := os.Stat(filepath.Join(root, phase)); e != nil {
		if errors.Is(e, os.ErrNotExist) {
			return nil
		}
		return e
	}
	if e := os.WriteFile(filepath.Join(root, phase+".reached"), nil, 0600); e != nil {
		return e
	}
	for {
		if _, e := os.Stat(filepath.Join(root, phase)); errors.Is(e, os.ErrNotExist) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func compileSchedule(s domain.Schedule, now time.Time) (domain.ScheduleRule, error) {
	if s.Every != "" {
		d, e := time.ParseDuration(s.Every)
		if e != nil || d > 8760*time.Hour {
			return domain.ScheduleRule{}, domain.Invalid("every exceeds 8760h")
		}
	}
	var loc *time.Location
	if s.Cron != "" {
		zone := s.Timezone
		if zone == "" {
			zone = "UTC"
		}
		var e error
		loc, e = time.LoadLocation(zone)
		if e != nil {
			return domain.ScheduleRule{}, domain.Invalid("invalid IANA timezone")
		}
	}
	return domain.CompileSchedule(s, now, loc)
}

type scheduleState struct {
	Enabled       bool                  `json:"enabled"`
	Anchor        time.Time             `json:"anchor"`
	Cursor        domain.ScheduleCursor `json:"cursor"`
	LastError     string                `json:"last_error"`
	Discarded     uint64                `json:"discarded"`
	DiscardReason string                `json:"discard_reason"`
}

func (s *Store) initSchedules() error {
	_, e := s.db.Exec(`CREATE TABLE IF NOT EXISTS schedules(task_id TEXT PRIMARY KEY REFERENCES tasks(id),state TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS schedule_periods(seq INTEGER PRIMARY KEY AUTOINCREMENT,task_id TEXT NOT NULL REFERENCES tasks(id),version INTEGER NOT NULL,start TEXT NOT NULL,end TEXT);
 CREATE INDEX IF NOT EXISTS submission_order ON submissions(task_id,input_key,state,seq);
 CREATE INDEX IF NOT EXISTS submission_state ON submissions(state);
 CREATE TABLE IF NOT EXISTS scheduled_submissions(submission_id TEXT PRIMARY KEY REFERENCES submissions(id),scheduled_at TEXT NOT NULL);`)
	return e
}
func loadSchedule(tx *sql.Tx, id string) (domain.Task, int, scheduleState, error) {
	var task domain.Task
	var version int
	var st scheduleState
	var raw string
	e := tx.QueryRow("SELECT definition,version FROM tasks WHERE id=?", id).Scan(&raw, &version)
	if errors.Is(e, sql.ErrNoRows) {
		return task, version, st, &domain.Fault{Code: 4, Kind: "not_found", Message: "Task not found"}
	}
	if e != nil {
		return task, version, st, e
	}
	if e = json.Unmarshal([]byte(raw), &task); e != nil {
		return task, version, st, e
	}
	e = tx.QueryRow("SELECT state FROM schedules WHERE task_id=?", id).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return task, version, st, nil
	}
	if e == nil {
		e = json.Unmarshal([]byte(raw), &st)
	}
	return task, version, st, e
}
func saveSchedule(tx *sql.Tx, id string, st scheduleState) error {
	b, e := json.Marshal(st)
	if e != nil {
		return e
	}
	_, e = tx.Exec("INSERT INTO schedules VALUES(?,?) ON CONFLICT(task_id) DO UPDATE SET state=excluded.state", id, string(b))
	return e
}
func history(tx *sql.Tx, id string, version int) ([]domain.ActivationPeriod, error) {
	rows, e := tx.Query("SELECT start,end FROM schedule_periods WHERE task_id=? AND version=? ORDER BY seq", id, version)
	if e != nil {
		return nil, e
	}
	defer func() { _ = rows.Close() }()
	out := []domain.ActivationPeriod{}
	for rows.Next() {
		var start string
		var end sql.NullString
		if e = rows.Scan(&start, &end); e != nil {
			return nil, e
		}
		a, e := domain.ParseScheduledAt(start)
		if e != nil {
			return nil, e
		}
		p := domain.ActivationPeriod{Start: a}
		if end.Valid {
			b, e := domain.ParseScheduledAt(end.String)
			if e != nil {
				return nil, e
			}
			p.End = &b
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func closeSchedule(tx *sql.Tx, id string, task domain.Task, st *scheduleState, now time.Time, reason string) error {
	if st.Enabled && task.Schedule != nil {
		r, e := compileSchedule(*task.Schedule, now)
		if e != nil {
			return e
		}
		st.Cursor = r.Observe(st.Cursor, now, st.Anchor)
	}
	if st.Cursor.Pending != nil {
		st.Discarded++
		st.Cursor.Pending = nil
	}
	st.DiscardReason = reason
	st.Enabled = false
	st.LastError = ""
	_, e := tx.Exec("UPDATE schedule_periods SET end=? WHERE task_id=? AND end IS NULL", now.Format(time.RFC3339Nano), id)
	return e
}
func (s *Store) SetSchedule(id string, enabled bool) error {
	now, e := scheduleNow()
	if e != nil {
		return e
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	task, version, st, e := loadSchedule(tx, id)
	if e != nil {
		return e
	}
	if task.Schedule == nil {
		return domain.Invalid("Task has no schedule")
	}
	if st.Enabled == enabled {
		return tx.Commit()
	}
	if enabled {
		var n int
		if e = tx.QueryRow("SELECT count(*) FROM disabled_tasks WHERE task_id=?", id).Scan(&n); e != nil {
			return e
		}
		if n > 0 {
			return &domain.Fault{Code: 6, Kind: "task_disabled", Message: "Task is disabled"}
		}
		st.Enabled = true
		st.Anchor = now
		st.Cursor.ObservedAt = now
		st.Cursor.Pending = nil
		st.LastError = ""
		_, e = tx.Exec("INSERT INTO schedule_periods(task_id,version,start) VALUES(?,?,?)", id, version, now.Format(time.RFC3339Nano))
	} else {
		e = closeSchedule(tx, id, task, &st, now, "disabled")
	}
	if e != nil {
		return e
	}
	if e = saveSchedule(tx, id, st); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) Schedule(id string) (map[string]any, error) {
	tx, e := s.db.Begin()
	if e != nil {
		return nil, e
	}
	defer func() { _ = tx.Rollback() }()
	task, version, st, e := loadSchedule(tx, id)
	if e != nil {
		return nil, e
	}
	h, e := history(tx, id, version)
	if e != nil {
		return nil, e
	}
	out := map[string]any{"protocol_version": 1, "task_id": id, "task_version": version, "schedule": task.Schedule, "enabled": st.Enabled, "anchor": st.Anchor, "observed_at": st.Cursor.ObservedAt, "latest_unaccepted_at": st.Cursor.Pending, "last_accepted_at": st.Cursor.LastAccepted, "skipped": st.Cursor.Skipped, "discarded": st.Discarded, "discard_reason": st.DiscardReason, "last_error": st.LastError, "activation_history": h}
	return out, tx.Commit()
}
func scheduleInput(tx *sql.Tx, id string, version int, at time.Time) (domain.SubmissionInput, error) {
	var in domain.SubmissionInput
	var raw string
	e := tx.QueryRow("SELECT definition FROM task_versions WHERE task_id=? AND version=?", id, version).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return in, &domain.Fault{Code: 4, Kind: "not_found", Message: "Task version not found"}
	}
	if e != nil {
		return in, e
	}
	var task domain.Task
	if e = json.Unmarshal([]byte(raw), &task); e != nil {
		return in, e
	}
	if task.Schedule == nil {
		return in, domain.Invalid("Task version has no schedule")
	}
	r, e := compileSchedule(*task.Schedule, at)
	if e != nil {
		return in, e
	}
	h, e := history(tx, id, version)
	if e != nil {
		return in, e
	}
	window, e := r.Window(at, h)
	if e != nil {
		return in, e
	}
	body, e := json.Marshal(map[string]any{"data": task.Schedule.Input, "occurrence": window})
	if e != nil {
		return in, e
	}
	body, e = domain.Canonical(body)
	if e != nil {
		return in, e
	}
	return domain.SubmissionInput{TaskID: id, TaskVersion: &version, InputKey: task.Schedule.InputKey, ConcurrencyKey: task.Schedule.ConcurrencyKey, Input: body}, nil
}
func (s *Store) SubmitSchedule(id string, at time.Time, version int) (domain.Result, error) {
	var out domain.Result
	tx, e := s.db.Begin()
	if e != nil {
		return out, e
	}
	defer func() { _ = tx.Rollback() }()
	if version == 0 {
		if e = tx.QueryRow("SELECT version FROM tasks WHERE id=?", id).Scan(&version); errors.Is(e, sql.ErrNoRows) {
			return out, &domain.Fault{Code: 4, Kind: "not_found", Message: "Task not found"}
		}
		if e != nil {
			return out, e
		}
	}
	in, e := scheduleInput(tx, id, version, at)
	if e != nil {
		return out, e
	}
	out, e = submitTx(tx, in, s.config)
	if e != nil {
		return out, e
	}
	if _, e = tx.Exec("INSERT OR IGNORE INTO scheduled_submissions VALUES(?,?)", out.SubmissionID, at.Format(time.RFC3339Nano)); e != nil {
		return out, e
	}
	return out, tx.Commit()
}
func (s *Store) PublishSchedules() error {
	now, e := scheduleNow()
	if e != nil {
		return e
	}
	rows, e := s.db.Query("SELECT task_id FROM schedules ORDER BY task_id")
	if e != nil {
		return e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			_ = rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	_ = rows.Close()
	if e != nil {
		return e
	}
	for _, id := range ids {
		if e = s.publishSchedule(id, now); e != nil {
			return e
		}
	}
	return nil
}
func (s *Store) publishSchedule(id string, now time.Time) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	task, version, st, e := loadSchedule(tx, id)
	if e != nil {
		return e
	}
	if !st.Enabled || task.Schedule == nil || now.Before(st.Cursor.ObservedAt) {
		return tx.Commit()
	}
	r, e := compileSchedule(*task.Schedule, now)
	if e != nil {
		return e
	}
	st.Cursor = r.Observe(st.Cursor, now, st.Anchor)
	if st.Cursor.Pending != nil {
		var active int
		if e = tx.QueryRow("SELECT count(*) FROM submissions WHERE task_id=? AND input_key=? AND state='active'", id, task.Schedule.InputKey).Scan(&active); e != nil {
			return e
		}
		if active == 0 {
			at := *st.Cursor.Pending
			var in domain.SubmissionInput
			in, e = scheduleInput(tx, id, version, at)
			if e == nil {
				_, e = tx.Exec("SAVEPOINT scheduled_intake")
			}
			var out domain.Result
			if e == nil {
				out, e = submitTx(tx, in, s.config)
				if e != nil {
					if _, rollbackErr := tx.Exec("ROLLBACK TO scheduled_intake"); rollbackErr != nil {
						return rollbackErr
					}
				}
			}
			if e != nil {
				st.LastError = e.Error()
			} else {
				if _, e = tx.Exec("INSERT OR IGNORE INTO scheduled_submissions VALUES(?,?)", out.SubmissionID, at.Format(time.RFC3339Nano)); e != nil {
					return e
				}
				st.Cursor.LastAccepted = &at
				st.Cursor.Pending = nil
				st.LastError = ""
			}
		}
	}
	if e = saveSchedule(tx, id, st); e != nil {
		return e
	}
	if e = scheduleBoundary("before_commit"); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	return scheduleBoundary("after_commit")
}
func updateSchedule(tx *sql.Tx, task domain.Task, version int) error {
	now, e := scheduleNow()
	if e != nil {
		return e
	}
	old, _, st, e := loadSchedule(tx, task.ID)
	if e != nil {
		return e
	}
	before, e := json.Marshal(old.Schedule)
	if e != nil {
		return e
	}
	after, e := json.Marshal(task.Schedule)
	if e != nil {
		return e
	}
	if string(before) == string(after) {
		// A new immutable version inherits the same interval grid; previous versions
		// retain their closed history and can still reconstruct old opportunities.
		if st.Enabled {
			if _, e = tx.Exec("UPDATE schedule_periods SET end=? WHERE task_id=? AND version=? AND end IS NULL", now.Format(time.RFC3339Nano), task.ID, version); e != nil {
				return e
			}
			_, e = tx.Exec("INSERT INTO schedule_periods(task_id,version,start) VALUES(?,?,?)", task.ID, version+1, st.Anchor.Format(time.RFC3339Nano))
			return e
		}
		return nil
	}
	active := st.Enabled
	if e = closeSchedule(tx, task.ID, old, &st, now, "schedule_changed"); e != nil {
		return e
	}
	if active && task.Schedule != nil {
		st.Enabled = true
		st.Anchor = now
		st.Cursor.ObservedAt = now
		if _, e = tx.Exec("INSERT INTO schedule_periods(task_id,version,start) VALUES(?,?,?)", task.ID, version+1, now.Format(time.RFC3339Nano)); e != nil {
			return e
		}
	}
	return saveSchedule(tx, task.ID, st)
}
