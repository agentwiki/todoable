package test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentwiki/todoable/internal/adapters/local"
	"github.com/agentwiki/todoable/internal/domain"
)

func cancelArgs(id string, ack, stopped bool) []string {
	args := []string{"submission", "cancel", id, "--reason", "destination checked; stop this input"}
	if ack {
		args = append(args, "--acknowledge-effects")
	}
	if stopped {
		args = append(args, "--processes-stopped")
	}
	return args
}
func awaitCancellation(t *testing.T, f runtimeFixture, id string) map[string]any {
	t.Helper()
	var v map[string]any
	waitUntil(t, func() bool {
		v = f.call(t, "run", "show", id, "--json")
		return v["state"] == "cancelled" || v["state"] == "blocked" && v["cancel_requested"] == true
	})
	return v
}
func uncertainAfterFixture(t *testing.T) runtimeFixture {
	t.Helper()
	f := manualFixture(t)
	p := filepath.Join(f.root, "runner.py")
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	injection := `if s=='after' and inp.get('uncertain_after'):
 open(os.path.join(rd,'published'),'a').write(label+'-'+str(seq)+'\n')
 while not os.path.exists(inp['after_release']):time.sleep(.01)
 sys.exit(0)
`
	writeTest(t, p, []byte(strings.Replace(string(b), "gates=inp.get(s+'_gates',{})", injection+"gates=inp.get(s+'_gates',{})", 1)), 0700)
	return f
}
func afterEffectRecovery(t *testing.T, action string) {
	t.Helper()
	f := uncertainAfterFixture(t)
	a := submitCore(t, f, "runtime", "posted", "resource", map[string]any{"label": "posted", "uncertain_after": true, "after_release": filepath.Join(f.root, "after-release")})
	id := a["run_id"].(string)
	cmd := crashDaemon(t, f)
	posted := filepath.Join(f.dir, "runs", id, "published")
	waitUntil(t, func() bool { b, e := os.ReadFile(posted); return e == nil && string(b) == "posted-1\n" })
	killDaemon(t, cmd)
	f.daemon(t)
	v := f.await(t, id)
	if v["stage"] != "blocked:outcome_unknown" {
		t.Fatalf("after effect not blocked %v", v)
	}
	b := submitCore(t, f, "runtime", "successor", "resource", map[string]any{"label": "successor"})
	waitReady(t, f, b["run_id"].(string))
	good := submitCore(t, f, "runtime", "control", "control", map[string]any{"label": "control"})
	completedRuns(t, f, good, 1)
	for _, e := range orderEvents(t, f) {
		if e.Label == "successor" && e.Stage != "start_check" {
			t.Fatalf("unconfirmed after released resource %v", e)
		}
	}
	if action == "" {
		return
	}
	want := "succeeded"
	if action == "confirm" {
		f.call(t, resumeArgs(id, currentStep(t, v), "confirm-success")...)
		v = f.await(t, id)
	} else {
		f.call(t, cancelArgs(a["submission_id"].(string), true, false)...)
		v = awaitCancellation(t, f, id)
		want = "cancelled"
	}
	if v["stage"] != want || v["calls_used"] != float64(1) {
		t.Fatalf("after resolution %v", v)
	}
	if want == "cancelled" && (v["effects_unknown"] != true || v["cancel_reason"] != "destination checked; stop this input") {
		t.Fatalf("effect acknowledgement lost %v", v)
	}
	completedRuns(t, f, b, 1)
	raw, e := os.ReadFile(posted)
	if e != nil || string(raw) != "posted-1\n" {
		t.Fatalf("publication replayed/lost %q %v", raw, e)
	}
	count := 0
	for _, event := range orderEvents(t, f) {
		if event.RunID == id && event.Stage == "after" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("after automatically replayed %d", count)
	}
}
func cancelledUnknownResolution(t *testing.T, confirm bool) {
	t.Helper()
	f := manualFixture(t)
	f.task["repeat"] = 2
	updateOrderTask(t, f)
	repair := filepath.Join(f.root, "repair")
	a := submitCore(t, f, "runtime", "cancel", "resource", map[string]any{"label": "cancel", "signal_stage": "agent", "repair": repair})
	id := a["run_id"].(string)
	f.daemon(t)
	v := f.await(t, id)
	if v["stage"] != "blocked:outcome_unknown" {
		t.Fatalf("agent uncertainty not recorded %v", v)
	}
	f.call(t, cancelArgs(a["submission_id"].(string), false, false)...)
	v = awaitCancellation(t, f, id)
	if v["stage"] != "blocked:outcome_unknown" || v["repeat_remaining"] != float64(0) {
		t.Fatalf("unknown cancellation prematurely completed %v", v)
	}
	f.rejected(t, 6, "cancel_requested", resumeArgs(id, currentStep(t, v), "retry")...)
	pending := submitCore(t, f, "runtime", "successor", "resource", map[string]any{"label": "successor"})
	waitReady(t, f, pending["run_id"].(string))
	if confirm {
		args := append(resumeArgs(id, currentStep(t, v), "confirm-failure"), "--exit-code", "17")
		f.call(t, args...)
	} else {
		f.call(t, cancelArgs(a["submission_id"].(string), true, true)...)
	}
	v = awaitCancellation(t, f, id)
	if v["state"] != "cancelled" || v["calls_used"] != float64(1) || len(v["runs"].([]any)) != 1 {
		t.Fatalf("resolved cancellation %v", v)
	}
	if !confirm {
		if v["effects_unknown"] != true {
			t.Fatalf("unknown effects not retained %v", v)
		}
		audits := v["cancellations"].([]any)
		last := audits[len(audits)-1].(map[string]any)
		if last["processes_stopped"] != true || last["acknowledge_effects"] != true || last["reason"] != "destination checked; stop this input" {
			t.Fatalf("user stop/effect audit lost %v", last)
		}
	}
	completedRuns(t, f, pending, 3)
	agents := 0
	for _, event := range orderEvents(t, f) {
		if event.RunID == id && event.Stage == "agent" {
			agents++
		}
		if event.RunID == id && (event.Stage == "after" || event.Stage == "agent" && event.Seq != 1) {
			t.Fatalf("cancellation launched new work %v", event)
		}
	}
	if agents != 1 {
		t.Fatalf("cancelled agent replayed outside stored calls: %d", agents)
	}
	for _, name := range []string{"artifact", "published", "count"} {
		if _, e := os.Stat(filepath.Join(f.dir, "runs", id, name)); !os.IsNotExist(e) {
			t.Fatalf("cancelled unknown command had new effect %s %v", name, e)
		}
	}
}
func unconfirmedCancellation(t *testing.T) {
	t.Helper()
	f := recoveryRuntime(t)
	stop := escapedCleanup(t, f)
	a := submitCore(t, f, "runtime", "escape", "resource", map[string]any{"label": "escape", "escape_stage": "before", "escape_stop": stop})
	id := a["run_id"].(string)
	f.daemon(t)
	v := f.await(t, id)
	if v["stage"] != "blocked:process_unknown" {
		t.Fatalf("fixture process not unknown %v", v)
	}
	f.call(t, cancelArgs(a["submission_id"].(string), true, false)...)
	v = awaitCancellation(t, f, id)
	if v["state"] != "blocked" || v["cancel_requested"] != true {
		t.Fatalf("acknowledgement replaced stop evidence %v", v)
	}
	pending := submitCore(t, f, "runtime", "pending", "resource", map[string]any{"label": "pending"})
	waitReady(t, f, pending["run_id"].(string))
	good := submitCore(t, f, "runtime", "good", "good", map[string]any{"label": "good"})
	completedRuns(t, f, good, 1)
	var held, owners int
	if e := f.database(t).QueryRow("SELECT (SELECT slot_held FROM run_schedule WHERE run_id=?),(SELECT count(*) FROM resources WHERE run_id=?)", id, id).Scan(&held, &owners); e != nil || held != 1 || owners != 1 {
		t.Fatalf("unconfirmed cancellation released ownership %d %d %v", held, owners, e)
	}
	for _, event := range orderEvents(t, f) {
		if event.Label == "pending" && event.Stage != "start_check" {
			t.Fatalf("ack alone started successor %v", event)
		}
	}
	writeTest(t, stop, nil, 0600)
	time.Sleep(100 * time.Millisecond)
	f.call(t, cancelArgs(a["submission_id"].(string), true, true)...)
	v = awaitCancellation(t, f, id)
	if v["state"] != "cancelled" || v["effects_unknown"] != true {
		t.Fatalf("confirmed isolation did not cancel %v", v)
	}
	completedRuns(t, f, pending, 1)
}

