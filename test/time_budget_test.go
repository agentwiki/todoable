package test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentwiki/todoable/internal/adapters/local"
	"github.com/agentwiki/todoable/internal/domain"
)

func runRemaining(t *testing.T, v map[string]any) time.Duration {
	t.Helper()
	text, ok := v["time_remaining"].(string)
	if !ok {
		t.Fatalf("missing remaining time %v", v)
	}
	n, err := time.ParseDuration(text)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func budgetExcludesWaiting(t *testing.T) {
	f := orderFixture(t, 1, "1200ms", "0s")
	f.task["limits"] = map[string]string{"run_timeout": "1s"}
	updateOrderTask(t, f)
	ga, gb := filepath.Join(f.root, "A-start"), filepath.Join(f.root, "B-start")
	ga2 := filepath.Join(f.root, "A-second-start")
	a := submitOrder(t, f, map[string]any{"label": "A", "start_check_gates": map[string]string{"1": ga, "2": ga2}}, "same")
	b := submitOrder(t, f, map[string]any{"label": "B", "start_check_gate": gb}, "same")
	f.daemon(t)
	waitExternal(t, f, "A", "start_check", 1)
	time.Sleep(1100 * time.Millisecond)
	v := f.call(t, "run", "show", a["run_id"].(string), "--json")
	if runRemaining(t, v) != time.Second || v["calls_used"] != float64(0) {
		t.Fatalf("start wait consumed execution budget %v", v)
	}
	v = f.call(t, "run", "show", b["run_id"].(string), "--json")
	if v["state"] != "waiting" || v["calls_used"] != float64(0) || len(v["steps"].([]any)) != 0 {
		t.Fatalf("predecessor wait performed work %v", v)
	}
	for _, e := range orderEvents(t, f) {
		if e.Label == "B" {
			t.Fatalf("predecessor wait executed command %v", e)
		}
	}
	writeTest(t, ga, nil, 0600)
	var second string
	waitUntil(t, func() bool {
		return f.database(t).QueryRow("SELECT id FROM runs WHERE submission_id=? AND run_seq=2", a["submission_id"]).Scan(&second) == nil
	})
	v = f.call(t, "run", "show", second, "--json")
	if v["state"] != "waiting" || v["calls_used"] != float64(0) || len(v["steps"].([]any)) != 0 {
		t.Fatalf("repeat delay consumed fresh Run time %v", v)
	}
	waitExternal(t, f, "A", "start_check", 2)
	v = f.call(t, "run", "show", second, "--json")
	if runRemaining(t, v) != time.Second {
		t.Fatalf("repeat delay consumed fresh budget %v", v)
	}
	writeTest(t, ga2, nil, 0600)
	ar := completedRuns(t, f, a, 2)
	waitExternal(t, f, "B", "start_check", 1)
	time.Sleep(1100 * time.Millisecond)
	if v = f.call(t, "run", "show", b["run_id"].(string), "--json"); runRemaining(t, v) != time.Second {
		t.Fatalf("predecessor plus start wait charged %v", v)
	}
	writeTest(t, gb, nil, 0600)
	br := completedRuns(t, f, b, 2)
	expected := []expectedOrderRun{}
	for i, runs := range [][]map[string]any{ar, br} {
		for j, v := range runs {
			if v["stage"] != "succeeded" || runRemaining(t, v) <= 0 || runRemaining(t, v) >= time.Second {
				t.Fatalf("excluded delay or execution charging %v", v)
			}
			expected = append(expected, expectedOrderRun{id: v["run_id"].(string), label: []string{"A", "B"}[i], seq: j + 1, stages: []string{"start_check", "start_check", "before", "finish_check", "agent", "finish_check", "after"}})
		}
	}
	assertOrderExecutions(t, f, expected)
}

// A persisted completed command followed by no live daemon still consumes Run
// time between stages; its result never becomes a new execution on restart.
func budgetBetweenSteps(t *testing.T) {
	f := recoveryRuntime(t)
	delete(f.task, "start")
	f.task["limits"] = map[string]string{"run_timeout": "600ms"}
	updateOrderTask(t, f)
	a := submitCore(t, f, "runtime", "gap", "resource", map[string]any{"label": "gap"})
	id := a["run_id"].(string)
	s, err := local.Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	x, err := s.Reserve()
	if err != nil || x == nil || x.Stage != "before" {
		t.Fatalf("before reserve %v %v", x, err)
	}
	out, err := s.Executor().Execute(context.Background(), *x)
	if err != nil || out.Kind != "exited" {
		t.Fatalf("before execution %v %v", out, err)
	}
	if err = s.Complete(*x, out, "finish_check"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	f.daemon(t)
	v := f.await(t, id)
	if v["stage"] != "failed:run_timeout" || runRemaining(t, v) != 0 || v["calls_used"] != float64(0) || len(v["steps"].([]any)) != 1 {
		t.Fatalf("inter-step outage reset budget/replayed Step %v", v)
	}
	events := orderEvents(t, f)
	if len(events) != 1 || events[0].Stage != "before" {
		t.Fatalf("commands executed after exhausted gap %v", events)
	}
	assertRunFiles(t, f, map[string]string{})
}
func budgetDelayedExec(t *testing.T) {
	f := recoveryRuntime(t)
	delete(f.task, "start")
	f.task["limits"] = map[string]string{"run_timeout": "300ms"}
	updateOrderTask(t, f)
	a := submitCore(t, f, "runtime", "delayed-exec", "resource", map[string]any{"label": "delayed-exec"})
	id := a["run_id"].(string)
	s, err := local.Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	x, err := s.Reserve()
	if err != nil || x == nil {
		t.Fatalf("reserve %v %v", x, err)
	}
	time.Sleep(400 * time.Millisecond)
	out, err := s.Executor().Execute(context.Background(), *x)
	if err != nil || out.Kind != "budget_exhausted" || out.PID != 0 {
		t.Fatalf("expired committed reservation executed %v %v", out, err)
	}
	if err = s.Complete(*x, out, domain.NextStage(*x, out)); err != nil {
		t.Fatal(err)
	}
	v := f.call(t, "run", "show", id, "--json")
	if v["stage"] != "failed:run_timeout" || runRemaining(t, v) != 0 || v["calls_used"] != float64(0) {
		t.Fatalf("expired intent completion %v", v)
	}
	if events := orderEvents(t, f); len(events) != 0 {
		t.Fatalf("external exec after exhausted reservation %v", events)
	}
	assertRunFiles(t, f, map[string]string{})
}

// A reservation delayed until only part of the budget remains must use that
// smaller duration, rather than its original per-command timeout.
func budgetDelayedLimitedExec(t *testing.T) {
	f := recoveryRuntime(t)
	delete(f.task, "start")
	f.task["limits"] = map[string]string{"run_timeout": "2s"}
	updateOrderTask(t, f)
	a := submitCore(t, f, "runtime", "limited-exec", "resource", map[string]any{"label": "limited-exec", "timeout_stage": "before"})
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
	time.Sleep(1500 * time.Millisecond)
	out, err := store.Executor().Execute(context.Background(), *x)
	if err != nil || out.Kind != "unknown" || out.PID <= 0 || out.ElapsedNS >= int64(1300*time.Millisecond) {
		t.Fatalf("command used stale reservation timeout %v %v", out, err)
	}
	if err = store.Complete(*x, out, domain.NextStage(*x, out)); err != nil {
		t.Fatal(err)
	}
	v := f.call(t, "run", "show", id, "--json")
	if v["stage"] != "blocked:outcome_unknown" || runRemaining(t, v) != 0 || v["calls_used"] != float64(0) || processExists(out.PID) {
		t.Fatalf("partial budget stop %v alive=%v", v, processExists(out.PID))
	}
	events := orderEvents(t, f)
	if len(events) != 1 || events[0].Stage != "before" {
		t.Fatalf("partial budget commands %v", events)
	}
	assertRunFiles(t, f, map[string]string{})
}

func budgetResumeQueue(t *testing.T) {
	f := manualFixture(t)
	delete(f.task, "start")
	f.task["limits"] = map[string]string{"run_timeout": "2s"}
	updateOrderTask(t, f)
	repair, gate := filepath.Join(f.root, "repair"), filepath.Join(f.root, "checks")
	a := submitCore(t, f, "runtime", "resume-budget", "resource", map[string]any{"label": "resume-budget", "signal_stage": "finish_check", "repair": repair})
	id := a["run_id"].(string)
	f.daemon(t)
	v := f.await(t, id)
	before := runRemaining(t, v)
	others := []map[string]any{}
	for _, label := range []string{"one", "two", "three", "four"} {
		others = append(others, submitCore(t, f, "other-task", label, label, map[string]any{"label": label, "start_check_gate": gate}))
		waitExternal(t, f, label, "start_check", 1)
	}
	if got := runRemaining(t, f.call(t, "run", "show", id, "--json")); got != before {
		t.Fatalf("confirmed block consumed time %v vs %v", got, before)
	}
	writeTest(t, repair, nil, 0600)
	f.call(t, resumeArgs(id, currentStep(t, v), "retry")...)
	time.Sleep(before + 100*time.Millisecond)
	v = f.call(t, "run", "show", id, "--json")
	if v["state"] != "waiting" || runRemaining(t, v) != 0 || len(v["steps"].([]any)) != 2 {
		t.Fatalf("resolved slot wait paused/reset budget %v", v)
	}
	writeTest(t, gate, nil, 0600)
	v = f.await(t, id)
	if v["stage"] != "failed:run_timeout" || v["calls_used"] != float64(0) || len(v["steps"].([]any)) != 2 {
		t.Fatalf("exhausted queue ran check %v", v)
	}
	for _, other := range others {
		completedRuns(t, f, other, 1)
	}
	counts := map[string]int{}
	for _, e := range orderEvents(t, f) {
		if e.RunID == id {
			counts[e.Stage]++
		}
	}
	if len(counts) != 2 || counts["before"] != 1 || counts["finish_check"] != 1 {
		t.Fatalf("replayed after queue exhaustion %v", counts)
	}
	for _, name := range []string{"artifact", "count", "published"} {
		if _, err := os.Stat(filepath.Join(f.dir, "runs", id, name)); !os.IsNotExist(err) {
			t.Fatalf("queue expiry produced %s %v", name, err)
		}
	}
}
func budgetStoppedAndOutage(t *testing.T) {
	f := manualFixture(t)
	f.task["limits"] = map[string]string{"run_timeout": "3s"}
	updateOrderTask(t, f)
	repair, gate := filepath.Join(f.root, "repair"), filepath.Join(f.root, "agent")
	a := submitCore(t, f, "runtime", "pause", "resource", map[string]any{"label": "pause", "signal_stage": "agent", "repair": repair, "agent_gate": gate})
	id := a["run_id"].(string)
	daemon := crashDaemon(t, f)
	v := f.await(t, id)
	before := runRemaining(t, v)
	killDaemon(t, daemon)
	time.Sleep(250 * time.Millisecond)
	daemon = crashDaemon(t, f)
	v = f.call(t, "run", "show", id, "--json")
	if runRemaining(t, v) != before || v["calls_used"] != float64(1) {
		t.Fatalf("confirmed block restart consumed/reset budget %v", v)
	}
	writeTest(t, repair, nil, 0600)
	f.call(t, resumeArgs(id, currentStep(t, v), "retry")...)
	waitUntil(t, func() bool {
		n := 0
		for _, e := range orderEvents(t, f) {
			if e.RunID == id && e.Stage == "agent" {
				n++
			}
		}
		return n == 2
	})
	killDaemon(t, daemon)
	time.Sleep(350 * time.Millisecond)
	f.daemon(t)
	v = f.await(t, id)
	remaining := runRemaining(t, v)
	if v["stage"] != "blocked:outcome_unknown" || v["calls_used"] != float64(2) || remaining > before-350*time.Millisecond || remaining <= 0 {
		t.Fatalf("outage reset calls/time %v before=%v", v, before)
	}
	time.Sleep(150 * time.Millisecond)
	if got := runRemaining(t, f.call(t, "run", "show", id, "--json")); got != remaining {
		t.Fatalf("confirmed recovery did not pause clock %v vs %v", got, remaining)
	}
	f.call(t, cancelArgs(a["submission_id"].(string), true, false)...)
	v = awaitCancellation(t, f, id)
	if v["state"] != "cancelled" || v["calls_used"] != float64(2) || runRemaining(t, v) != remaining {
		t.Fatalf("cancellation restored exhausted work budget %v", v)
	}
	assertRunFiles(t, f, map[string]string{})
}
func budgetZeroResolution(t *testing.T, stage, action string) {
	f := recoveryRuntime(t)
	f.task["limits"] = map[string]string{"run_timeout": "450ms"}
	updateOrderTask(t, f)
	a := submitCore(t, f, "runtime", "zero", "resource", map[string]any{"label": "zero", "timeout_stage": stage})
	id := a["run_id"].(string)
	daemon := crashDaemon(t, f)
	v := f.await(t, id)
	if v["stage"] != "blocked:outcome_unknown" || runRemaining(t, v) != 0 {
		t.Fatalf("Run limit did not exhaust changing command %v", v)
	}
	before := resumeDurableState(t, f, id)
	f.rejected(t, 6, "budget_exhausted", resumeArgs(id, currentStep(t, v), "retry")...)
	if got := resumeDurableState(t, f, id); got != before {
		t.Fatalf("zero retry changed durable state %s vs %s", got, before)
	}
	killDaemon(t, daemon)
	f.daemon(t)
	v = f.call(t, "run", "show", id, "--json")
	if v["stage"] != "blocked:outcome_unknown" || runRemaining(t, v) != 0 {
		t.Fatalf("restart cleared exhausted block %v", v)
	}
	stepN := len(v["steps"].([]any))
	eventN := len(orderEvents(t, f))
	want := "failed:run_timeout"
	if action == "cancel" {
		f.call(t, cancelArgs(a["submission_id"].(string), true, false)...)
		want = "cancelled"
	} else {
		args := resumeArgs(id, currentStep(t, v), action)
		if action == "confirm-failure" {
			args = append(args, "--exit-code", "17")
			want = "failed:" + stage
		} else if stage == "after" {
			want = "succeeded"
		}
		f.call(t, args...)
	}
	v = f.call(t, "run", "show", id, "--json")
	if v["stage"] != want || runRemaining(t, v) != 0 || len(v["steps"].([]any)) != stepN || len(orderEvents(t, f)) != eventN {
		t.Fatalf("zero-time resolution ran new work %v want %s", v, want)
	}
	calls := float64(0)
	if stage == "after" {
		calls = 1
	}
	if v["calls_used"] != calls {
		t.Fatalf("zero resolution reset calls %v", v)
	}
	if stage == "before" {
		assertRunFiles(t, f, map[string]string{})
	} else {
		assertRunFiles(t, f, map[string]string{filepath.Join(f.dir, "runs", id, "artifact"): "zero-1", filepath.Join(f.dir, "runs", id, "count"): "1"})
	}
}
func budgetUnconfirmedCheck(t *testing.T) {
	f := recoveryRuntime(t)
	f.task["limits"] = map[string]string{"run_timeout": "1800ms"}
	updateOrderTask(t, f)
	stop := escapedCleanup(t, f)
	a := submitCore(t, f, "runtime", "unknown-check", "resource", map[string]any{"label": "unknown-check", "escape_stage": "finish_check", "escape_stop": stop})
	id := a["run_id"].(string)
	daemon := crashDaemon(t, f)
	v := f.await(t, id)
	before := runRemaining(t, v)
	if v["stage"] != "blocked:process_unknown" || before <= 0 || before >= 1800*time.Millisecond {
		t.Fatalf("unknown check fixture %v", v)
	}
	time.Sleep(before + 100*time.Millisecond)
	v = f.call(t, "run", "show", id, "--json")
	if runRemaining(t, v) != 0 {
		t.Fatalf("unconfirmed stopped clock %v", v)
	}
	killDaemon(t, daemon)
	f.daemon(t)
	v = f.call(t, "run", "show", id, "--json")
	f.rejected(t, 6, "process_unknown", resumeArgs(id, currentStep(t, v), "retry")...)
	f.rejected(t, 6, "budget_exhausted", append(resumeArgs(id, currentStep(t, v), "retry"), "--processes-stopped")...)
	count := len(v["steps"].([]any))
	f.call(t, cancelArgs(a["submission_id"].(string), true, false)...)
	v = f.call(t, "run", "show", id, "--json")
	if v["state"] != "blocked" || runRemaining(t, v) != 0 || len(v["steps"].([]any)) != count {
		t.Fatalf("unknown check resumed/cancelled without stop %v", v)
	}
	writeTest(t, stop, nil, 0600)
	time.Sleep(100 * time.Millisecond)
	f.call(t, cancelArgs(a["submission_id"].(string), false, true)...)
	v = awaitCancellation(t, f, id)
	if v["state"] != "cancelled" || runRemaining(t, v) != 0 || len(v["steps"].([]any)) != count || v["calls_used"] != float64(0) {
		t.Fatalf("zero check cancellation %v", v)
	}
	for _, e := range orderEvents(t, f) {
		if e.RunID == id && (e.Stage == "agent" || e.Stage == "after") {
			t.Fatalf("unknown check continued work %v", e)
		}
	}
	assertRunFiles(t, f, map[string]string{})
}
func timeBudgetContinuity(t *testing.T) {
	t.Run("waiting", budgetExcludesWaiting)
	t.Run("stage-gap", budgetBetweenSteps)
	t.Run("delayed-exec", budgetDelayedExec)
	t.Run("delayed-limited-exec", budgetDelayedLimitedExec)
	t.Run("resume-queue", budgetResumeQueue)
	t.Run("pause-outage", budgetStoppedAndOutage)
}
func timeBudgetExhaustion(t *testing.T) {
	for _, stage := range []string{"before", "after"} {
		for _, action := range []string{"confirm-success", "confirm-failure", "cancel"} {
			t.Run(fmt.Sprintf("zero-%s-%s", stage, action), func(t *testing.T) { budgetZeroResolution(t, stage, action) })
		}
	}
	t.Run("unknown-check", budgetUnconfirmedCheck)
}
