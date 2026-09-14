package test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agentwiki/todoable/internal/adapters/local"
)

// These gates belong to the external command fixture, never the daemon.
func recoveryRuntime(t *testing.T) runtimeFixture {
	t.Helper()
	f := coreFixture(t)
	b, e := os.ReadFile(filepath.Join(f.root, "runner.py"))
	if e != nil {
		t.Fatal(e)
	}
	injection := `if s=='start_check' and inp.get('recheck'):
 p=os.path.join(rd,'starts');n=int(open(p).read())+1 if os.path.exists(p) else 1;open(p,'w').write(str(n))
 if n>1:
  mode=inp['recheck']
  if mode=='false':sys.exit(1)
  if mode=='error':os.kill(os.getpid(),signal.SIGTERM)
  if mode=='escape':inp['escape_stage']='start_check'
if s==inp.get('escape_stage'):
 import subprocess
 code="import os,time;open("+repr(os.path.join(rd,'escaped.pid'))+",'w').write(str(os.getpid()))\nwhile not os.path.exists("+repr(inp['escape_stop'])+") and os.path.isdir("+repr(rd)+"):time.sleep(.02)"
 subprocess.Popen(['/usr/bin/python3','-c',code],start_new_session=True)
 sys.exit(0)
if s=='agent' and inp.get('crash_gate'):
 open(os.path.join(rd,'agent.pid'),'w').write(str(os.getpid()))
 while not os.path.exists(inp['crash_gate']):time.sleep(.01)
if s==inp.get('timeout_stage'):
 while True:time.sleep(.02)
if s==inp.get('signal_stage'):os.kill(os.getpid(),signal.SIGTERM)
`
	script := strings.Replace(string(b), "gates=inp.get(s+'_gates',{})", injection+"gates=inp.get(s+'_gates',{})", 1)
	writeTest(t, filepath.Join(f.root, "runner.py"), []byte(script), 0700)
	return f
}
func processExists(pid int) bool {
	b, e := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if e != nil {
		return false
	}
	fields := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
	return fields[0] != "Z" && fields[0] != "X"
}
func escapedCleanup(t *testing.T, f runtimeFixture) string {
	t.Helper()
	stop := filepath.Join(f.root, "escape-stop")
	t.Cleanup(func() {
		writeTest(t, stop, nil, 0600)
		waitUntil(t, func() bool {
			paths, _ := filepath.Glob(filepath.Join(f.dir, "runs", "*", "escaped.pid"))
			for _, p := range paths {
				b, _ := os.ReadFile(p)
				pid, _ := strconv.Atoi(string(b))
				if processExists(pid) {
					return false
				}
			}
			return true
		})
	})
	return stop
}
func independentSlots(t *testing.T, unknown bool) {
	t.Helper()
	f := recoveryRuntime(t)
	stop := escapedCleanup(t, f)
	ainput := map[string]any{"label": "blocked", "block_before_run": 1}
	if unknown {
		ainput = map[string]any{"label": "blocked", "escape_stage": "before", "escape_stop": stop}
	}
	a := submitCore(t, f, "runtime", "blocked", "blocked", ainput)
	f.daemon(t)
	av := f.await(t, a["run_id"].(string))
	want := "blocked:outcome_unknown"
	if unknown {
		want = "blocked:process_unknown"
	}
	if av["stage"] != want {
		t.Fatalf("blocked owner %v", av)
	}
	blocked := submitCore(t, f, "runtime", "same", "blocked", map[string]any{"label": "same"})
	waitReady(t, f, blocked["run_id"].(string))
	gates := []string{filepath.Join(f.root, "release-0"), filepath.Join(f.root, "release-1"), filepath.Join(f.root, "release-2")}
	subs := []map[string]any{}
	for i := range 3 {
		label := fmt.Sprintf("free-%d", i)
		subs = append(subs, submitCore(t, f, "runtime", label, label, map[string]any{"label": label, "before_gate": gates[i]}))
	}
	capacity := 2
	if unknown {
		capacity = 1
	}
	// Parallel preflight completion determines ready order, not submission order.
	waitUntil(t, func() bool {
		entered := 0
		for _, event := range orderEvents(t, f) {
			if strings.HasPrefix(event.Label, "free-") && event.Stage == "before" {
				entered++
			}
		}
		return entered >= capacity
	})
	for _, sub := range subs {
		waitReady(t, f, sub["run_id"].(string))
	}
	time.Sleep(150 * time.Millisecond)
	assertSlotBoundary(t, f, capacity, unknown)
	for _, gate := range gates {
		writeTest(t, gate, nil, 0600)
	}
	for i := range 3 {
		r := completedRuns(t, f, subs[i], 1)
		if r[0]["stage"] != "succeeded" {
			t.Fatalf("unrelated run did not finish %v", r)
		}
	}
	for _, event := range orderEvents(t, f) {
		if event.Label == "same" && event.Stage != "start_check" {
			t.Fatalf("blocked resource reused %v", event)
		}
	}
	stages := append(runtimeStages(1), "after", "after_done")
	expected := []expectedOrderRun{}
	for i := range 3 {
		expected = append(expected, expectedOrderRun{subs[i]["run_id"].(string), fmt.Sprintf("free-%d", i), 1, stages})
	}
	assertOrderExecutions(t, f, expected)
	active := map[string]bool{}
	peak := 0
	for _, event := range orderEvents(t, f) {
		if !strings.HasPrefix(event.Label, "free-") {
			continue
		}
		switch event.Stage {
		case "before":
			if active[event.RunID] {
				t.Fatalf("duplicate changing interval %v", event)
			}
			active[event.RunID] = true
			if len(active) > capacity {
				t.Fatalf("external intervals plus unconfirmed slot exceeded cap: active=%v unknown=%v", active, unknown)
			}
			peak = max(peak, len(active))
		case "after_done":
			if !active[event.RunID] {
				t.Fatalf("missing changing interval %v", event)
			}
			delete(active, event.RunID)
		}
	}
	if len(active) != 0 || peak != capacity {
		t.Fatalf("incomplete parallel intervals active=%v peak=%d want=%d", active, peak, capacity)
	}
	var held int
	if e := f.database(t).QueryRow("SELECT count(*) FROM run_schedule WHERE slot_held=1").Scan(&held); e != nil || held != map[bool]int{false: 0, true: 1}[unknown] {
		t.Fatalf("remaining held slots %d %v", held, e)
	}
}
func assertSlotBoundary(t *testing.T, f runtimeFixture, capacity int, unknown bool) {
	t.Helper()
	entries := 0
	for _, e := range orderEvents(t, f) {
		if strings.HasPrefix(e.Label, "free-") && e.Stage == "before" {
			entries++
		}
	}
	if entries != capacity {
		t.Fatalf("external parallel intervals %d want %d", entries, capacity)
	}
	var held int
	if e := f.database(t).QueryRow("SELECT count(*) FROM run_schedule WHERE slot_held=1").Scan(&held); e != nil || held != 2 {
		t.Fatalf("slot cap %d %v unknown=%v", held, e, unknown)
	}
}