// A real command completes, but the test holds its result delivery to choose
// transaction order. Independent CLI cancellation and subsequent daemon startup
// cover both sides of the durable boundary.
func cancellationCompletionOrder(t *testing.T, completeFirst bool) {
	t.Helper()
	f := recoveryRuntime(t)
	delete(f.task, "start")
	f.task["repeat"] = 1
	updateOrderTask(t, f)
	a := submitCore(t, f, "runtime", "race", "resource", map[string]any{"label": "race"})
	id := a["run_id"].(string)
	store, e := local.Open(f.dir)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	runner := store.Executor()
	var last domain.Execution
	var outcome domain.Outcome
	for {
		x, e := store.Reserve()
		if e != nil {
			t.Fatal(e)
		}
		if x == nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		out, e := runner.Execute(context.Background(), *x)
		if e != nil {
			t.Fatal(e)
		}
		if x.Stage == "after" {
			last = *x
			outcome = out
			break
		}
		if e = store.Complete(*x, out, domain.NextStage(*x, out)); e != nil {
			t.Fatal(e)
		}
	}
	if completeFirst {
		if e = store.Complete(last, outcome, "succeeded"); e != nil {
			t.Fatal(e)
		}
	}
	f.call(t, cancelArgs(a["submission_id"].(string), false, false)...)
	if !completeFirst {
		v := f.call(t, "run", "show", id, "--json")
		if v["state"] == "cancelled" {
			t.Fatal("cancel completed before reserved Step result")
		}
		if e = store.Complete(last, outcome, "succeeded"); e != nil {
			t.Fatal(e)
		}
	}
	before := f.call(t, "run", "show", id, "--json")
	if e = store.Complete(last, domain.Outcome{Kind: "exited", ExitCode: 99}, "failed:after"); e != nil {
		t.Fatalf("late completion crashed control loop %v", e)
	}
	after := f.call(t, "run", "show", id, "--json")
	if after["stage"] != before["stage"] || len(after["steps"].([]any)) != len(before["steps"].([]any)) {
		t.Fatalf("late completion overwrote state %v", after)
	}
	var diagnostics int
	if e = f.database(t).QueryRow("SELECT count(*) FROM runtime_diagnostics WHERE step_id=?", last.StepID).Scan(&diagnostics); e != nil || diagnostics != 1 {
		t.Fatalf("late result not diagnosed %d %v", diagnostics, e)
	}
	runs := after["runs"].([]any)
	want := 1
	if completeFirst {
		want = 2
	}
	if len(runs) != want {
		t.Fatalf("cancellation round race %v", runs)
	}
	for i, item := range runs {
		run := item.(map[string]any)
		expected := "cancelled"
		if completeFirst && i == 0 {
			expected = "succeeded"
		}
		if run["state"] != expected {
			t.Fatalf("cancel/completion transaction order %v", runs)
		}
	}
	f.daemon(t)
	good := submitCore(t, f, "runtime", "control", "control", map[string]any{"label": "control"})
	completedRuns(t, f, good, 2)
	for _, event := range orderEvents(t, f) {
		if event.Label == "race" && event.Seq != 1 {
			t.Fatalf("cancelled repeat executed %v", event)
		}
	}
	raw, e := os.ReadFile(filepath.Join(f.dir, "runs", id, "published"))
	if e != nil || string(raw) != "race-1\n" {
		t.Fatalf("completed external effect lost %q %v", raw, e)
	}
}
func cancelBeforeExec(t *testing.T) {
	t.Helper()
	f := recoveryRuntime(t)
	delete(f.task, "start")
	updateOrderTask(t, f)
	a := submitCore(t, f, "runtime", "unstarted", "resource", map[string]any{"label": "unstarted"})
	id := a["run_id"].(string)
	store, e := local.Open(f.dir)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	x, e := store.Reserve()
	if e != nil || x == nil || x.Stage != "before" {
		t.Fatalf("intent boundary %v %v", x, e)
	}
	f.call(t, cancelArgs(a["submission_id"].(string), false, false)...)
	out, e := store.Executor().Execute(context.Background(), *x)
	if e != nil || out.Kind != "not_started" {
		t.Fatalf("cancelled reservation executed %v %v", out, e)
	}
	if e = store.Complete(*x, out, domain.NextStage(*x, out)); e != nil {
		t.Fatal(e)
	}
	v := awaitCancellation(t, f, id)
	if v["state"] != "cancelled" || v["calls_used"] != float64(0) {
		t.Fatalf("unstarted cancellation %v", v)
	}
	if events := orderEvents(t, f); len(events) != 0 {
		t.Fatalf("command started after cancellation %v", events)
	}
	next, e := store.Reserve()
	if e != nil || next != nil {
		t.Fatalf("new reservation after cancellation %v %v", next, e)
	}
}
func cancelActiveCommand(t *testing.T) {
	t.Helper()
	f := recoveryRuntime(t)
	f.task["repeat"] = 1
	updateOrderTask(t, f)
	a := submitCore(t, f, "runtime", "active", "resource", map[string]any{"label": "active", "agent_gate": filepath.Join(f.root, "agent-release")})
	id := a["run_id"].(string)
	f.daemon(t)
	waitExternal(t, f, "active", "agent", 1)
	var pid int
	waitUntil(t, func() bool {
		return f.database(t).QueryRow("SELECT pid FROM steps WHERE run_id=? AND stage='agent'", id).Scan(&pid) == nil && pid > 0
	})
	f.call(t, cancelArgs(a["submission_id"].(string), false, false)...)
	v := awaitCancellation(t, f, id)
	if v["stage"] != "blocked:outcome_unknown" || processExists(pid) {
		t.Fatalf("active cancellation did not stop/block %v alive=%v", v, processExists(pid))
	}
	f.call(t, append(resumeArgs(id, currentStep(t, v), "confirm-failure"), "--exit-code", "17")...)
	v = awaitCancellation(t, f, id)
	if v["state"] != "cancelled" || len(v["runs"].([]any)) != 1 || v["calls_used"] != float64(1) {
		t.Fatalf("active cancelled result %v", v)
	}
	for _, event := range orderEvents(t, f) {
		if event.RunID == id && event.Stage == "after" {
			t.Fatalf("cancellation ran after %v", event)
		}
	}
}

