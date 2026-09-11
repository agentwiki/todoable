package local

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/agentwiki/todoable/internal/domain"
)

func (s *Store) initRuntime() error {
	_, e := s.db.Exec(`CREATE TABLE IF NOT EXISTS runtime(run_id TEXT PRIMARY KEY REFERENCES runs(id),stage TEXT NOT NULL,phase TEXT NOT NULL DEFAULT '',last_check TEXT,remaining_ns INTEGER NOT NULL,next_at INTEGER NOT NULL DEFAULT 0,owner_version INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS resources(key TEXT PRIMARY KEY,run_id TEXT NOT NULL UNIQUE REFERENCES runs(id));
 CREATE TABLE IF NOT EXISTS steps(seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT NOT NULL UNIQUE,run_id TEXT NOT NULL REFERENCES runs(id),stage TEXT NOT NULL,call_index INTEGER NOT NULL,owner_version INTEGER NOT NULL,result TEXT,pid INTEGER,pgid INTEGER,boot_id TEXT,process_start TEXT);
 CREATE TABLE IF NOT EXISTS ready_queue(seq INTEGER PRIMARY KEY AUTOINCREMENT,run_id TEXT NOT NULL UNIQUE REFERENCES runs(id));`)
	return e
}
func (s *Store) Reserve() (*domain.Execution, error) {
	tx, e := s.db.Begin()
	if e != nil {
		return nil, e
	}
	defer func() { _ = tx.Rollback() }()
	rows, e := tx.Query(`SELECT r.id,s.snapshot FROM runs r JOIN submissions s ON s.id=r.submission_id LEFT JOIN runtime rt ON rt.run_id=r.id WHERE rt.run_id IS NULL AND r.state='waiting'`)
	if e != nil {
		return nil, e
	}
	type initRun struct{ id, snapshot string }
	var pending []initRun
	for rows.Next() {
		var p initRun
		if e = rows.Scan(&p.id, &p.snapshot); e != nil {
			_ = rows.Close()
			return nil, e
		}
		pending = append(pending, p)
	}
	e = rows.Err()
	_ = rows.Close()
	if e != nil {
		return nil, e
	}
	for _, p := range pending {
		var task domain.Task
		if e = json.Unmarshal([]byte(p.snapshot), &task); e != nil {
			return nil, e
		}
		budget, _ := time.ParseDuration(task.Limits["run_timeout"])
		stage := "ready"
		phase := ""
		if task.Start != nil {
			stage = "start_check"
			phase = "preflight"
		}
		if _, e = tx.Exec("INSERT INTO runtime(run_id,stage,phase,remaining_ns) VALUES(?,?,?,?)", p.id, stage, phase, int64(budget)); e != nil {
			return nil, e
		}
		if stage == "ready" {
			if _, e = tx.Exec("INSERT INTO ready_queue(run_id) VALUES(?)", p.id); e != nil {
				return nil, e
			}
		}
	}
	var x domain.Execution
	var snap, last, input string
	var remaining int64
	e = tx.QueryRow(`SELECT r.id,r.submission_id,s.task_id,s.task_version,s.input_key,s.input,r.run_seq,r.calls_used,s.snapshot,rt.stage,rt.phase,coalesce(rt.last_check,'null'),rt.remaining_ns FROM runs r JOIN submissions s ON s.id=r.submission_id JOIN runtime rt ON rt.run_id=r.id LEFT JOIN ready_queue q ON q.run_id=r.id WHERE r.state IN ('waiting','running') AND rt.next_at<=? AND NOT EXISTS(SELECT 1 FROM steps st WHERE st.run_id=r.id AND st.result IS NULL) AND NOT EXISTS(SELECT 1 FROM submissions older WHERE older.task_id=s.task_id AND older.input_key=s.input_key AND older.seq<s.seq AND older.state='active') AND (rt.stage!='ready' OR NOT EXISTS(SELECT 1 FROM resources WHERE key=s.concurrency_key)) ORDER BY CASE WHEN r.state='running' THEN 0 ELSE 1 END,coalesce(q.seq,s.seq) LIMIT 1`, time.Now().UnixNano()).Scan(&x.RunID, &x.SubmissionID, &x.TaskID, &x.TaskVersion, &x.InputKey, &input, &x.RunSeq, &x.CallIndex, &snap, &x.Stage, &x.Phase, &last, &remaining)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if e != nil {
		return nil, e
	}
	x.Input = json.RawMessage(input)
	if e = json.Unmarshal([]byte(snap), &x.Task); e != nil {
		return nil, e
	}
	if e = json.Unmarshal([]byte(last), &x.LastCheck); e != nil {
		return nil, e
	}
	if x.Stage == "ready" {
		if _, e = tx.Exec("INSERT INTO resources(key,run_id) SELECT concurrency_key,? FROM submissions WHERE id=?", x.RunID, x.SubmissionID); e != nil {
			return nil, e
		}
		if _, e = tx.Exec("DELETE FROM ready_queue WHERE run_id=?", x.RunID); e != nil {
			return nil, e
		}
		x.Phase = "claimed"
		x.Stage = "finish_check"
		if len(x.Task.Before) > 0 {
			x.Stage = "before"
		}
		if x.Task.Start != nil {
			x.Stage = "start_check"
		}
	}
	x.StepID, e = uuid()
	if e != nil {
		return nil, e
	}
	if x.Stage == "agent" {
		x.CallIndex++
	}
	timeout, _ := time.ParseDuration(x.Task.Limits[x.Stage+"_timeout"])
	x.TimeoutNS = int64(timeout)
	if x.Stage != "start_check" && remaining < x.TimeoutNS {
		x.TimeoutNS = remaining
	}
	if x.TimeoutNS <= 0 {
		return nil, errors.New("run time budget exhausted")
	}
	if _, e = tx.Exec("UPDATE runtime SET stage=?,phase=?,owner_version=owner_version+1 WHERE run_id=?", x.Stage, x.Phase, x.RunID); e != nil {
		return nil, e
	}
	state := "running"
	if x.Phase == "preflight" {
		state = "waiting"
	}
	if _, e = tx.Exec("UPDATE runs SET state=?,calls_used=? WHERE id=?", state, x.CallIndex, x.RunID); e != nil {
		return nil, e
	}
	if _, e = tx.Exec("INSERT INTO steps(id,run_id,stage,call_index,owner_version) SELECT ?,run_id,?,?,owner_version FROM runtime WHERE run_id=?", x.StepID, x.Stage, x.CallIndex, x.RunID); e != nil {
		return nil, e
	}
	return &x, tx.Commit()
}
func (s *Store) Started(p domain.ProcessIdentity) error {
	result, e := s.db.Exec("UPDATE steps SET pid=?,pgid=?,boot_id=?,process_start=? WHERE id=? AND result IS NULL", p.PID, p.PGID, p.BootID, p.ProcessStart, p.StepID)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e == nil && n != 1 {
		return errors.New("step ownership changed")
	}
	return e
}
func (s *Store) Complete(x domain.Execution, o domain.Outcome, next string) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	raw, e := json.Marshal(o)
	if e != nil {
		return e
	}
	result, e := tx.Exec(`UPDATE steps SET result=? WHERE id=? AND result IS NULL AND owner_version=(SELECT owner_version FROM runtime WHERE run_id=?)`, string(raw), x.StepID, x.RunID)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return errors.New("stale Step completion")
	}
	last := "null"
	if x.LastCheck != nil {
		b, _ := json.Marshal(x.LastCheck)
		last = string(b)
	}
	if strings.HasSuffix(x.Stage, "_check") {
		kind := strings.TrimSuffix(x.Stage, "_check")
		b, _ := json.Marshal(domain.CheckFeedback{Kind: kind, ExitCode: o.ExitCode, Stdout: o.Stdout, Stderr: o.Stderr, Truncated: o.Truncated})
		last = string(b)
	}
	elapsed := o.ElapsedNS
	if x.Stage == "start_check" {
		elapsed = 0
	}
	state := "running"
	phase := x.Phase
	nextAt := int64(0)
	if next == "waiting" || next == "ready" {
		state = "waiting"
		phase = "preflight"
		if next == "waiting" {
			next = "start_check"
			poll, _ := time.ParseDuration(x.Task.Start.PollEvery)
			nextAt = time.Now().Add(poll).UnixNano()
		}
		if _, e = tx.Exec("DELETE FROM resources WHERE run_id=?", x.RunID); e != nil {
			return e
		}
		if next == "ready" {
			if _, e = tx.Exec("INSERT INTO ready_queue(run_id) VALUES(?)", x.RunID); e != nil {
				return e
			}
		}
	}
	terminal := next == "succeeded" || strings.HasPrefix(next, "failed:") || strings.HasPrefix(next, "skipped:")
	if terminal || strings.HasPrefix(next, "blocked:") {
		state = strings.SplitN(next, ":", 2)[0]
	}
	if _, e = tx.Exec("UPDATE runtime SET stage=?,phase=?,last_check=?,remaining_ns=max(0,remaining_ns-?),next_at=? WHERE run_id=?", next, phase, last, elapsed, nextAt, x.RunID); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE runs SET state=? WHERE id=?", state, x.RunID); e != nil {
		return e
	}
	if terminal {
		if _, e = tx.Exec("DELETE FROM resources WHERE run_id=?", x.RunID); e != nil {
			return e
		}
		var remaining int
		if e = tx.QueryRow("SELECT remaining FROM repeat_budgets WHERE submission_id=?", x.SubmissionID).Scan(&remaining); e != nil {
			return e
		}
		if remaining > 0 {
			id, err := uuid()
			if err != nil {
				return err
			}
			if _, e = tx.Exec("UPDATE repeat_budgets SET remaining=remaining-1 WHERE submission_id=?", x.SubmissionID); e != nil {
				return e
			}
			if _, e = tx.Exec("INSERT INTO runs(id,submission_id,run_seq,state) VALUES(?,?,?,'waiting')", id, x.SubmissionID, x.RunSeq+1); e != nil {
				return e
			}
		} else {
			if _, e = tx.Exec("UPDATE submissions SET state='completed' WHERE id=?", x.SubmissionID); e != nil {
				return e
			}
		}
	}
	return tx.Commit()
}
func (s *Store) runtimeView(id string, out map[string]any) (map[string]any, error) {
	var stage, last string
	var budget int64
	e := s.db.QueryRow("SELECT stage,coalesce(last_check,'null'),remaining_ns FROM runtime WHERE run_id=?", id).Scan(&stage, &last, &budget)
	if errors.Is(e, sql.ErrNoRows) {
		return out, nil
	}
	if e != nil {
		return nil, e
	}
	out["stage"] = stage
	out["time_remaining"] = time.Duration(budget).String()
	out["last_check"] = json.RawMessage(last)
	if strings.HasPrefix(stage, "blocked:") {
		out["blocked_reason"] = strings.TrimPrefix(stage, "blocked:")
	}
	if strings.HasPrefix(stage, "failed:") {
		out["failure_reason"] = strings.TrimPrefix(stage, "failed:")
	}
	rows, e := s.db.Query("SELECT id,stage,call_index,result,pid,pgid,boot_id,process_start FROM steps WHERE run_id=? ORDER BY seq", id)
	if e != nil {
		return nil, e
	}
	defer func() { _ = rows.Close() }()
	steps := []any{}
	for rows.Next() {
		var step, st string
		var call int
		var result, boot, start sql.NullString
		var pid, pgid sql.NullInt64
		if e = rows.Scan(&step, &st, &call, &result, &pid, &pgid, &boot, &start); e != nil {
			return nil, e
		}
		item := map[string]any{"step_id": step, "stage": st, "call_index": call, "result": nil, "pid": pid.Int64, "pgid": pgid.Int64, "boot_id": boot.String, "process_start": start.String}
		if result.Valid {
			item["result"] = json.RawMessage(result.String)
		}
		steps = append(steps, item)
	}
	out["steps"] = steps
	return out, rows.Err()
}