func crashDaemon(t *testing.T, f runtimeFixture) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(f.bin, "--data-dir", f.dir, "daemon")
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0")
	log, e := os.Create(filepath.Join(f.root, "crash-daemon.stderr"))
	if e != nil {
		t.Fatal(e)
	}
	cmd.Stderr = log
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = log.Close() })
	return cmd
}
func killDaemon(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if e := cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = cmd.Wait()
}
func crashAgentBoundaries(t *testing.T) {
	for _, boundary := range []string{"running", "effect", "exited"} {
		t.Run(boundary, func(t *testing.T) {
			f := recoveryRuntime(t)
			gate := filepath.Join(f.root, "agent-release")
			if boundary == "effect" {
				b, e := os.ReadFile(filepath.Join(f.root, "runner.py"))
				if e != nil {
					t.Fatal(e)
				}
				script := strings.Replace(string(b), ";sys.exit(0)", "\n while not os.path.exists(inp['effect_release']):time.sleep(.01)\n sys.exit(0)", 1)
				writeTest(t, filepath.Join(f.root, "runner.py"), []byte(script), 0700)
			}
			a := submitCore(t, f, "runtime", "crash", "resource", map[string]any{"label": "crash", "crash_gate": gate, "effect_release": filepath.Join(f.root, "effect-release")})
			id := a["run_id"].(string)
			cmd := crashDaemon(t, f)
			pidPath := filepath.Join(f.dir, "runs", id, "agent.pid")
			waitUntil(t, func() bool { _, e := os.Stat(pidPath); return e == nil })
			var pid int
			var step string
			waitUntil(t, func() bool {
				return f.database(t).QueryRow("SELECT id,pid FROM steps WHERE run_id=? AND stage='agent'", id).Scan(&step, &pid) == nil && pid > 0
			})
			t.Cleanup(func() {
				if processExists(pid) {
					_ = syscall.Kill(-pid, syscall.SIGKILL)
				}
			})
			if e := cmd.Process.Signal(syscall.SIGSTOP); e != nil {
				t.Fatal(e)
			}
			if boundary != "running" {
				writeTest(t, gate, nil, 0600)
				waitUntil(t, func() bool { _, e := os.Stat(filepath.Join(f.dir, "runs", id, "artifact")); return e == nil })
			}
			if boundary == "exited" {
				waitUntil(t, func() bool { return !processExists(pid) })
			}
			killDaemon(t, cmd)
			f.daemon(t)
			v := f.await(t, id)
			if v["stage"] != "blocked:outcome_unknown" || v["calls_used"] != float64(1) {
				t.Fatalf("crash %s restored slot/outcome %v", boundary, v)
			}
			if processExists(pid) {
				t.Fatal("recorded command still alive after recovery")
			}
			b := submitCore(t, f, "runtime", "successor", "resource", map[string]any{"label": "successor"})
			waitReady(t, f, b["run_id"].(string))
			control := submitCore(t, f, "runtime", "control", "other", map[string]any{"label": "control"})
			completedRuns(t, f, control, 1)
			events := 0
			for _, e := range orderEvents(t, f) {
				if e.RunID == id && e.Stage == "agent" {
					events++
				}
				if e.Label == "successor" && e.Stage != "start_check" {
					t.Fatalf("resource released %v", e)
				}
			}
			if events != 1 {
				t.Fatalf("agent replayed %d", events)
			}
			raw, e := os.ReadFile(filepath.Join(f.dir, "runs", id, "artifact"))
			if boundary == "running" {
				if !os.IsNotExist(e) {
					t.Fatalf("unissued effect appeared %q %v", raw, e)
				}
			} else if e != nil || string(raw) != "crash-1" {
				t.Fatalf("effect lost %q %v", raw, e)
			}
		})
	}
}

