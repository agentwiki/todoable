package local

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/agentwiki/todoable/internal/domain"
)

func (s *Store) Cancel(r domain.CancelRequest) (map[string]any, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var state string
	if err = tx.QueryRow("SELECT state FROM submissions WHERE id=?", r.SubmissionID).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &domain.Fault{Code: 4, Kind: "not_found", Message: "submission not found"}
		}
		return nil, err
	}
	now := s.runtimeNow()
	if _, err = tx.Exec(`INSERT INTO cancel_requests(submission_id,reason,acknowledge_effects,processes_stopped) VALUES(?,?,?,?) ON CONFLICT(submission_id) DO UPDATE SET reason=excluded.reason,acknowledge_effects=max(acknowledge_effects,excluded.acknowledge_effects),processes_stopped=max(processes_stopped,excluded.processes_stopped)`, r.SubmissionID, r.Reason, r.AcknowledgeEffects, r.ProcessesStopped); err != nil {
		return nil, err
	}
	if _, err = tx.Exec("INSERT INTO cancellation_audit(submission_id,reason,acknowledge_effects,processes_stopped,created_at) VALUES(?,?,?,?,?)", r.SubmissionID, r.Reason, r.AcknowledgeEffects, r.ProcessesStopped, now); err != nil {
		return nil, err
	}
	if _, err = tx.Exec("UPDATE repeat_budgets SET remaining=0 WHERE submission_id=?", r.SubmissionID); err != nil {
		return nil, err
	}
	if err = chargeClocks(tx, now); err != nil {
		return nil, err
	}
	if err = settleCancellations(tx); err != nil {
		return nil, err
	}
	if err = tx.QueryRow("SELECT state FROM submissions WHERE id=?", r.SubmissionID).Scan(&state); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"protocol_version": 1, "submission_id": r.SubmissionID, "state": state, "cancel_requested": true}, nil
}
func cancelledSubmission(tx *sql.Tx, id string) (bool, error) {
	var n int
	err := tx.QueryRow("SELECT count(*) FROM cancel_requests WHERE submission_id=?", id).Scan(&n)
	return n != 0, err
}
func finishCancelledRun(tx *sql.Tx, id string) error {
	if _, err := tx.Exec("UPDATE runs SET state='cancelled' WHERE id=?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE runtime SET stage='cancelled',owner_version=owner_version+1 WHERE run_id=?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM resources WHERE run_id=?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE run_schedule SET slot_held=0 WHERE run_id=?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM ready_queue WHERE run_id=?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM resume_queue WHERE run_id=?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM check_slots WHERE step_id IN(SELECT id FROM steps WHERE run_id=?)", id); err != nil {
		return err
	}
	if err := stopClock(tx, id); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE submissions SET state='cancelled' WHERE id=(SELECT submission_id FROM runs WHERE id=?) AND NOT EXISTS(SELECT 1 FROM runs WHERE submission_id=submissions.id AND state IN ('waiting','running','blocked'))`, id)
	return err
}
func settleCancellations(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT r.id,r.state,c.acknowledge_effects,c.processes_stopped,coalesce((SELECT id FROM steps WHERE run_id=r.id ORDER BY seq DESC LIMIT 1),'') FROM runs r JOIN cancel_requests c ON c.submission_id=r.submission_id WHERE r.state IN ('waiting','running','blocked') AND NOT EXISTS(SELECT 1 FROM steps WHERE run_id=r.id AND result IS NULL)`)
	if err != nil {
		return err
	}
	type pending struct {
		id, state, step string
		ack, declared   bool
	}
	items := []pending{}
	for rows.Next() {
		var p pending
		if err = rows.Scan(&p.id, &p.state, &p.ack, &p.declared, &p.step); err != nil {
			break
		}
		items = append(items, p)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, p := range items {
		if p.state == "blocked" {
			b, e := loadBlocked(tx, domain.ResumeRequest{RunID: p.id, StepID: p.step})
			if e != nil {
				return e
			}
			if !stoppedEvidence(b, p.declared) {
				continue
			}
			if e = stopClock(tx, p.id); e != nil {
				return e
			}
			if _, e = tx.Exec("UPDATE run_schedule SET slot_held=0 WHERE run_id=?", p.id); e != nil {
				return e
			}
			if _, e = tx.Exec("DELETE FROM check_slots WHERE step_id=?", p.step); e != nil {
				return e
			}
			if !strings.HasSuffix(b.execution.Stage, "_check") {
				if !p.ack {
					continue
				}
				if _, e = tx.Exec("UPDATE cancel_requests SET effects_unknown=1 WHERE submission_id=?", b.execution.SubmissionID); e != nil {
					return e
				}
			}
		}
		if err = finishCancelledRun(tx, p.id); err != nil {
			return err
		}
	}
	return nil
}

