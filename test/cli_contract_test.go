package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestScenario_SC_31(t *testing.T) {
	verify(t, "V-01", taskLifecycle)
	verify(t, "V-02", taskLifecycleRejections)
}

func taskLifecycle(t *testing.T) {
	f := orderFixture(t, 1, "0s", "0s")
	f.task["id"] = "lifecycle"
	f.task["schedule"] = map[string]any{"every": "1s", "input_key": "scheduled", "concurrency_key": "scheduled", "input": map[string]any{"label": "scheduled"}}
	raw, _ := json.Marshal(f.task)
	path := filepath.Join(f.root, "lifecycle.json")
	writeTest(t, path, raw, 0600)
	first := f.call(t, "task", "register", path)
	if first["task_version"] != float64(1) || configCount(t, f, "SELECT count(*) FROM runs") != 0 {
		t.Fatal(first)
	}
	if show := f.call(t, "schedule", "show", "lifecycle", "--json"); show["enabled"] != false {
		t.Fatal(show)
	}
	if same := f.call(t, "task", "register", path); same["task_version"] != float64(1) {
		t.Fatal(same)
	}
	f.task["prompt"] = "updated task"
	raw, _ = json.Marshal(f.task)
	writeTest(t, path, raw, 0600)
	f.rejected(t, 3, "definition_conflict", "task", "register", path)
	f.rejected(t, 3, "version_conflict", "task", "update", path, "--if-version", "2")
	if update := f.call(t, "task", "update", path, "--if-version", "1"); update["task_version"] != float64(2) {
		t.Fatal(update)
	}
	items := []map[string]any{}
	for _, label := range []string{"A", "B"} {
		items = append(items, submitCore(t, f, "lifecycle", label, label, map[string]any{"label": label}))
	}
	before := f.durableAdmission(t)
	f.call(t, "schedule", "enable", "lifecycle")
	f.call(t, "task", "disable", "lifecycle")
	if f.durableAdmission(t) != before {
		t.Fatal("disable changed admitted work")
	}
	writeTest(t, f.input, []byte(`{"task_id":"lifecycle","input_key":"new","input":{},"concurrency_key":"new"}`), 0600)
	f.rejected(t, 6, "task_disabled", "run", "submit", f.input)
	f.call(t, "task", "enable", "lifecycle")
	if show := f.call(t, "schedule", "show", "lifecycle", "--json"); show["enabled"] != false {
		t.Fatal("enable reactivated schedule", show)
	}
	f.call(t, "schedule", "enable", "lifecycle")
	f.call(t, "schedule", "disable", "lifecycle")
	f.call(t, "schedule", "enable", "lifecycle")
	result := f.call(t, "task", "cancel", "lifecycle", "--reason", "stop all admitted work")
	if result["enabled"] != false || len(result["submission_ids"].([]any)) != 2 {
		t.Fatal(result)
	}
	for _, item := range items {
		view := f.call(t, "run", "show", item["run_id"].(string), "--json")
		if view["state"] != "cancelled" || view["cancel_requested"] != true || view["repeat_remaining"] != float64(0) || view["calls_used"] != float64(0) {
			t.Fatal(view)
		}
	}
	if view := f.call(t, "task", "show", "lifecycle", "--json"); view["enabled"] != false {
		t.Fatal("Task cancel did not disable intake", view)
	}
	if view := f.call(t, "schedule", "show", "lifecycle", "--json"); view["enabled"] != false {
		t.Fatal("Task cancel did not disable schedule", view)
	}
	f.call(t, "task", "enable", "lifecycle")
	stop := configDaemon(t, f)
	time.Sleep(1100 * time.Millisecond)
	if len(orderEvents(t, f)) != 0 {
		t.Fatal("reenable executed historical work")
	}
	for _, item := range items {
		if v := f.call(t, "run", "show", item["run_id"].(string), "--json"); v["state"] != "cancelled" {
			t.Fatal(v)
		}
	}
	stop()
}

