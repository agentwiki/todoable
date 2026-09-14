package local

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/agentwiki/todoable/internal/domain"
)

type blockedStep struct {
	execution domain.Execution
	owner     domain.ProcessIdentity
	result    domain.Outcome
	remaining int64
	confirmed bool
}

func loadBlocked(tx *sql.Tx, r domain.ResumeRequest) (blockedStep, error) {
	var b blockedStep
	var snapshot, result string
	x := &b.execution
	p := &b.owner
	err := tx.QueryRow(`SELECT st.id,st.run_id,r.submission_id,r.run_seq,st.stage,r.calls_used,rt.phase,s.snapshot,coalesce(st.result,'{}'),coalesce(st.pid,0),coalesce(st.pgid,0),coalesce(st.boot_id,''),coalesce(st.process_start,''),rt.remaining_ns,EXISTS(SELECT 1 FROM stop_confirmations WHERE step_id=st.id)
 FROM steps st JOIN runs r ON r.id=st.run_id JOIN runtime rt ON rt.run_id=r.id JOIN submissions s ON s.id=r.submission_id
 WHERE r.id=? AND st.id=? AND r.state='blocked' AND st.seq=(SELECT max(seq) FROM steps WHERE run_id=r.id)`, r.RunID, r.StepID).Scan(&x.StepID, &x.RunID, &x.SubmissionID, &x.RunSeq, &x.Stage, &x.CallIndex, &x.Phase, &snapshot, &result, &p.PID, &p.PGID, &p.BootID, &p.ProcessStart, &b.remaining, &b.confirmed)
	if errors.Is(err, sql.ErrNoRows) {
		return b, &domain.Fault{Code: 6, Kind: "invalid_state", Message: "resume requires the current blocked Step"}
	}
	if err != nil {
		return b, err
	}
	p.StepID = x.StepID
	if err = json.Unmarshal([]byte(snapshot), &x.Task); err != nil {
		return b, err
	}
	err = json.Unmarshal([]byte(result), &b.result)
	return b, err
}
func stoppedEvidence(b blockedStep, declared bool) bool {
	if declared || b.confirmed {
		return true
	}
	if b.result.Untracked {
		return false
	}
	switch b.result.Kind {
	case "exited", "unknown", "interrupted", "start_failed", "not_started":
		return true
	}
	stopped, err := RecordedProcessStopped(b.owner)
	return err == nil && stopped
}
func (s *Store) Resume(r domain.ResumeRequest) (map[string]any, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.runtimeNow()
	if err = chargeClocks(tx, now); err != nil {
		return nil, err
	}
	b, err := loadBlocked(tx, r)
	if err != nil {
		return nil, err
	}
	cancelled, err := cancelledSubmission(tx, b.execution.SubmissionID)
	if err != nil {
		return nil, err
	}
	if cancelled && r.Action == "retry" {
		return nil, &domain.Fault{Code: 6, Kind: "cancel_requested", Message: "cancelled submissions cannot retry"}
	}
	check := strings.HasSuffix(b.execution.Stage, "_check")
	if check && r.Action != "retry" {
		return nil, domain.Invalid("checks allow retry only")
	}
	if !stoppedEvidence(b, r.ProcessesStopped) {
		return nil, &domain.Fault{Code: 6, Kind: "process_unknown", Message: "previous processes are not confirmed stopped"}
	}
	if r.Action == "retry" && b.remaining <= 0 {
		return nil, &domain.Fault{Code: 6, Kind: "budget_exhausted", Message: "no Run time remains for retry"}
	}
	next := b.execution.Stage
	code := any(nil)
	if r.Action != "retry" {
		exit := 0
		if r.ExitCode != nil {
			exit = *r.ExitCode
		}
		code = exit
		next = domain.NextStage(b.execution, domain.Outcome{Kind: "exited", ExitCode: exit})
	} else if next == "agent" {
		next = "finish_check"
	}
	if _, err = tx.Exec("INSERT INTO step_resolutions(run_id,step_id,action,exit_code,reason,processes_stopped,created_at) VALUES(?,?,?,?,?,?,?)", r.RunID, r.StepID, r.Action, code, r.Reason, r.ProcessesStopped, now); err != nil {
		return nil, err
	}
	if err = stopClock(tx, r.RunID); err != nil {
		return nil, err
	}
	if _, err = tx.Exec("DELETE FROM check_slots WHERE step_id=?", r.StepID); err != nil {
		return nil, err
	}
	if _, err = tx.Exec("UPDATE run_schedule SET slot_held=0 WHERE run_id=?", r.RunID); err != nil {
		return nil, err
	}
	// A user assertion never pretends to be an observed command result.
	if _, err = tx.Exec("UPDATE steps SET result=coalesce(result,'{\"kind\":\"unobserved\",\"exit_code\":-1}') WHERE id=?", r.StepID); err != nil {
		return nil, err
	}
	if cancelled {
		next = "cancelled"
	}
	terminal := next == "cancelled" || next == "succeeded" || strings.HasPrefix(next, "failed:")
	if !terminal && b.remaining <= 0 {
		next = "failed:run_timeout"
		terminal = true
	}
	if terminal {
		if _, err = tx.Exec("UPDATE runtime SET stage=?,owner_version=owner_version+1 WHERE run_id=?", next, r.RunID); err != nil {
			return nil, err
		}
		if _, err = tx.Exec("UPDATE runs SET state=? WHERE id=?", strings.SplitN(next, ":", 2)[0], r.RunID); err != nil {
			return nil, err
		}
		if err = finishRun(tx, b.execution); err != nil {
			return nil, err
		}
	} else {
		if next == "start_check" { // Re-enter the start protocol before acquiring a new claim.
			if _, err = tx.Exec("DELETE FROM resources WHERE run_id=?", r.RunID); err != nil {
				return nil, err
			}
			if _, err = tx.Exec("UPDATE runtime SET stage='start_check',phase='preflight',next_at=0,owner_version=owner_version+1 WHERE run_id=?", r.RunID); err != nil {
				return nil, err
			}
		} else {
			if _, err = tx.Exec("INSERT INTO resume_queue(run_id,stage) VALUES(?,?)", r.RunID, next); err != nil {
				return nil, err
			}
			if _, err = tx.Exec("UPDATE runtime SET stage='ready',next_at=0,owner_version=owner_version+1 WHERE run_id=?", r.RunID); err != nil {
				return nil, err
			}
			if err = enqueueReady(tx, r.RunID); err != nil {
				return nil, err
			}
		}
		// Only a confirmed blocked interval pauses an existing execution clock.
		// Queueing for a slot after resolution is part of that same Run.
		if _, err = tx.Exec("UPDATE run_clocks SET ticking_at=? WHERE run_id=? AND ticking_at=0", now, r.RunID); err != nil {
			return nil, err
		}
		if _, err = tx.Exec("UPDATE runs SET state='waiting' WHERE id=?", r.RunID); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s.Run(r.RunID)
}
func (s *Store) resolutionView(id string, out map[string]any) (map[string]any, error) {
	rows, err := s.db.Query("SELECT step_id,action,exit_code,reason,processes_stopped,created_at FROM step_resolutions WHERE run_id=? ORDER BY seq", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []any{}
	for rows.Next() {
		var step, action, reason string
		var code sql.NullInt64
		var declared bool
		var at int64
		if err = rows.Scan(&step, &action, &code, &reason, &declared, &at); err != nil {
			return nil, err
		}
		var exit any
		if code.Valid {
			exit = code.Int64
		}
		records = append(records, map[string]any{"step_id": step, "action": action, "exit_code": exit, "reason": reason, "processes_stopped": declared, "created_at": at, "manual": true})
	}
	out["resolutions"] = records
	if err = rows.Err(); err != nil {
		return nil, err
	}
	_ = rows.Close()
	return s.cancellationView(id, out)
}