// CancellationRequested is checked immediately before exec and during running
// commands. A request can race with exec; the next poll then attempts a stop.
func (s *Store) CancellationRequested(stepID string) (bool, error) {
	var n int
	err := s.db.QueryRow("SELECT count(*) FROM steps st JOIN runs r ON r.id=st.run_id JOIN cancel_requests c ON c.submission_id=r.submission_id WHERE st.id=?", stepID).Scan(&n)
	return n != 0, err
}

// ReconcileCancellations stops old blocked groups without holding a transaction.
// Ordinary active commands are handled by their runner's cancellation polling.
func (s *Store) ReconcileCancellations(ctx context.Context) error {
	rows, err := s.db.Query(`SELECT st.id,coalesce(st.pid,0),coalesce(st.pgid,0),coalesce(st.boot_id,''),coalesce(st.process_start,''),coalesce(st.result,'{}') FROM steps st JOIN runs r ON r.id=st.run_id JOIN cancel_requests c ON c.submission_id=r.submission_id WHERE r.state='blocked' AND st.seq=(SELECT max(seq) FROM steps WHERE run_id=r.id) AND NOT EXISTS(SELECT 1 FROM stop_confirmations WHERE step_id=st.id)`)
	if err != nil {
		return err
	}
	owners := []domain.ProcessIdentity{}
	for rows.Next() {
		var p domain.ProcessIdentity
		var raw string
		if err = rows.Scan(&p.StepID, &p.PID, &p.PGID, &p.BootID, &p.ProcessStart, &raw); err != nil {
			break
		}
		var out domain.Outcome
		if err = json.Unmarshal([]byte(raw), &out); err != nil {
			break
		}
		if out.Kind == "process_unknown" && !out.Untracked {
			owners = append(owners, p)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, p := range owners {
		stopped, _ := StopRecordedProcess(ctx, p)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if stopped {
			if _, err = s.db.Exec("INSERT OR IGNORE INTO stop_confirmations(step_id) VALUES(?)", p.StepID); err != nil {
				return err
			}
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = chargeClocks(tx, s.runtimeNow()); err != nil {
		return err
	}
	if err = settleCancellations(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) cancellationView(id string, out map[string]any) (map[string]any, error) {
	var reason string
	var effects bool
	err := s.db.QueryRow("SELECT reason,effects_unknown FROM cancel_requests WHERE submission_id=(SELECT submission_id FROM runs WHERE id=?)", id).Scan(&reason, &effects)
	if err == sql.ErrNoRows {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	out["cancel_requested"] = true
	out["effects_unknown"] = effects
	out["cancel_reason"] = reason
	rows, err := s.db.Query("SELECT reason,acknowledge_effects,processes_stopped,created_at FROM cancellation_audit WHERE submission_id=(SELECT submission_id FROM runs WHERE id=?) ORDER BY seq", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []any{}
	for rows.Next() {
		var r string
		var ack, declared bool
		var at int64
		if err = rows.Scan(&r, &ack, &declared, &at); err != nil {
			return nil, err
		}
		records = append(records, map[string]any{"reason": r, "acknowledge_effects": ack, "processes_stopped": declared, "created_at": at})
	}
	out["cancellations"] = records
	return out, rows.Err()
}
