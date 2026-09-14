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
			f.daemon(t)
			v := f.await(t, id)
			if v["stage"] != "blocked:process_unknown" || !processExists(pid) {
				t.Fatalf("mismatched PID killed/accepted %v live=%v", v, processExists(pid))
			}
			if request {
				f.rejected(t, 6, "process_unknown", resumeArgs(id, step, "retry")...)
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