func intentAndSavedResult(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(fmt.Sprintf("saved-%v", saved), func(t *testing.T) {
			f := recoveryRuntime(t)
			a := submitCore(t, f, "runtime", "intent", "resource", map[string]any{"label": "intent"})
			id := a["run_id"].(string)
			store, e := local.Open(f.dir)
			if e != nil {
				t.Fatal(e)
			}
			// Drive real committed reservations and real external commands until the
			// desired persistence boundary. The restart itself is the public daemon.
			runner := store.Executor()
			for {
				x, e := store.Reserve()
				if e != nil {
					t.Fatal(e)
				}
				if x == nil {
					time.Sleep(10 * time.Millisecond)
					continue
				}
				if x.Stage == "agent" {
					if saved {
						out, e := runner.Execute(context.Background(), *x)
						if e != nil {
							t.Fatal(e)
						}
						if e = store.Complete(*x, out, "finish_check"); e != nil {
							t.Fatal(e)
						}
					}
					break
				}
				out, e := runner.Execute(context.Background(), *x)
				if e != nil {
					t.Fatal(e)
				}
				next := "finish_check"
				switch x.Stage {
				case "start_check":
					next = "ready"
					if x.Phase == "claimed" {
						next = "before"
					}
				case "finish_check":
					next = "agent"
				}
				if e = store.Complete(*x, out, next); e != nil {
					t.Fatal(e)
				}
			}
			if e = store.Close(); e != nil {
				t.Fatal(e)
			}
			f.daemon(t)
			v := f.await(t, id)
			want := "blocked:outcome_unknown"
			if saved {
				want = "succeeded"
			}
			if v["stage"] != want || v["calls_used"] != float64(1) {
				t.Fatalf("restored boundary %v", v)
			}
			if !saved {
				assertUnidentifiedIntent(t, f, id)
			}
			n := 0
			for _, e := range orderEvents(t, f) {
				if e.RunID == id && e.Stage == "agent" {
					n++
				}
			}
			wantN := 0
			if saved {
				wantN = 1
			}
			if n != wantN {
				t.Fatalf("agent executions %d want%d", n, wantN)
			}
			var calls int
			var input string
			if e = f.database(t).QueryRow("SELECT r.calls_used,s.input FROM runs r JOIN submissions s ON s.id=r.submission_id WHERE r.id=?", id).Scan(&calls, &input); e != nil || calls != 1 || input != `{"label":"intent"}` {
				t.Fatalf("durable intent %d %s %v", calls, input, e)
			}
		})
	}
}