func taskLifecycleRejections(t *testing.T) {
	f := orderFixture(t, 0, "0s", "0s")
	admitted := submitCore(t, f, "runtime", "keep", "keep", map[string]any{"label": "keep"})
	before, definitions := f.durableAdmission(t), parsingDefinitions(t, f.intakeFixture)
	for _, args := range [][]string{{"task", "cancel", "runtime", "--reason", " "}, {"task", "cancel", "runtime"}, {"task", "update", f.manifest, "--if-version", "0"}, {"task", "enable", "runtime", "extra"}} {
		f.rejected(t, 2, "validation_error", args...)
	}
	f.rejected(t, 4, "not_found", "task", "cancel", "missing", "--reason", "no task")
	f.rejected(t, 4, "not_found", "task", "enable", "missing")
	f.rejected(t, 3, "version_conflict", "task", "update", f.manifest, "--if-version", "99")
	if before != f.durableAdmission(t) || definitions != parsingDefinitions(t, f.intakeFixture) {
		t.Fatal("rejected Task operation mutated definitions or receipts")
	}
	if view := f.call(t, "task", "show", "runtime", "--json"); view["enabled"] != true {
		t.Fatal(view)
	}
	if view := f.call(t, "run", "show", admitted["run_id"].(string), "--json"); view["cancel_requested"] != false {
		t.Fatal(view)
	}
	if len(orderEvents(t, f)) != 0 {
		t.Fatal("CLI ran work")
	}
	// A failure while recording the second receipt must roll back the entire Task request.
	second := submitCore(t, f, "runtime", "second", "second", map[string]any{"label": "second"})
	before, definitions = f.durableAdmission(t), parsingDefinitions(t, f.intakeFixture)
	if _, err := f.database(t).Exec("CREATE TRIGGER reject_task_cancel BEFORE INSERT ON cancellation_audit WHEN NEW.submission_id='" + second["submission_id"].(string) + "' BEGIN SELECT RAISE(ABORT,'controlled audit failure'); END"); err != nil {
		t.Fatal(err)
	}
	f.rejected(t, 1, "storage_error", "task", "cancel", "runtime", "--reason", "atomic request")
	if before != f.durableAdmission(t) || definitions != parsingDefinitions(t, f.intakeFixture) {
		t.Fatal("failed Task cancellation partially committed")
	}
	if configCount(t, f, "SELECT count(*) FROM disabled_tasks") != 0 || configCount(t, f, "SELECT count(*) FROM cancel_requests") != 0 || configCount(t, f, "SELECT count(*) FROM cancellation_audit") != 0 {
		t.Fatal("failed Task cancellation left request or disabled state")
	}
	if _, err := f.database(t).Exec("DROP TRIGGER reject_task_cancel"); err != nil {
		t.Fatal(err)
	}
	otherTask := map[string]any{}
	for key, value := range f.task {
		otherTask[key] = value
	}
	otherTask["id"] = "unrelated"
	raw, _ := json.Marshal(otherTask)
	path := filepath.Join(f.root, "unrelated.json")
	writeTest(t, path, raw, 0600)
	f.call(t, "task", "register", path)
	other := submitCore(t, f, "unrelated", "keep", "keep-other", map[string]any{"label": "other"})
	f.call(t, "task", "cancel", "runtime", "--reason", "only this task")
	view := f.call(t, "run", "show", other["run_id"].(string), "--json")
	if view["state"] != "waiting" || view["cancel_requested"] != false || view["repeat_remaining"] != float64(0) {
		t.Fatal("Task cancellation changed another Task receipt", view)
	}
	if task := f.call(t, "task", "show", "unrelated", "--json"); task["enabled"] != true {
		t.Fatal(task)
	}
}

func TestScenario_SC_41(t *testing.T) {
	verify(t, "V-01", func(t *testing.T) { cliPathsAndErrors(t); cliObservationAndLogs(t); cliAdditionalCommands(t) })
	verify(t, "V-02", cliAbsentAndStaleDaemon)
}

type cliCall struct {
	args, env  []string
	cwd, stdin string
}

