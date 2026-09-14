package local

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/agentwiki/todoable/internal/domain"
)

// recoverIntents runs under the daemon lock, before any worker can reserve.
// Recorded blocks require an explicit human action; restarting is not one.
func (s *Store) recoverIntents(ctx context.Context) error {
	rows, err := s.db.Query(`SELECT st.id,st.run_id,r.submission_id,st.stage,st.call_index,rt.phase,s.snapshot,coalesce(rt.last_check,'null'),coalesce(st.pid,0),coalesce(st.pgid,0),coalesce(st.boot_id,''),coalesce(st.process_start,'')
 FROM steps st JOIN runs r ON r.id=st.run_id JOIN runtime rt ON rt.run_id=r.id JOIN submissions s ON s.id=r.submission_id
 WHERE st.result IS NULL AND r.state IN ('waiting','running') ORDER BY st.seq`)
	if err != nil {
		return err
	}
	type intent struct {
		execution domain.Execution
		owner     domain.ProcessIdentity
	}
	intents := []intent{}
	for rows.Next() {
		var i intent
		var snapshot, last string
		x := &i.execution
		p := &i.owner
		if err = rows.Scan(&x.StepID, &x.RunID, &x.SubmissionID, &x.Stage, &x.CallIndex, &x.Phase, &snapshot, &last, &p.PID, &p.PGID, &p.BootID, &p.ProcessStart); err != nil {
			break
		}
		p.StepID = x.StepID
		if err = json.Unmarshal([]byte(snapshot), &x.Task); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(last), &x.LastCheck); err != nil {
			break
		}
		intents = append(intents, i)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, i := range intents {
		stopped, inspectErr := StopRecordedProcess(ctx, i.owner)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		out := domain.Outcome{Kind: "process_unknown", ExitCode: -1, PID: i.owner.PID, PGID: i.owner.PGID, BootID: i.owner.BootID, ProcessStart: i.owner.ProcessStart}
		if inspectErr != nil {
			out.Stderr = inspectErr.Error()
		}
		next := "blocked:process_unknown"
		check := strings.HasSuffix(i.execution.Stage, "_check")
		if stopped {
			out.Kind = "interrupted"
			next = "blocked:outcome_unknown"
			if check {
				next = i.execution.Stage
			}
		} else if i.owner.PID == 0 && !check {
			// Intent is durable, but whether exec happened is unknowable. Keep
			// the Run slot until the missing process evidence is resolved.
			next = "blocked:outcome_unknown"
		}
		if err = s.Complete(i.execution, out, next); err != nil {
			return err
		}
	}
	return nil
}
