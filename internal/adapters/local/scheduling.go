package local

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/agentwiki/todoable/internal/domain"
)

func initializeRuntime(tx *sql.Tx, now int64) error {
	if _, e := tx.Exec("INSERT OR IGNORE INTO run_schedule(run_id) SELECT id FROM runs"); e != nil {
		return e
	}
	rows, e := tx.Query(`SELECT r.id,s.snapshot FROM runs r JOIN submissions s ON s.id=r.submission_id JOIN run_schedule rs ON rs.run_id=r.id LEFT JOIN runtime rt ON rt.run_id=r.id WHERE rt.run_id IS NULL AND r.state='waiting' AND rs.eligible_at<=? AND NOT EXISTS(SELECT 1 FROM submissions older WHERE older.task_id=s.task_id AND older.input_key=s.input_key AND older.seq<s.seq AND older.state='active') ORDER BY s.seq,r.run_seq`, now)
	if e != nil {
		return e
	}
	type pendingRun struct{ id, snapshot string }
	pending := []pendingRun{}
	for rows.Next() {
		var p pendingRun
		if e = rows.Scan(&p.id, &p.snapshot); e != nil {
			_ = rows.Close()
			return e
		}
		pending = append(pending, p)
	}
	e = rows.Err()
	_ = rows.Close()
	if e != nil {
		return e
	}
	for _, p := range pending {
		var task domain.Task
		if e = json.Unmarshal([]byte(p.snapshot), &task); e != nil {
			return e
		}
		budget, _ := time.ParseDuration(task.Limits["run_timeout"])
		stage := "ready"
		phase := ""
		if task.Start != nil {
			stage = "start_check"
			phase = "preflight"
		}
		if _, e = tx.Exec("INSERT INTO runtime(run_id,stage,phase,remaining_ns) VALUES(?,?,?,?)", p.id, stage, phase, int64(budget)); e != nil {
			return e
		}
		if stage == "ready" {
			if e = enqueueReady(tx, p.id); e != nil {
				return e
			}
		}
	}
	return nil
}
func enqueueReady(tx *sql.Tx, id string) error {
	result, e := tx.Exec("INSERT INTO ready_queue(run_id) VALUES(?)", id)
	if e != nil {
		return e
	}
	seq, e := result.LastInsertId()
	if e != nil {
		return e
	}
	_, e = tx.Exec("UPDATE run_schedule SET ready_seq=? WHERE run_id=?", seq, id)
	return e
}
func finishRun(tx *sql.Tx, x domain.Execution) error {
	cancelled, err := cancelledSubmission(tx, x.SubmissionID)
	if err != nil {
		return err
	}
	if cancelled {
		return finishCancelledRun(tx, x.RunID)
	}
	if _, e := tx.Exec("DELETE FROM resources WHERE run_id=?", x.RunID); e != nil {
		return e
	}
	if _, e := tx.Exec("UPDATE run_schedule SET slot_held=0 WHERE run_id=?", x.RunID); e != nil {
		return e
	}
	var remaining int
	if e := tx.QueryRow("SELECT remaining FROM repeat_budgets WHERE submission_id=?", x.SubmissionID).Scan(&remaining); e != nil {
		return e
	}
	if remaining == 0 {
		if _, e := tx.Exec("UPDATE submissions SET state='completed' WHERE id=?", x.SubmissionID); e != nil {
			return e
		}
		_, e := tx.Exec("INSERT OR IGNORE INTO completion_times VALUES(?,?)", x.SubmissionID, time.Now().UnixNano())
		return e
	}
	id, e := uuid()
	if e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE repeat_budgets SET remaining=remaining-1 WHERE submission_id=?", x.SubmissionID); e != nil {
		return e
	}
	if _, e = tx.Exec("INSERT INTO runs(id,submission_id,run_seq,state) VALUES(?,?,?,'waiting')", id, x.SubmissionID, x.RunSeq+1); e != nil {
		return e
	}
	delay, _ := time.ParseDuration(x.Task.RepeatDelay)
	_, e = tx.Exec("INSERT INTO run_schedule(run_id,eligible_at) VALUES(?,?)", id, time.Now().Add(delay).UnixNano())
	return e
}
func (s *Store) viewSummary(id string, out map[string]any) (map[string]any, error) {
	var ready, first, deadline, nextAt int64
	e := s.db.QueryRow("SELECT rs.ready_seq,rs.start_started_at,rs.wait_deadline,coalesce(rt.next_at,0) FROM run_schedule rs LEFT JOIN runtime rt ON rt.run_id=rs.run_id WHERE rs.run_id=?", id).Scan(&ready, &first, &deadline, &nextAt)
	if e != nil && e != sql.ErrNoRows {
		return nil, e
	}
	out["ready_seq"] = ready
	for k, v := range map[string]int64{"start_started_at": first, "start_deadline": deadline, "next_check_at": nextAt} {
		out[k] = nil
		if v != 0 {
			out[k] = time.Unix(0, v).UTC().Format(time.RFC3339Nano)
		}
	}
	rows, e := s.db.Query(`SELECT r.id,r.run_seq,r.state,r.calls_used,coalesce(rt.stage,'waiting') FROM runs r LEFT JOIN runtime rt ON rt.run_id=r.id WHERE r.submission_id=(SELECT submission_id FROM runs WHERE id=?) ORDER BY r.run_seq`, id)
	if e != nil {
		return nil, e
	}
	defer func() { _ = rows.Close() }()
	runs := []any{}
	success, failed, skipped := 0, 0, 0
	last := ""
	for rows.Next() {
		var rid, state, stage string
		var seq, calls int
		if e = rows.Scan(&rid, &seq, &state, &calls, &stage); e != nil {
			return nil, e
		}
		runs = append(runs, map[string]any{"run_id": rid, "run_seq": seq, "state": state, "stage": stage, "calls_used": calls})
		switch state {
		case "succeeded":
			success++
			last = stage
		case "failed":
			failed++
			last = stage
		case "skipped":
			skipped++
			last = stage
		}
	}
	out["runs"] = runs
	out["summary"] = map[string]any{"succeeded": success, "failed": failed, "skipped": skipped, "last_result": last}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	_ = rows.Close()
	return s.resolutionView(id, out)
}