func cliOutput(t *testing.T, bin string, call cliCall) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, call.args...)
	cmd.Dir = call.cwd
	cmd.Env = append(append(os.Environ(), "GORACE=atexit_sleep_ms=0"), call.env...)
	cmd.Stdin = strings.NewReader(call.stdin)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	return out.String(), stderr.String(), code
}
func cliJSON(t *testing.T, bin string, call cliCall) map[string]any {
	t.Helper()
	out, stderr, code := cliOutput(t, bin, call)
	if code != 0 || stderr != "" {
		t.Fatalf("CLI %v: %d %s %s", call.args, code, out, stderr)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil || result["protocol_version"] != float64(1) {
		t.Fatalf("JSON result %s %v", out, err)
	}
	return result
}
func cliPathsAndErrors(t *testing.T) {
	f := newRuntime(t)
	root := t.TempDir()
	for _, xdg := range []bool{true, false} {
		home := filepath.Join(root, fmt.Sprint(xdg), "home")
		data := filepath.Join(root, fmt.Sprint(xdg), "xdg")
		expected := filepath.Join(data, "todoable")
		if !xdg {
			data = ""
			expected = filepath.Join(home, ".local/share/todoable")
		}
		call := cliCall{args: []string{"task", "register", "task.json"}, env: []string{"HOME=" + home, "XDG_DATA_HOME=" + data}, cwd: f.root}
		registered := cliJSON(t, f.bin, call)
		if registered["task_version"] != float64(1) {
			t.Fatal(registered)
		}
		info, err := os.Stat(expected)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("data permissions %v %v", info, err)
		}
		call.args = []string{"run", "submit", "-"}
		call.stdin = `{"task_id":"runtime","input_key":"stdin","input":{},"concurrency_key":"stdin"}`
		first := cliJSON(t, f.bin, call)
		sameSubmission(t, first, cliJSON(t, f.bin, call))
		for _, field := range []string{"submission_id", "task_version", "deduplicated", "state", "run_id"} {
			if _, ok := first[field]; !ok {
				t.Fatal("missing admission field", field)
			}
		}
		writeTest(t, filepath.Join(f.root, "relative.json"), []byte(strings.ReplaceAll(call.stdin, "stdin", "relative")), 0600)
		call.args = []string{"run", "submit", "relative.json"}
		call.stdin = ""
		cliJSON(t, f.bin, call)
	}
	explicit := filepath.Join(root, "explicit")
	cliJSON(t, f.bin, cliCall{args: []string{"--data-dir", explicit, "task", "register", f.manifest}, env: []string{"HOME=" + root, "XDG_DATA_HOME=" + root}})
	if info, err := os.Stat(explicit); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("explicit permissions %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(root, "todoable")); !os.IsNotExist(err) {
		t.Fatal("explicit path ignored")
	}
	for _, args := range [][]string{{"status", "--bad"}, {"run", "show", "missing", "--json", "--json"}, {"task", "show", "runtime", "--version", "0"}, {"logs"}, {"unknown", "x", "y"}} {
		f.rejected(t, 2, "validation_error", args...)
	}
	for _, args := range [][]string{{"task", "show", "missing", "--json"}, {"task", "show", "runtime", "--version", "99", "--json"}, {"run", "show", "missing", "--json"}, {"logs", "missing"}, {"status", "--task", "missing", "--json"}} {
		f.rejected(t, 4, "not_found", args...)
	}
	bad := filepath.Join(root, "not-a-directory")
	writeTest(t, bad, []byte("file"), 0600)
	out, stderr, code := cliOutput(t, f.bin, cliCall{args: []string{"--data-dir", bad, "status", "--json"}})
	var failure map[string]any
	err := json.Unmarshal([]byte(stderr), &failure)
	if code != 1 || out != "" || err != nil || failure["protocol_version"] != float64(1) || failure["error"] != "storage_error" || failure["message"] == "" {
		t.Fatalf("storage failure %d %s %s", code, out, stderr)
	}
	db := f.database(t)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("UPDATE tasks SET version=version"); err != nil {
		t.Fatal(err)
	}
	f.rejected(t, 5, "database_busy", "task", "enable", "runtime")
	_ = tx.Rollback()
	if events, err := os.ReadDir(f.records); err != nil || len(events) != 0 {
		t.Fatalf("CLI executed task %v %v", events, err)
	}
}

