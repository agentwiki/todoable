package local

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/agentwiki/todoable/internal/domain"
)

func (s *Store) initObservation() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS daemon_observation(singleton INTEGER PRIMARY KEY CHECK(singleton=1),state TEXT NOT NULL,observed_at INTEGER NOT NULL)`)
	return err
}

// RecordDaemonState records an observation, never a guarantee of current liveness.
func (s *Store) RecordDaemonState(state string) error {
	if state == "running" && s.storagePaused() {
		state = "storage_paused"
	}
	_, err := s.db.Exec(`INSERT INTO daemon_observation VALUES(1,?,?) ON CONFLICT(singleton) DO UPDATE SET state=excluded.state,observed_at=excluded.observed_at`, state, time.Now().UnixNano())
	return err
}

func (s *Store) TaskView(id string, version int) (map[string]any, error) {
	var current int
	var enabled bool
	err := s.db.QueryRow(`SELECT version,NOT EXISTS(SELECT 1 FROM disabled_tasks WHERE task_id=tasks.id) FROM tasks WHERE id=?`, id).Scan(&current, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &domain.Fault{Code: 4, Kind: "not_found", Message: "Task not found"}
	}
	if err != nil {
		return nil, err
	}
	if version == 0 {
		version = current
	}
	var raw string
	err = s.db.QueryRow(`SELECT definition FROM task_versions WHERE task_id=? AND version=?`, id, version).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &domain.Fault{Code: 4, Kind: "not_found", Message: "Task version not found"}
	}
	if err != nil {
		return nil, err
	}
	var definition map[string]any
	if err = json.Unmarshal([]byte(raw), &definition); err != nil {
		return nil, err
	}
	if env, ok := definition["env"].(map[string]any); ok {
		for key := range env {
			env[key] = "<redacted>"
		}
	}
	return map[string]any{"protocol_version": 1, "task_id": id, "task_version": version, "current_version": current, "enabled": enabled, "definition": definition}, nil
}

func (s *Store) Status(task string) (map[string]any, error) {
	if task != "" {
		if _, err := s.TaskView(task, 0); err != nil {
			return nil, err
		}
	}
	var state string
	var at int64
	err := s.db.QueryRow(`SELECT state,observed_at FROM daemon_observation WHERE singleton=1`).Scan(&state, &at)
	daemon := map[string]any{"state": "unknown", "observed_at": nil, "stale": true}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		daemon["state"] = state
		daemon["observed_at"] = time.Unix(0, at).UTC().Format(time.RFC3339Nano)
		daemon["stale"] = time.Since(time.Unix(0, at)) > 2*time.Second
	}
	rows, err := s.db.Query(`SELECT r.id FROM runs r JOIN submissions s ON s.id=r.submission_id WHERE s.state='active' AND (?='' OR s.task_id=?) ORDER BY s.seq,r.run_seq`, task, task)
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
	runs := []any{}
	for _, id := range ids {
		run, e := s.Run(id)
		if e != nil {
			return nil, e
		}
		runs = append(runs, run)
	}
	return map[string]any{"protocol_version": 1, "daemon": daemon, "runs": runs}, nil
}
