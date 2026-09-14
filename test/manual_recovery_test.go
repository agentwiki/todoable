package test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func manualFixture(t *testing.T) runtimeFixture {
	t.Helper()
	f := recoveryRuntime(t)
	p := filepath.Join(f.root, "runner.py")
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	script := strings.Replace(string(b), "if s==inp.get('signal_stage'):os.kill(os.getpid(),signal.SIGTERM)", "if s==inp.get('signal_stage') and not os.path.exists(inp['repair']):os.kill(os.getpid(),signal.SIGTERM)", 1)
	writeTest(t, p, []byte(script), 0700)
	return f
}
func currentStep(t *testing.T, v map[string]any) string {
	t.Helper()
	steps := v["steps"].([]any)
	if len(steps) == 0 {
		t.Fatal("missing blocked Step")
	}
	return steps[len(steps)-1].(map[string]any)["step_id"].(string)
}
func resumeArgs(id, step, action string) []string {
	return []string{"resume", id, "--step", step, "--action", action, "--reason", "verified destination and stopped worker"}
}
func resumeMatrix(t *testing.T, focused bool) {
	for _, stage := range []string{"start_check", "before", "finish_check", "agent", "after"} {
		for _, action := range []string{"retry", "confirm-success", "confirm-failure"} {
			selected := map[string]string{"start_check": "confirm-success", "agent": "retry", "after": "confirm-failure"}
			if focused && selected[stage] != action {
				continue
			}
			t.Run(stage+"-"+action, func(t *testing.T) {
				f := manualFixture(t)
				repair := filepath.Join(f.root, "repair")
				a := submitCore(t, f, "runtime", "manual", "resource", map[string]any{"label": "manual", "signal_stage": stage, "repair": repair})
				id := a["run_id"].(string)
				f.daemon(t)
				before := f.await(t, id)
				step := currentStep(t, before)
				args := resumeArgs(id, step, action)
				if action == "confirm-failure" {
					args = append(args, "--exit-code", "17")
				}
				if strings.HasSuffix(stage, "_check") && action != "retry" {
					f.rejected(t, 2, "validation_error", args...)
					after := f.call(t, "run", "show", id, "--json")
					if after["stage"] != before["stage"] || len(after["resolutions"].([]any)) != 0 {
						t.Fatalf("forbidden check confirmation changed state %v", after)
					}
					return
				}
				writeTest(t, repair, nil, 0600)
				result := f.call(t, args...)
				if result["run_id"] != id {
					t.Fatalf("wrong resumed run %v", result)
				}
				v := f.await(t, id)
				want := "succeeded"
				if action == "confirm-failure" && (stage == "before" || stage == "after") {
					want = "failed:" + stage
				}
				if v["stage"] != want {
					t.Fatalf("manual transition %v", v)
				}
				calls := float64(1)
				if stage == "before" && action == "confirm-failure" {
					calls = 0
				}
				if stage == "agent" {
					calls = 2
				}
				if v["calls_used"] != calls {
					t.Fatalf("manual slot budget %v", v)
				}
				audits := v["resolutions"].([]any)
				if len(audits) != 1 {
					t.Fatalf("missing resolution audit %v", v)
				}
				audit := audits[0].(map[string]any)
				if audit["step_id"] != step || audit["action"] != action || audit["reason"] != "verified destination and stopped worker" || audit["manual"] != true || audit["processes_stopped"] != false {
					t.Fatalf("resolution audit %v", audit)
				}
				if action == "confirm-failure" && audit["exit_code"] != float64(17) {
					t.Fatalf("manual failure code %v", audit)
				}
				steps := v["steps"].([]any)
				found := false
				for i, item := range steps {
					s := item.(map[string]any)
					if s["step_id"] == step {
						found = true
						if s["result"].(map[string]any)["kind"] != "unknown" {
							t.Fatalf("manual assertion overwrote observation %v", s)
						}
						if stage == "agent" && (i+1 >= len(steps) || steps[i+1].(map[string]any)["stage"] != "finish_check") {
							t.Fatalf("agent resolution skipped check %v", steps)
						}
					}
				}
				if !found {
					t.Fatal("original blocked Step disappeared")
				}
				// The retrying agent is externally observable exactly once; its first call
				// was interrupted before the fixture wrote an effect.
				if calls > 0 {
					raw, e := os.ReadFile(filepath.Join(f.dir, "runs", id, "artifact"))
					if e != nil || string(raw) != "manual-1" {
						t.Fatalf("manual external effect %q %v", raw, e)
					}
				}
				if action == "retry" && stage == "after" {
					raw, e := os.ReadFile(filepath.Join(f.dir, "runs", id, "published"))
					if e != nil || string(raw) != "manual-1\n" {
						t.Fatalf("retried publication %q %v", raw, e)
					}
				}
			})
		}
	}
}
func resumeBudgetsAndArguments(t *testing.T) {
	f := manualFixture(t)
	repair := filepath.Join(f.root, "repair")
	f.task["limits"] = map[string]string{"run_timeout": "2s"}
	updateOrderTask(t, f)
	a := submitCore(t, f, "runtime", "arguments", "resource", map[string]any{"label": "arguments", "signal_stage": "agent", "repair": repair})
	id := a["run_id"].(string)
	f.daemon(t)
	v := f.await(t, id)
	step := currentStep(t, v)
	for _, args := range [][]string{
		{"resume", id, "--step", step, "--action", "retry", "--reason", ""},
		append(resumeArgs(id, step, "retry"), "--exit-code", "1"),
		resumeArgs(id, step, "confirm-failure"),
		append(resumeArgs(id, step, "confirm-failure"), "--exit-code", "0"),
		append(resumeArgs(id, step, "confirm-failure"), "--exit-code", "256"),
	} {
		f.rejected(t, 2, "validation_error", args...)
	}
	f.rejected(t, 6, "invalid_state", resumeArgs(id, "other-step", "retry")...)
	// Stopped blocks do not consume time, including a fresh CLI's wall clock.
	before, e := time.ParseDuration(v["time_remaining"].(string))
	if e != nil {
		t.Fatal(e)
	}
	time.Sleep(150 * time.Millisecond)
	after := f.call(t, "run", "show", id, "--json")
	remaining, e := time.ParseDuration(after["time_remaining"].(string))
	if e != nil || remaining != before || remaining >= 2*time.Second {
		t.Fatalf("blocked budget reset or consumed before=%v after=%v err=%v", before, remaining, e)
	}
	writeTest(t, repair, nil, 0600)
	f.call(t, resumeArgs(id, step, "retry")...)
	result := f.await(t, id)
	if result["stage"] != "succeeded" || result["calls_used"] != float64(2) {
		t.Fatalf("retry lost original call %v", result)
	}
	final, e := time.ParseDuration(result["time_remaining"].(string))
	if e != nil || final >= remaining {
		t.Fatalf("resume reset/nonconsuming budget %v >= %v", final, remaining)
	}
}
func mismatchedProcesses(t *testing.T, request bool) {
	for _, stage := range []string{"agent", "finish_check"} {
		t.Run(stage, func(t *testing.T) {
			f := recoveryRuntime(t)
			gate := filepath.Join(f.root, "hold")
			input := map[string]any{"label": "mismatch", stage + "_gate": gate}
			a := submitCore(t, f, "runtime", "mismatch", "resource", input)
			id := a["run_id"].(string)
			cmd := crashDaemon(t, f)
			waitExternal(t, f, "mismatch", stage, 1)
			var pid int
			var step string
			waitUntil(t, func() bool {
				return f.database(t).QueryRow("SELECT id,pid FROM steps WHERE run_id=? AND stage=? AND result IS NULL", id, stage).Scan(&step, &pid) == nil && pid > 0
			})
			t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
			killDaemon(t, cmd)
			if _, e := f.database(t).Exec("UPDATE steps SET process_start='0' WHERE id=?", step); e != nil {
				t.Fatal(e)
			}
			restarted := crashDaemon(t, f)
			v := f.await(t, id)
			if v["stage"] != "blocked:process_unknown" || !processExists(pid) {
				t.Fatalf("mismatched PID killed/accepted %v live=%v", v, processExists(pid))
			}
			if request {
				// Own the SQLite writer while stopping the daemon, so it cannot
				// be suspended inside a transaction needed by the CLI.
				tx, err := f.database(t).Begin()
				if err != nil {
					t.Fatal(err)
				}
				if _, err = tx.Exec("UPDATE runtime SET remaining_ns=remaining_ns WHERE run_id=?", id); err != nil {
					t.Fatal(err)
				}
				if err = restarted.Process.Signal(syscall.SIGSTOP); err != nil {
					t.Fatal(err)
				}
				waitUntil(t, func() bool {
					raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(restarted.Process.Pid), "stat"))
					return err == nil && strings.Fields(string(raw[strings.LastIndexByte(string(raw), ')')+1:]))[0] == "T"
				})
				if err = tx.Commit(); err != nil {
					t.Fatal(err)
				}
				actions := []string{"retry"}
				if stage == "agent" {
					actions = append(actions, "confirm-success", "confirm-failure")
				}
				for _, action := range actions {
					before := resumeDurableState(t, f, id)
					args := resumeArgs(id, step, action)
					if action == "confirm-failure" {
						args = append(args, "--exit-code", "17")
					}
					f.rejected(t, 6, "process_unknown", args...)
					if after := resumeDurableState(t, f, id); after != before {
						t.Fatalf("rejected %s changed durable state before=%s after=%s", action, before, after)
					}
					if !processExists(pid) {
						t.Fatalf("rejected %s killed unidentified process", action)
					}
				}
				if err = restarted.Process.Signal(syscall.SIGCONT); err != nil {
					t.Fatal(err)
				}
			}
			if !processExists(pid) {
				t.Fatal("resume killed a PID with different start identity")
			}
			var slots, resources, checks int
			if e := f.database(t).QueryRow("SELECT (SELECT slot_held FROM run_schedule WHERE run_id=?),(SELECT count(*) FROM resources WHERE run_id=?),(SELECT count(*) FROM check_slots WHERE step_id=?)", id, id, step).Scan(&slots, &resources, &checks); e != nil || slots != 1 || resources != 1 {
				t.Fatalf("unconfirmed ownership released %d %d %d %v", slots, resources, checks, e)
			}
			wantChecks := 0
			if stage == "finish_check" {
				wantChecks = 1
			}
			if checks != wantChecks {
				t.Fatalf("unconfirmed check slot %d", checks)
			}
			b := submitCore(t, f, "runtime", "same", "resource", map[string]any{"label": "same"})
			waitReady(t, f, b["run_id"].(string))
			good := submitCore(t, f, "runtime", "other", "other", map[string]any{"label": "other"})
			completedRuns(t, f, good, 1)
			for _, event := range orderEvents(t, f) {
				if event.Label == "same" && event.Stage != "start_check" {
					t.Fatalf("mismatch released resource %v", event)
				}
			}
			latest := f.call(t, "run", "show", id, "--json")
			if latest["stage"] != "blocked:process_unknown" || len(latest["steps"].([]any)) != len(v["steps"].([]any)) {
				t.Fatalf("rejected resolution later executed %v", latest)
			}
			for _, name := range []string{"artifact", "published", "count"} {
				if _, err := os.Stat(filepath.Join(f.dir, "runs", id, name)); !os.IsNotExist(err) {
					t.Fatalf("rejected resolution produced %s: %v", name, err)
				}
			}
		})
	}
}
func orphanGroupRecovery(t *testing.T) {
	f := recoveryRuntime(t)
	p := filepath.Join(f.root, "runner.py")
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	injection := `if s=='agent':
 import subprocess
 child=subprocess.Popen(['/usr/bin/python3','-c','import time;time.sleep(60)'])
 open(os.path.join(rd,'orphan.pid'),'w').write(str(child.pid))
 sys.exit(0)
`
	writeTest(t, p, []byte(strings.Replace(string(b), "gates=inp.get(s+'_gates',{})", injection+"gates=inp.get(s+'_gates',{})", 1)), 0700)
	a := submitCore(t, f, "runtime", "orphan", "resource", map[string]any{"label": "orphan"})
	id := a["run_id"].(string)
	cmd := crashDaemon(t, f)
	var child int
	waitUntil(t, func() bool {
		b, e := os.ReadFile(filepath.Join(f.dir, "runs", id, "orphan.pid"))
		if e != nil {
			return false
		}
		child, e = strconv.Atoi(string(b))
		return e == nil
	})
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	var leader int
	waitUntil(t, func() bool {
		return f.database(t).QueryRow("SELECT pid FROM steps WHERE run_id=? AND stage='agent'", id).Scan(&leader) == nil && leader > 0 && !processExists(leader)
	})
	killDaemon(t, cmd)
	if !processExists(child) {
		t.Fatal("orphan did not survive daemon")
	}
	f.daemon(t)
	v := f.await(t, id)
	if v["stage"] != "blocked:outcome_unknown" || processExists(child) {
		t.Fatalf("orphan not stopped before blocking %v alive=%v", v, processExists(child))
	}
	var owner string
	if e = f.database(t).QueryRow("SELECT run_id FROM resources WHERE key='resource'").Scan(&owner); e != nil || owner != id {
		t.Fatalf("orphan changing resource %s %v", owner, e)
	}
	count := 0
	for _, event := range orderEvents(t, f) {
		if event.RunID == id && event.Stage == "agent" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("orphan agent replay %d", count)
	}
}