// The execution is real; only the ownership handoff is injected to deliver an
// old completion while its result is still NULL. A duplicate result alone would
// not exercise the owner/state/Step guards.
func cancellationStaleOwner(t *testing.T, changed string) {
	t.Helper()
	f := recoveryRuntime(t)
	delete(f.task, "start")
	updateOrderTask(t, f)
	a := submitCore(t, f, "runtime", "stale", "resource", map[string]any{"label": "stale"})
	id := a["run_id"].(string)
	store, err := local.Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	x, err := store.Reserve()
	if err != nil || x == nil || x.Stage != "before" {
		t.Fatalf("reserve %v %v", x, err)
	}
	out, err := store.Executor().Execute(context.Background(), *x)
	if err != nil || out.Kind != "exited" {
		t.Fatalf("execute %v %v", out, err)
	}
	f.call(t, cancelArgs(a["submission_id"].(string), false, false)...)
	query := "UPDATE runtime SET owner_version=owner_version+1 WHERE run_id=?"
	if changed == "stage" {
		query = "UPDATE runtime SET stage='agent' WHERE run_id=?"
	}
	if changed == "state" {
		query = "UPDATE runs SET state='blocked' WHERE id=?"
	}
	if _, err = f.database(t).Exec(query, id); err != nil {
		t.Fatal(err)
	}
	before := resumeDurableState(t, f, id)
	if err = store.Complete(*x, out, domain.NextStage(*x, out)); err != nil {
		t.Fatal(err)
	}
	if after := resumeDurableState(t, f, id); after != before {
		t.Fatalf("stale %s completion changed durable state\nbefore %s\nafter %s", changed, before, after)
	}
	var diagnostics int
	if err = f.database(t).QueryRow("SELECT count(*) FROM runtime_diagnostics WHERE step_id=?", x.StepID).Scan(&diagnostics); err != nil || diagnostics != 1 {
		t.Fatalf("stale diagnostic %d %v", diagnostics, err)
	}
	if next, err := store.Reserve(); err != nil || next != nil {
		t.Fatalf("cancelled stale ownership reserved work %v %v", next, err)
	}
	events := orderEvents(t, f)
	if len(events) != 1 || events[0].Stage != "before" {
		t.Fatalf("unexpected external work %v", events)
	}
	for _, name := range []string{"artifact", "published", "count"} {
		if _, err := os.Stat(filepath.Join(f.dir, "runs", id, name)); !os.IsNotExist(err) {
			t.Fatalf("stale completion caused effect %s %v", name, err)
		}
	}
}