func assertUnidentifiedIntent(t *testing.T, f runtimeFixture, id string) {
	t.Helper()
	var held, owners, calls, pid, pgid int
	var result, boot, start string
	err := f.database(t).QueryRow(`SELECT rs.slot_held,(SELECT count(*) FROM resources WHERE run_id=r.id AND key='resource'),r.calls_used,coalesce(st.pid,0),coalesce(st.pgid,0),coalesce(st.boot_id,''),coalesce(st.process_start,''),json_extract(st.result,'$.kind') FROM runs r JOIN run_schedule rs ON rs.run_id=r.id JOIN steps st ON st.run_id=r.id WHERE r.id=? AND st.stage='agent'`, id).Scan(&held, &owners, &calls, &pid, &pgid, &boot, &start, &result)
	if err != nil || held != 1 || owners != 1 || calls != 1 || pid != 0 || pgid != 0 || boot != "" || start != "" || result != "process_unknown" {
		t.Fatalf("unidentified intent lost process/slot evidence held=%d owners=%d calls=%d pid=%d pgid=%d boot=%q start=%q result=%q err=%v", held, owners, calls, pid, pgid, boot, start, result, err)
	}
	same := submitCore(t, f, "runtime", "same-resource", "resource", map[string]any{"label": "same-resource"})
	waitReady(t, f, same["run_id"].(string))
	firstGate := filepath.Join(f.root, "unidentified-release")
	first := submitCore(t, f, "runtime", "independent-first", "independent-first", map[string]any{"label": "independent-first", "before_gate": firstGate})
	waitExternal(t, f, "independent-first", "before", 1)
	second := submitCore(t, f, "runtime", "independent-second", "independent-second", map[string]any{"label": "independent-second"})
	waitReady(t, f, second["run_id"].(string))
	// Both independent Runs are eligible. Only one physical interval can run
	// alongside the unidentified intent's still-reserved Run slot.
	time.Sleep(150 * time.Millisecond)
	view := f.call(t, "run", "show", second["run_id"].(string), "--json")
	if view["state"] != "waiting" || view["calls_used"] != float64(0) {
		t.Fatalf("unidentified intent slot reused %v", view)
	}
	for _, event := range orderEvents(t, f) {
		if (event.Label == "independent-second" || event.Label == "same-resource") && event.Stage != "start_check" {
			t.Fatalf("unidentified intent allowed extra changing interval %v", event)
		}
	}
	if err = f.database(t).QueryRow("SELECT count(*) FROM run_schedule WHERE slot_held=1").Scan(&held); err != nil || held != 2 {
		t.Fatalf("unidentified plus live Run slots %d %v", held, err)
	}
	writeTest(t, firstGate, nil, 0600)
	completedRuns(t, f, first, 1)
	completedRuns(t, f, second, 1)
	expected := []expectedOrderRun{{first["run_id"].(string), "independent-first", 1, append(runtimeStages(1), "after", "after_done")}, {second["run_id"].(string), "independent-second", 1, append(runtimeStages(1), "after", "after_done")}}
	assertOrderExecutions(t, f, expected)
	for _, event := range orderEvents(t, f) {
		if event.RunID == id && event.Stage == "agent" {
			t.Fatal("unissued agent created an external effect")
		}
		if event.Label == "same-resource" && event.Stage != "start_check" {
			t.Fatalf("unidentified resource released %v", event)
		}
	}
	if err = f.database(t).QueryRow("SELECT slot_held FROM run_schedule WHERE run_id=?", id).Scan(&held); err != nil || held != 1 {
		t.Fatalf("unidentified slot returned after unrelated work %d %v", held, err)
	}
}

