package local

import (
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/agentwiki/todoable/internal/domain"
)

func (s *Store) Update(task domain.Task, expected int) (int, error) {
	raw, e := json.Marshal(task)
	if e != nil {
		return 0, e
	}
	definition, e := domain.Canonical(raw)
	if e != nil {
		return 0, e
	}
	tx, e := s.db.Begin()
	if e != nil {
		return 0, e
	}
	defer func() { _ = tx.Rollback() }()
	var current int
	var previous string
	e = tx.QueryRow("SELECT version,definition FROM tasks WHERE id=?", task.ID).Scan(&current, &previous)
	if errors.Is(e, sql.ErrNoRows) {
		return 0, &domain.Fault{Code: 4, Kind: "not_found", Message: "Task not found"}
	}
	if e != nil {
		return 0, e
	}
	if current != expected {
		return 0, &domain.Fault{Code: 3, Kind: "version_conflict", Message: "expected Task version differs"}
	}
	if previous == string(definition) {
		return current, tx.Commit()
	}
	current++
	if _, e = tx.Exec("INSERT INTO task_versions VALUES(?,?,?)", task.ID, current, string(definition)); e != nil {
		return 0, e
	}
	if _, e = tx.Exec("UPDATE tasks SET version=?,definition=? WHERE id=?", current, string(definition), task.ID); e != nil {
		return 0, e
	}
	return current, tx.Commit()
}
func (s *Store) SetEnabled(id string, enabled bool) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if e = tx.QueryRow("SELECT count(*) FROM tasks WHERE id=?", id).Scan(&n); e != nil {
		return e
	}
	if n == 0 {
		return &domain.Fault{Code: 4, Kind: "not_found", Message: "Task not found"}
	}
	if enabled {
		_, e = tx.Exec("DELETE FROM disabled_tasks WHERE task_id=?", id)
	} else {
		_, e = tx.Exec("INSERT OR IGNORE INTO disabled_tasks VALUES(?)", id)
	}
	if e != nil {
		return e
	}
	return tx.Commit()
}