func cliHuman(t *testing.T, f runtimeFixture, args ...string) string {
	t.Helper()
	out, stderr, code := cliOutput(t, f.bin, cliCall{args: append([]string{"--data-dir", f.dir}, args...)})
	if code != 0 || stderr != "" || out == "" || json.Valid([]byte(out)) {
		t.Fatalf("human query %v: %d %s %s", args, code, out, stderr)
	}
	return out
}
func assertRunFields(t *testing.T, view map[string]any) {
	t.Helper()
	for _, field := range []string{"task_version", "input_key", "submission_id", "run_id", "state", "stage", "calls_used", "repeat", "repeat_remaining", "concurrency_key", "predecessor", "next_check_at", "scheduled_at", "last_check", "time_remaining", "cancel_requested", "blocked_reason"} {
		if _, ok := view[field]; !ok {
			t.Fatal("missing Run observation", field)
		}
	}
}
func cliObservationAndLogs(t *testing.T) {
	f := newRuntime(t)
	f.task["prompt"] = "second definition without a start condition"
	delete(f.task, "start")
	updateOrderTask(t, f)
	script := strings.Replace(orderScript, "gates=inp.get(s+'_gates',{})", "sys.stdout.buffer.write((s+' out\\n').encode()+b'\\xff');sys.stdout.flush();sys.stderr.write(s+' err\\n');sys.stderr.flush()\ngates=inp.get(s+'_gates',{})", 1)
	writeTest(t, filepath.Join(f.root, "runner.py"), []byte(script), 0700)
	// Expectations describe the two submitted definitions, independently of
	// TaskView and the currently mutable fixture map.
	for _, expected := range []struct {
		version, prompt string
		number          float64
		start           bool
	}{
		{"1", "literal {{input}} $HOME", 1, true},
		{"2", "second definition without a start condition", 2, false},
	} {
		view := f.call(t, "task", "show", "runtime", "--version", expected.version, "--json")
		definition := view["definition"].(map[string]any)
		if view["task_version"] != expected.number || view["current_version"] != float64(2) || definition["prompt"] != expected.prompt {
			t.Fatalf("selected Task definition differs from submitted version %s: %v", expected.version, view)
		}
		start, hasStart := definition["start"].(map[string]any)
		if hasStart != expected.start || (hasStart && start["poll_every"] != "1s") {
			t.Fatalf("selected Task start condition differs for version %s: %v", expected.version, definition)
		}
		raw, _ := json.Marshal(view)
		if bytes.Contains(raw, []byte("task-value")) || bytes.Contains(raw, []byte("submission-secret")) || definition["env"].(map[string]any)["OVERRIDE"] != "<redacted>" {
			t.Fatalf("Task environment exposed %s", raw)
		}
		human := cliHuman(t, f, "task", "show", "runtime", "--version", expected.version)
		if strings.Contains(human, "task-value") || !strings.Contains(human, "Enabled: true") || !strings.Contains(human, "Task: runtime (version "+expected.version+", current 2)") || !strings.Contains(human, expected.prompt) {
			t.Fatal(human)
		}
	}
	latest := f.call(t, "task", "show", "runtime", "--json")
	if latest["task_version"] != float64(2) || latest["current_version"] != float64(2) || latest["definition"].(map[string]any)["prompt"] != "second definition without a start condition" {
		t.Fatalf("default Task version does not select current definition: %v", latest)
	}
	gate := filepath.Join(f.root, "follow-release")
	first := submitCore(t, f, "runtime", "same", "A", map[string]any{"label": "A", "before_gate": gate})
	second := submitCore(t, f, "runtime", "same", "B", map[string]any{"label": "B"})
	firstID := first["run_id"].(string)
	view := f.call(t, "run", "show", second["run_id"].(string), "--json")
	assertRunFields(t, view)
	if view["predecessor"] != first["submission_id"] {
		t.Fatal("missing predecessor", view)
	}
	otherTask := map[string]any{}
	for key, value := range f.task {
		otherTask[key] = value
	}
	otherTask["id"] = "other-task"
	otherRaw, _ := json.Marshal(otherTask)
	otherPath := filepath.Join(f.root, "other-task.json")
	writeTest(t, otherPath, otherRaw, 0600)
	f.call(t, "task", "register", otherPath)
	other := submitCore(t, f, "other-task", "other", "other", map[string]any{"label": "other"})
	status := f.call(t, "status", "--task", "runtime", "--json")
	if len(status["runs"].([]any)) != 2 {
		t.Fatal(status)
	}
	for _, raw := range status["runs"].([]any) {
		assertRunFields(t, raw.(map[string]any))
		view := raw.(map[string]any)
		if view["task_id"] != "runtime" || view["task_version"] != float64(2) || view["input_key"] != "same" || view["state"] != "waiting" || view["stage"] != "waiting" || view["calls_used"] != float64(0) || view["repeat_remaining"] != float64(0) || view["cancel_requested"] != false || view["last_check"] != nil || view["scheduled_at"] != nil {
			t.Fatal("incorrect status receipt values", view)
		}
	}
	cliHuman(t, f, "run", "show", firstID)
	cliHuman(t, f, "status", "--task", "runtime")
	stop := configDaemon(t, f)
	f.rejected(t, 5, "daemon_running", "daemon")
	waitExternal(t, f, "A", "before", 1)
	active := f.call(t, "run", "show", firstID, "--json")
	step := currentStep(t, active)
	f.rejected(t, 4, "not_found", "logs", firstID, "--step", "missing")
	f.rejected(t, 4, "not_found", resumeArgs(firstID, "missing", "retry")...)
	f.rejected(t, 6, "invalid_state", resumeArgs(firstID, step, "retry")...)
	followPath := filepath.Join(f.root, "follow-output")
	output, err := os.Create(followPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(f.bin, "--data-dir", f.dir, "logs", firstID, "--follow")
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0")
	cmd.Stdout = output
	var followErr bytes.Buffer
	cmd.Stderr = &followErr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = output.Close() })
	waitUntil(t, func() bool {
		b, e := os.ReadFile(followPath)
		return e == nil && bytes.Contains(b, []byte("before err\n"))
	})
	writeTest(t, gate, nil, 0600)
	completedRuns(t, f, first, 1)
	completedRuns(t, f, second, 1)
	completedRuns(t, f, other, 1)
	select {
	case err = <-done:
		if err != nil {
			t.Fatalf("follow %v %s", err, &followErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("follow did not finish")
	}
	_ = output.Close()
	expected := ""
	for _, stage := range []string{"before", "finish_check", "agent", "finish_check", "after"} {
		expected += stage + " out\n\xff" + stage + " err\n"
	}
	followed, err := os.ReadFile(followPath)
	if err != nil || string(followed) != expected {
		t.Fatalf("follow bytes %q want %q %v", followed, expected, err)
	}
	out, stderr, code := cliOutput(t, f.bin, cliCall{args: []string{"--data-dir", f.dir, "logs", firstID}})
	if code != 0 || stderr != "" || out != expected {
		t.Fatalf("stored logs %d %q %s", code, out, stderr)
	}
	out, stderr, code = cliOutput(t, f.bin, cliCall{args: []string{"--data-dir", f.dir, "logs", firstID, "--step", step}})
	if code != 0 || stderr != "" || out != "before out\n\xffbefore err\n" {
		t.Fatalf("Step logs %d %q %s", code, out, stderr)
	}
	if len(f.call(t, "status", "--json")["runs"].([]any)) != 0 {
		t.Fatal("completed work included in pending status")
	}
	stop()
}

func cliAbsentAndStaleDaemon(t *testing.T) {
	f := orderFixture(t, 0, "0s", "0s")
	f.task["schedule"] = map[string]any{"every": "1s", "input_key": "scheduled", "concurrency_key": "scheduled", "input": map[string]any{"label": "scheduled"}}
	updateOrderTask(t, f)
	f.call(t, "schedule", "enable", "runtime")
	a := submitCore(t, f, "runtime", "pending", "pending", map[string]any{"label": "pending"})
	time.Sleep(1100 * time.Millisecond)
	if configCount(t, f, "SELECT count(*) FROM submissions") != 1 || len(orderEvents(t, f)) != 0 {
		t.Fatal("daemon-absent CLI published or executed work")
	}
	before := f.call(t, "status", "--json")["daemon"].(map[string]any)
	if before["state"] != "unknown" || before["observed_at"] != nil || before["stale"] != true {
		t.Fatal(before)
	}
	f.call(t, "schedule", "disable", "runtime")
	cmd := exec.Command(f.bin, "--data-dir", f.dir, "daemon")
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	completedRuns(t, f, a, 1)
	waitUntil(t, func() bool { return f.call(t, "status", "--json")["daemon"].(map[string]any)["state"] == "running" })
	freeze, err := f.database(t).Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = freeze.Exec("UPDATE daemon_observation SET state=state"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	_ = freeze.Rollback()
	observed := f.call(t, "status", "--json")["daemon"].(map[string]any)["observed_at"]
	time.Sleep(2200 * time.Millisecond)
	stale := f.call(t, "status", "--json")["daemon"].(map[string]any)
	if stale["state"] != "running" || stale["observed_at"] != observed || stale["stale"] != true {
		t.Fatal("stale state reported live", stale)
	}
	human := cliHuman(t, f, "status")
	if !strings.Contains(human, "Stale: true") || !strings.Contains(human, "does not prove current liveness") {
		t.Fatal(human)
	}
	if err := cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		return f.call(t, "status", "--json")["daemon"].(map[string]any)["observed_at"] != observed
	})
	// Observe reception by an already-running daemon; fixture work must start within its one-second poll contract.
	start := time.Now()
	b := submitCore(t, f, "runtime", "poll", "poll", map[string]any{"label": "poll"})
	waitExternal(t, f, "poll", "start_check", 1)
	if time.Since(start) > time.Second {
		t.Fatal("daemon exceeded one-second reception polling")
	}
	completedRuns(t, f, b, 1)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	final := f.call(t, "status", "--json")["daemon"].(map[string]any)
	if final["state"] != "stopped" {
		t.Fatal(final)
	}
}