func recheckFailures(t *testing.T) {
	for _, mode := range []string{"false", "error", "escape"} {
		t.Run(mode, func(t *testing.T) {
			f := recoveryRuntime(t)
			stop := escapedCleanup(t, f)
			a := submitCore(t, f, "runtime", "recheck", "resource", map[string]any{"label": "recheck", "recheck": mode, "escape_stop": stop})
			f.daemon(t)
			id := a["run_id"].(string)
			if mode == "false" {
				waitUntil(t, func() bool {
					var n int
					return f.database(t).QueryRow("SELECT count(*) FROM steps WHERE run_id=? AND stage='start_check' AND result IS NOT NULL", id).Scan(&n) == nil && n >= 2
				})
			} else {
				f.await(t, id)
			}
			// A further preflight can only run after the failed claim was durably released.
			v := f.call(t, "run", "show", id, "--json")
			want := "waiting"
			if mode == "error" {
				want = "blocked:check_error"
			}
			if mode == "escape" {
				want = "blocked:process_unknown"
			}
			if mode == "false" {
				if v["state"] != "waiting" {
					t.Fatalf("false recheck %v", v)
				}
			} else if v["stage"] != want {
				t.Fatalf("recheck %v", v)
			}
			var resources, slots int
			if e := f.database(t).QueryRow("SELECT (SELECT count(*) FROM resources WHERE run_id=?),(SELECT slot_held FROM run_schedule WHERE run_id=?)", id, id).Scan(&resources, &slots); e != nil {
				t.Fatal(e)
			}
			held := 0
			if mode == "escape" {
				held = 1
			}
			if resources != held || slots != held {
				t.Fatalf("recheck resource/slot %d %d want%d", resources, slots, held)
			}
			b := submitCore(t, f, "runtime", "other", "resource", map[string]any{"label": "other"})
			if mode == "escape" {
				waitReady(t, f, b["run_id"].(string))
				control := submitCore(t, f, "runtime", "control", "control", map[string]any{"label": "control"})
				completedRuns(t, f, control, 1)
			} else {
				completedRuns(t, f, b, 1)
			}
			for _, e := range orderEvents(t, f) {
				if e.Label == "recheck" && e.Stage != "start_check" {
					t.Fatalf("recheck performed change %v", e)
				}
				if mode == "escape" && e.Label == "other" && e.Stage != "start_check" {
					t.Fatalf("unconfirmed resource released %v", e)
				}
			}
			for _, name := range []string{"artifact", "published", "count"} {
				if _, e := os.Stat(filepath.Join(f.dir, "runs", id, name)); !os.IsNotExist(e) {
					t.Fatalf("recheck effect %s %v", name, e)
				}
			}
		})
	}
	missingWorkdirStages(t)
}
func changingFailures(t *testing.T) {
	for _, mode := range []string{"timeout", "signal"} {
		for _, stage := range []string{"before", "agent", "after"} {
			t.Run(mode+"-"+stage, func(t *testing.T) {
				f := recoveryRuntime(t)
				f.task["limits"] = map[string]string{stage + "_timeout": "200ms"}
				updateOrderTask(t, f)
				input := map[string]any{"label": "failed", mode + "_stage": stage}
				a := submitCore(t, f, "runtime", "failed", "resource", input)
				f.daemon(t)
				v := f.await(t, a["run_id"].(string))
				if v["stage"] != "blocked:outcome_unknown" {
					t.Fatalf("unobserved change considered definitive %v", v)
				}
				calls := float64(0)
				if stage != "before" {
					calls = 1
				}
				if v["calls_used"] != calls {
					t.Fatalf("slot changed %v", v)
				}
				steps := v["steps"].([]any)
				last := steps[len(steps)-1].(map[string]any)
				if last["stage"] != stage || last["result"].(map[string]any)["kind"] != "unknown" {
					t.Fatalf("unknown result %v", last)
				}
				var owner string
				if e := f.database(t).QueryRow("SELECT run_id FROM resources WHERE key='resource'").Scan(&owner); e != nil || owner != a["run_id"] {
					t.Fatalf("unknown effects resource released %s %v", owner, e)
				}
				if _, e := os.Stat(filepath.Join(f.dir, "runs", a["run_id"].(string), "published")); !os.IsNotExist(e) {
					t.Fatalf("failed step published %v", e)
				}
			})
		}
	}
	// The command claims success on stdout but the executable finish condition
	// remains false through the last available call.
	f := newRuntime(t)
	a := f.submit(t, map[string]any{"success_at": 4})
	f.daemon(t)
	v := f.await(t, a["run_id"].(string))
	assertRuntime(t, f, a, v, "failed:max_calls", 3, runtimeStages(3))
}

