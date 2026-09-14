package local

import (
	"database/sql"
	"errors"

	"github.com/agentwiki/todoable/internal/domain"
)

func (s *Store) CancelTask(request domain.TaskCancelRequest) (map[string]any, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var version int
	if err = tx.QueryRow(`SELECT version FROM tasks WHERE id=?`, request.TaskID).Scan(&version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &domain.Fault{Code: 4, Kind: "not_found", Message: "Task not found"}
		}
		return nil, err
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO disabled_tasks VALUES(?)`, request.TaskID); err != nil {
		return nil, err
	}
	now, err := scheduleNow()
	if err != nil {
		return nil, err
	}
	task, _, st, err := loadSchedule(tx, request.TaskID)
	if err != nil {
		return nil, err
	}
	if err = closeSchedule(tx, request.TaskID, task, &st, now, "task_cancelled"); err != nil {
		return nil, err
	}
	if err = saveSchedule(tx, request.TaskID, st); err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT id FROM submissions WHERE task_id=? AND state='active' ORDER BY seq`, request.TaskID)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err = tx.Exec(`INSERT INTO cancel_requests(submission_id,reason,acknowledge_effects,processes_stopped) VALUES(?,?,0,0) ON CONFLICT(submission_id) DO UPDATE SET reason=excluded.reason`, id, request.Reason); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(`INSERT INTO cancellation_audit(submission_id,reason,acknowledge_effects,processes_stopped,created_at) VALUES(?,?,0,0,?)`, id, request.Reason, s.runtimeNow()); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(`UPDATE repeat_budgets SET remaining=0 WHERE submission_id=?`, id); err != nil {
			return nil, err
		}
	}
	if err = chargeClocks(tx, s.runtimeNow()); err != nil {
		return nil, err
	}
	if err = settleCancellations(tx); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"protocol_version": 1, "task_id": request.TaskID, "task_version": version, "enabled": false, "cancel_requested": true, "submission_ids": ids}, nil
}