func cliAdditionalCommands(t *testing.T) {
	f := orderFixture(t, 0, "0s", "0s")
	f.task["schedule"] = map[string]any{"every": "1s", "input_key": "manual-period", "concurrency_key": "manual-period", "input": map[string]any{"label": "period"}}
	updateOrderTask(t, f)
	f.call(t, "schedule", "enable", "runtime")
	show := f.call(t, "schedule", "show", "runtime", "--json")
	cliHuman(t, f, "schedule", "show", "runtime")
	anchor, err := time.Parse(time.RFC3339Nano, show["anchor"].(string))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	period := f.call(t, "schedule", "submit", "runtime", "--at", anchor.Add(time.Second).Format(time.RFC3339Nano))
	f.call(t, "schedule", "disable", "runtime")
	cancelled := f.call(t, "submission", "cancel", period["submission_id"].(string), "--reason", "cancel accepted period")
	if cancelled["state"] != "cancelled" || cancelled["cancel_requested"] != true {
		t.Fatal(cancelled)
	}
	f.call(t, "task", "disable", "runtime")
	f.call(t, "task", "enable", "runtime")
	blocked := submitCore(t, f, "runtime", "manual", "manual", map[string]any{"label": "manual", "block_before_run": 1})
	stop := configDaemon(t, f)
	view := f.await(t, blocked["run_id"].(string))
	if view["stage"] != "blocked:outcome_unknown" {
		t.Fatal(view)
	}
	resolved := f.call(t, resumeArgs(blocked["run_id"].(string), currentStep(t, view), "confirm-success")...)
	if resolved["run_id"] != blocked["run_id"] {
		t.Fatal(resolved)
	}
	completedRuns(t, f, blocked, 1)
	stop()
	f.call(t, "task", "cancel", "runtime", "--reason", "stop future intake")
	writeTest(t, f.input, []byte(`{"task_id":"runtime","input_key":"new","input":{},"concurrency_key":"new"}`), 0600)
	f.rejected(t, 6, "task_disabled", "run", "submit", f.input)
}