func readOnlyIntentRecovery(t *testing.T) {
	for _, stage := range []string{"start_check", "finish_check"} {
		t.Run(stage, func(t *testing.T) {
			f := recoveryRuntime(t)
			gate := filepath.Join(f.root, "check-release")
			a := submitCore(t, f, "runtime", "read-only", "resource", map[string]any{"label": "read-only", stage + "_gate": gate})
			id := a["run_id"].(string)
			cmd := crashDaemon(t, f)
			waitExternal(t, f, "read-only", stage, 1)
			var pid int
			waitUntil(t, func() bool {
				return f.database(t).QueryRow("SELECT pid FROM steps WHERE run_id=? AND stage=? AND result IS NULL", id, stage).Scan(&pid) == nil && pid > 0
			})
			t.Cleanup(func() {
				if processExists(pid) {
					_ = syscall.Kill(-pid, syscall.SIGKILL)
				}
			})
			killDaemon(t, cmd)
			// Leave the old command alive. Recovery must stop it before a new check.
			if !processExists(pid) {
				t.Fatal("fixture check exited before restart")
			}
			f.daemon(t)
			waitUntil(t, func() bool {
				var n int
				return f.database(t).QueryRow("SELECT count(*) FROM steps WHERE run_id=? AND stage=?", id, stage).Scan(&n) == nil && n >= 2
			})
			if processExists(pid) {
				t.Fatal("new check reserved before old process stopped")
			}
			writeTest(t, gate, nil, 0600)
			runs := completedRuns(t, f, a, 1)
			if runs[0]["stage"] != "succeeded" || runs[0]["calls_used"] != float64(1) {
				t.Fatalf("check recovery %v", runs)
			}
			checkN, agentN := 0, 0
			for _, event := range orderEvents(t, f) {
				if event.RunID == id && event.Stage == stage {
					checkN++
				}
				if event.RunID == id && event.Stage == "agent" {
					agentN++
				}
			}
			if checkN != 3 || agentN != 1 {
				t.Fatalf("replayed wrong steps checks=%d agents=%d", checkN, agentN)
			}
			var slots int
			if e := f.database(t).QueryRow("SELECT count(*) FROM check_slots").Scan(&slots); e != nil || slots != 0 {
				t.Fatalf("recovered check slot %d %v", slots, e)
			}
		})
	}
}
func recordedBlockSurvivesRestart(t *testing.T) {
	f := recoveryRuntime(t)
	a := submitCore(t, f, "runtime", "blocked", "resource", map[string]any{"label": "blocked", "signal_stage": "before"})
	id := a["run_id"].(string)
	cmd := crashDaemon(t, f)
	before := f.await(t, id)
	killDaemon(t, cmd)
	f.daemon(t)
	good := submitCore(t, f, "runtime", "good", "good", map[string]any{"label": "good"})
	completedRuns(t, f, good, 1)
	after := f.call(t, "run", "show", id, "--json")
	if after["stage"] != "blocked:outcome_unknown" || after["calls_used"] != float64(0) || len(after["steps"].([]any)) != len(before["steps"].([]any)) {
		t.Fatalf("restart cleared recorded block %v", after)
	}
	n := 0
	for _, event := range orderEvents(t, f) {
		if event.RunID == id && event.Stage == "before" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("recorded block replayed external command %d", n)
	}
}