// Exactly one CLI request must finish through the daemon's reconciliation loop.
// The external TERM gate separates request persistence from confirmed stopping.
func cancelActiveAcknowledged(t *testing.T) {
	t.Helper()
	f := recoveryRuntime(t)
	f.task["repeat"] = 1
	updateOrderTask(t, f)
	requested, release := filepath.Join(f.root, "term-requested"), filepath.Join(f.root, "allow-stop")
	script := filepath.Join(f.root, "runner.py")
	raw, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	injection := `if s=='agent' and inp.get('active_effect'):
 open(os.path.join(rd,'effect-once'),'a').write(label+'-'+str(seq)+'\n')
 def hold_term(signum,frame):
  open(inp['term_requested'],'w').write('TERM')
  while not os.path.exists(inp['allow_stop']):time.sleep(.01)
  sys.exit(143)
 signal.signal(signal.SIGTERM,hold_term)
`
	writeTest(t, script, []byte(strings.Replace(string(raw), "gates=inp.get(s+'_gates',{})", injection+"gates=inp.get(s+'_gates',{})", 1)), 0700)
	a := submitCore(t, f, "runtime", "active-ack", "resource", map[string]any{"label": "active-ack", "active_effect": true, "term_requested": requested, "allow_stop": release, "agent_gate": filepath.Join(f.root, "never-release-agent")})
	id := a["run_id"].(string)
	f.daemon(t)
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	effect := filepath.Join(f.dir, "runs", id, "effect-once")
	waitUntil(t, func() bool { b, e := os.ReadFile(effect); return e == nil && string(b) == "active-ack-1\n" })
	var pid int
	waitUntil(t, func() bool {
		return f.database(t).QueryRow("SELECT pid FROM steps WHERE run_id=? AND stage='agent'", id).Scan(&pid) == nil && pid > 0
	})
	answer := f.call(t, cancelArgs(a["submission_id"].(string), true, false)...)
	if answer["state"] != "active" || answer["cancel_requested"] != true {
		t.Fatalf("request claimed stopping complete %v", answer)
	}
	waitUntil(t, func() bool { b, e := os.ReadFile(requested); return e == nil && string(b) == "TERM" })
	pending := submitCore(t, f, "runtime", "successor", "resource", map[string]any{"label": "successor"})
	waitReady(t, f, pending["run_id"].(string))
	v := f.call(t, "run", "show", id, "--json")
	if v["state"] != "running" || v["cancel_requested"] != true || !processExists(pid) {
		t.Fatalf("ack completed before process stop %v alive=%v", v, processExists(pid))
	}
	var held, resources, inflight int
	if err = f.database(t).QueryRow("SELECT (SELECT slot_held FROM run_schedule WHERE run_id=?),(SELECT count(*) FROM resources WHERE run_id=?),(SELECT count(*) FROM steps WHERE run_id=? AND result IS NULL)", id, id, id).Scan(&held, &resources, &inflight); err != nil || held != 1 || resources != 1 || inflight != 1 {
		t.Fatalf("active ack released reservation %d %d %d %v", held, resources, inflight, err)
	}
	for _, e := range orderEvents(t, f) {
		if e.Label == "successor" && e.Stage != "start_check" {
			t.Fatalf("successor ran before acknowledged process stopped %v", e)
		}
	}
	writeTest(t, release, nil, 0600)
	// No second Cancel and no Resume: only background reconciliation can settle.
	waitUntil(t, func() bool { v = f.call(t, "run", "show", id, "--json"); return v["state"] == "cancelled" })
	if processExists(pid) || v["stage"] != "cancelled" || v["effects_unknown"] != true || v["calls_used"] != float64(1) || v["repeat_remaining"] != float64(0) || len(v["runs"].([]any)) != 1 {
		t.Fatalf("asynchronous acknowledged cancellation %v alive=%v", v, processExists(pid))
	}
	audits := v["cancellations"].([]any)
	if len(audits) != 1 || len(v["resolutions"].([]any)) != 0 {
		t.Fatalf("single request replaced by another action %v", v)
	}
	audit := audits[0].(map[string]any)
	if audit["reason"] != "destination checked; stop this input" || audit["acknowledge_effects"] != true || audit["processes_stopped"] != false || v["cancel_reason"] != audit["reason"] {
		t.Fatalf("acknowledgement audit %v", v)
	}
	steps := v["steps"].([]any)
	last := steps[len(steps)-1].(map[string]any)
	if last["stage"] != "agent" || last["result"].(map[string]any)["kind"] != "unknown" {
		t.Fatalf("ack overwrote observed interrupted result %v", last)
	}
	var checks int
	if err = f.database(t).QueryRow("SELECT (SELECT slot_held FROM run_schedule WHERE run_id=?),(SELECT count(*) FROM resources WHERE run_id=?),(SELECT count(*) FROM check_slots WHERE step_id IN(SELECT id FROM steps WHERE run_id=?))", id, id, id).Scan(&held, &resources, &checks); err != nil || held != 0 || resources != 0 || checks != 0 {
		t.Fatalf("settled cancellation retained ownership %d %d %d %v", held, resources, checks, err)
	}
	completedRuns(t, f, pending, 2)
	agents := 0
	for _, e := range orderEvents(t, f) {
		if e.RunID == id {
			if e.Stage == "agent" {
				agents++
			}
			if e.Stage == "after" || e.Seq != 1 {
				t.Fatalf("cancelled request continued work %v", e)
			}
		}
	}
	if agents != 1 {
		t.Fatalf("acknowledged agent replayed %d", agents)
	}
	b, err := os.ReadFile(effect)
	if err != nil || string(b) != "active-ack-1\n" {
		t.Fatalf("pre-stop external effect changed %q %v", b, err)
	}
	for _, name := range []string{"artifact", "count", "published"} {
		if _, err := os.Stat(filepath.Join(f.dir, "runs", id, name)); !os.IsNotExist(err) {
			t.Fatalf("cancelled agent finished hidden work %s %v", name, err)
		}
	}
}