func resumeCheckCapacity(t *testing.T) {
	t.Helper()
	f := manualFixture(t)
	delete(f.task, "start")
	updateOrderTask(t, f)
	repair := filepath.Join(f.root, "repair")
	finishGate := filepath.Join(f.root, "finish-release")
	startGate := filepath.Join(f.root, "start-release")
	a := submitCore(t, f, "runtime", "resumed", "resumed", map[string]any{"label": "resumed", "signal_stage": "finish_check", "repair": repair, "finish_check_gate": finishGate})
	id := a["run_id"].(string)
	f.daemon(t)
	v := f.await(t, id)
	if v["stage"] != "blocked:check_error" {
		t.Fatalf("initial finish check not blocked %v", v)
	}
	others := []map[string]any{}
	for _, label := range []string{"one", "two", "three", "four"} {
		others = append(others, submitCore(t, f, "other-task", label, label, map[string]any{"label": label, "start_check_gate": startGate}))
		waitExternal(t, f, label, "start_check", 1)
	}
	writeTest(t, repair, nil, 0600)
	f.call(t, resumeArgs(id, currentStep(t, v), "retry")...)
	time.Sleep(150 * time.Millisecond)
	var checks, slots int
	if e := f.database(t).QueryRow("SELECT (SELECT count(*) FROM steps WHERE run_id=? AND stage='finish_check'),(SELECT count(*) FROM check_slots)", id).Scan(&checks, &slots); e != nil || checks != 1 || slots != 4 {
		t.Fatalf("resumed finish check exceeded cap checks=%d slots=%d err=%v", checks, slots, e)
	}
	events := 0
	for _, event := range orderEvents(t, f) {
		if event.RunID == id && event.Stage == "finish_check" {
			events++
		}
	}
	if events != 1 {
		t.Fatalf("resumed check executed outside slot accounting %d", events)
	}
	writeTest(t, startGate, nil, 0600)
	waitUntil(t, func() bool {
		return f.database(t).QueryRow("SELECT count(*) FROM steps WHERE run_id=? AND stage='finish_check'", id).Scan(&checks) == nil && checks == 2
	})
	writeTest(t, finishGate, nil, 0600)
	runs := completedRuns(t, f, a, 1)
	if runs[0]["stage"] != "succeeded" || runs[0]["calls_used"] != float64(1) {
		t.Fatalf("capacity release did not resume check %v", runs)
	}
	for _, other := range others {
		completedRuns(t, f, other, 1)
	}
	before, agent, finish := 0, 0, 0
	for _, event := range orderEvents(t, f) {
		if event.RunID != id {
			continue
		}
		switch event.Stage {
		case "before":
			before++
		case "agent":
			agent++
		case "finish_check":
			finish++
		}
	}
	if before != 1 || agent != 1 || finish != 3 {
		t.Fatalf("resume executed wrong stages before=%d agent=%d finish=%d", before, agent, finish)
	}
	for name, want := range map[string]string{"artifact": "resumed-1", "published": "resumed-1\n"} {
		raw, e := os.ReadFile(filepath.Join(f.dir, "runs", id, name))
		if e != nil || string(raw) != want {
			t.Fatalf("resumed output %s %q %v", name, raw, e)
		}
	}
	if e := f.database(t).QueryRow("SELECT count(*) FROM check_slots").Scan(&slots); e != nil || slots != 0 {
		t.Fatalf("finished checks retained capacity %d %v", slots, e)
	}
}

func resumeDurableState(t *testing.T, f runtimeFixture, id string) string {
	t.Helper()
	var state string
	err := f.database(t).QueryRow(`SELECT json_object('state',r.state,'calls',r.calls_used,'stage',rt.stage,'remaining',rt.remaining_ns,'owner',rt.owner_version,'ticking',coalesce(c.ticking_at,0),'repeat',b.remaining,'resource',(SELECT count(*) FROM resources WHERE run_id=r.id),'slot',rs.slot_held,'checks',(SELECT count(*) FROM check_slots WHERE step_id IN(SELECT id FROM steps WHERE run_id=r.id)),'steps',(SELECT json_group_array(json_object('id',id,'result',result,'owner',owner_version)) FROM steps WHERE run_id=r.id),'audits',(SELECT count(*) FROM step_resolutions WHERE run_id=r.id)) FROM runs r JOIN runtime rt ON rt.run_id=r.id JOIN run_schedule rs ON rs.run_id=r.id JOIN repeat_budgets b ON b.submission_id=r.submission_id LEFT JOIN run_clocks c ON c.run_id=r.id WHERE r.id=?`, id).Scan(&state)
	if err != nil {
		t.Fatal(err)
	}
	return state
}
