package test

import (
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

func TestScenario_SC_40(t *testing.T) {
	verify(t, "V-01", func(t *testing.T) {
		configConcurrentClients(t)
		configRuntimeChange(t)
		configCheckLimit(t)
		configCompletedLogRestart(t, false)
		configCompletedLogRestart(t, true)
	})
	verify(t, "V-02", func(t *testing.T) { configAdmissionChange(t) })
}
func configDaemon(t *testing.T, f runtimeFixture) func() {
	t.Helper()
	stderr, e := os.CreateTemp(f.root, "config-daemon-")
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(f.bin, "--data-dir", f.dir, "daemon")
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0")
	cmd.Stderr = stderr
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case e := <-done:
			if e != nil {
				b, _ := os.ReadFile(stderr.Name())
				t.Errorf("daemon: %v %s", e, b)
			}
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			t.Error("daemon did not stop")
		}
		_ = stderr.Close()
	}
	t.Cleanup(stop)
	waitUntil(t, func() bool {
		lock, e := os.OpenFile(filepath.Join(f.dir, "daemon.lock"), os.O_RDWR, 0600)
		if e != nil {
			return false
		}
		defer func() { _ = lock.Close() }()
		e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if e == nil {
			return false
		}
		b, e := os.ReadFile(filepath.Join(f.dir, "daemon.lock"))
		return e == nil && len(b) == 64
	})
	return stop
}
func configRuntimeChange(t *testing.T) {
	f := orderFixture(t, 1, "0s", "0s")
	delete(f.task, "start")
	f.task["schedule"] = map[string]any{"every": "8760h", "input_key": "schedule", "concurrency_key": "schedule", "input": map[string]any{"label": "S"}}
	updateOrderTask(t, f)
	script := strings.Replace(orderScript, "gates=inp.get(s+'_gates',{})", "sys.stdout.write('O'*96);sys.stdout.flush();sys.stderr.write('E'*96);sys.stderr.flush()\ngates=inp.get(s+'_gates',{})", 1)
	script = strings.Replace(script, "count>=1", "count>=inp.get('needed_calls',1)", 1)
	writeTest(t, filepath.Join(f.root, "runner.py"), []byte(script), 0700)
	cfg := filepath.Join(f.dir, "config.yaml")
	oldConfig := "max_running_runs: 2\nmax_check_processes: 2\nmax_pending_submissions: 10\nmax_pending_per_input_key: 10\nstep_log_bytes: 512\n"
	newConfig := "max_running_runs: 1\nmax_check_processes: 1\nmax_pending_submissions: 2\nmax_pending_per_input_key: 1\nstep_log_bytes: 64\nmax_repeat: 0\nmax_calls_per_run: 2\ncaps: {agent_timeout: 1s, run_timeout: 1s}\n"
	writeTest(t, cfg, []byte(oldConfig), 0600)
	gate := filepath.Join(f.root, "initial-release")
	a := submitCore(t, f, "runtime", "A", "A", map[string]any{"label": "A", "before_gate": gate})
	b := submitCore(t, f, "runtime", "B", "B", map[string]any{"label": "B", "before_gate": gate})
	stop := configDaemon(t, f)
	f.rejected(t, 5, "daemon_running", "daemon")
	waitExternal(t, f, "A", "before", 1)
	waitExternal(t, f, "B", "before", 1)
	if n := configCount(t, f, "SELECT count(*) FROM run_schedule WHERE slot_held=1"); n != 2 {
		t.Fatalf("initial running slots %d", n)
	}
	before := f.durableAdmission(t)
	definitions := parsingDefinitions(t, f.intakeFixture)
	writeTest(t, cfg, []byte(newConfig), 0600)
	f.call(t, "run", "show", a["run_id"].(string), "--json")
	f.call(t, "schedule", "show", "runtime", "--json")
	for _, args := range [][]string{{"task", "register", f.manifest}, {"task", "update", f.manifest, "--if-version", "3"}, {"task", "enable", "runtime"}, {"task", "disable", "runtime"}, {"run", "submit", filepath.Join(f.root, "core-input.json")}, {"schedule", "enable", "runtime"}, {"schedule", "disable", "runtime"}, {"schedule", "submit", "runtime", "--at", "2026-09-14T00:00:00Z"}} {
		f.rejected(t, 6, "config_mismatch", args...)
	}
	if f.durableAdmission(t) != before || parsingDefinitions(t, f.intakeFixture) != definitions {
		t.Fatal("mismatched CLI changed durable work")
	}
	writeTest(t, gate, nil, 0600)
	for _, item := range []map[string]any{a, b} {
		views := completedRuns(t, f, item, 2)
		assertConfigLogs(t, views, 192, false)
	}
	stop()
	// Accept under the original caps while no daemon is running, then lower them.
	writeTest(t, cfg, []byte(oldConfig), 0600)
	gate = filepath.Join(f.root, "restart-release")
	queued := []map[string]any{}
	snapshots := map[string]string{}
	for _, label := range []string{"C", "D", "E"} {
		item := submitCore(t, f, "runtime", label, label, map[string]any{"label": label, "before_gate": gate, "needed_calls": 3})
		queued = append(queued, item)
		var snapshot string
		if e := f.database(t).QueryRow("SELECT snapshot FROM submissions WHERE id=?", item["submission_id"]).Scan(&snapshot); e != nil {
			t.Fatal(e)
		}
		snapshots[item["submission_id"].(string)] = snapshot
	}
	writeTest(t, cfg, []byte(newConfig), 0600)
	stop = configDaemon(t, f)
	waitExternal(t, f, "C", "before", 1)
	for _, item := range queued[1:] {
		waitReady(t, f, item["run_id"].(string))
	}
	// The old two-hour budget must survive a new one-second installation cap.
	until := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(until) {
		if n := configCount(t, f, "SELECT count(*) FROM run_schedule WHERE slot_held=1"); n != 1 {
			t.Fatalf("restarted daemon running slots %d", n)
		}
		if n := configCount(t, f, "SELECT count(*) FROM submissions WHERE state='active'"); n != 3 {
			t.Fatalf("existing over-cap receipts changed: %d", n)
		}
		for _, event := range orderEvents(t, f) {
			if (event.Label == "D" || event.Label == "E") && event.Stage == "before" {
				t.Fatal("new Run cap was ignored")
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, item := range queued {
		var snapshot string
		var total, remaining int
		if e := f.database(t).QueryRow("SELECT s.snapshot,b.total,b.remaining FROM submissions s JOIN repeat_budgets b ON b.submission_id=s.id WHERE s.id=?", item["submission_id"]).Scan(&snapshot, &total, &remaining); e != nil {
			t.Fatal(e)
		}
		if snapshot != snapshots[item["submission_id"].(string)] || total != 1 || remaining != 1 {
			t.Fatal("restart rewrote snapshot or repeat budget")
		}
	}
	writeTest(t, gate, nil, 0600)
	for _, item := range queued {
		views := completedRuns(t, f, item, 2)
		assertConfigLogs(t, views, 64, true)
		for _, view := range views {
			if view["stage"] != "succeeded" || view["calls_used"] != float64(3) {
				t.Fatalf("existing execution was recapped: %v", view)
			}
			artifact, e := os.ReadFile(filepath.Join(f.dir, "runs", view["run_id"].(string), "artifact"))
			if e != nil || !strings.HasPrefix(string(artifact), view["input_key"].(string)+"-") {
				t.Fatalf("legacy artifact: %s %v", artifact, e)
			}
		}
	}
	stop()
}
func assertConfigLogs(t *testing.T, views []map[string]any, want int64, truncated bool) {
	t.Helper()
	for _, view := range views {
		steps := view["steps"].([]any)
		if len(steps) == 0 {
			t.Fatal("no execution steps")
		}
		for _, raw := range steps {
			result := raw.(map[string]any)["result"].(map[string]any)
			var total int64
			for _, key := range []string{"stdout_path", "stderr_path"} {
				info, e := os.Stat(result[key].(string))
				if e != nil {
					t.Fatal(e)
				}
				total += info.Size()
			}
			if total != want || result["truncated"] != truncated {
				t.Fatalf("configured log cap: bytes=%d want=%d result=%v", total, want, result)
			}
		}
	}
}
func configCount(t *testing.T, f runtimeFixture, query string) int {
	t.Helper()
	var n int
	if e := f.database(t).QueryRow(query).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}
func configCheckLimit(t *testing.T) {
	f := orderFixture(t, 0, "0s", "0s")
	cfg := filepath.Join(f.dir, "config.yaml")
	for _, limit := range []int{2, 1} {
		writeTest(t, cfg, []byte(fmt.Sprintf("max_check_processes: %d", limit)), 0600)
		gate := filepath.Join(f.root, fmt.Sprintf("checks-%d", limit))
		items := []map[string]any{}
		for _, label := range []string{fmt.Sprintf("check%d-A", limit), fmt.Sprintf("check%d-B", limit)} {
			items = append(items, submitCore(t, f, "runtime", label, label, map[string]any{"label": label, "start_check_gate": gate}))
		}
		stop := configDaemon(t, f)
		waitUntil(t, func() bool { return configCount(t, f, "SELECT count(*) FROM check_slots") == limit })
		until := time.Now().Add(250 * time.Millisecond)
		for time.Now().Before(until) {
			if n := configCount(t, f, "SELECT count(*) FROM check_slots"); n != limit {
				t.Fatalf("check cap %d: %d", limit, n)
			}
			var seen int
			for _, e := range orderEvents(t, f) {
				if strings.HasPrefix(e.Label, fmt.Sprintf("check%d-", limit)) && e.Stage == "start_check" {
					seen++
				}
			}
			if seen > limit {
				t.Fatalf("external checks exceeded cap %d: %d", limit, seen)
			}
			time.Sleep(15 * time.Millisecond)
		}
		writeTest(t, gate, nil, 0600)
		for _, item := range items {
			completedRuns(t, f, item, 1)
		}
		stop()
	}
}
func configAdmissionChange(t *testing.T) {
	f := newIntake(t)
	for i := 0; i < 3; i++ {
		writeTest(t, f.input, []byte(inputJSON(fmt.Sprintf(`{"n":%d}`, i))), 0600)
		f.call(t, "run", "submit", f.input)
	}
	cfg := filepath.Join(f.dir, "config.yaml")
	writeTest(t, cfg, []byte("max_pending_submissions: 2\nmax_pending_per_input_key: 2\n"), 0600)
	before := f.durableAdmission(t)
	writeTest(t, f.input, []byte(inputJSON(`{"n":4}`)), 0600)
	f.rejected(t, 5, "queue_full", "run", "submit", f.input)
	if f.durableAdmission(t) != before {
		t.Fatal("lower cap removed existing submissions")
	}
	writeTest(t, f.input, []byte(inputJSON(`{"n":0}`)), 0600)
	if v := f.call(t, "run", "submit", f.input); v["deduplicated"] != true {
		t.Fatal(v)
	}
	// Independently exercise the per-key limit with room in the total queue.
	writeTest(t, cfg, []byte("max_pending_submissions: 10\nmax_pending_per_input_key: 2\n"), 0600)
	writeTest(t, f.input, []byte(inputJSON(`{"n":4}`)), 0600)
	f.rejected(t, 5, "queue_full", "run", "submit", f.input)
	raw := strings.Replace(inputJSON(`{"n":4}`), `"input_key":"item"`, `"input_key":"other"`, 1)
	writeTest(t, f.input, []byte(raw), 0600)
	f.call(t, "run", "submit", f.input)
	f.noExecution(t)
	// Current and truly historical selected versions both obey today's Task caps.
	path := filepath.Join(filepath.Dir(f.dir), "task.json")
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var task map[string]any
	if e = json.Unmarshal(b, &task); e != nil {
		t.Fatal(e)
	}
	task["repeat"] = 0
	task["finish"].(map[string]any)["max_calls"] = 1
	b, e = json.Marshal(task)
	if e != nil {
		t.Fatal(e)
	}
	writeTest(t, path, b, 0600)
	f.call(t, "task", "update", path, "--if-version", "1")
	writeTest(t, cfg, []byte("max_repeat: 0\nmax_calls_per_run: 1\n"), 0600)
	raw = `{"task_id":"concurrent","input_key":"historical","input":{},"concurrency_key":"historical","task_version":1}`
	writeTest(t, f.input, []byte(raw), 0600)
	parsingRejected(t, f, "run", "submit", f.input)
	raw = strings.Replace(raw, `,"task_version":1`, "", 1)
	writeTest(t, f.input, []byte(raw), 0600)
	if v := f.call(t, "run", "submit", f.input); v["task_version"] != float64(2) {
		t.Fatal(v)
	}
	writeTest(t, cfg, []byte("max_pending_submissions: 2\nmax_pending_per_input_key: 2\n"), 0600)
	writeTest(t, f.input, []byte(strings.Replace(raw, `"input_key":"historical"`, `"input_key":"after-drain"`, 1)), 0600)
	f.rejected(t, 5, "queue_full", "run", "submit", f.input)
	runtime := runtimeFixture{intakeFixture: f, root: filepath.Dir(f.dir)}
	stop := configDaemon(t, runtime)
	waitUntil(t, func() bool {
		return configCount(t, runtime, "SELECT count(*) FROM submissions WHERE state='active'") == 0
	})
	accepted := f.call(t, "run", "submit", f.input)
	if accepted["deduplicated"] != false {
		t.Fatal(accepted)
	}
	waitUntil(t, func() bool {
		return configCount(t, runtime, "SELECT count(*) FROM submissions WHERE state='active'") == 0
	})
	if n := configCount(t, runtime, "SELECT count(*) FROM submissions"); n != 6 {
		t.Fatalf("draining removed history or lost new receipt: %d", n)
	}
	stop()
}

func configCompletedLogRestart(t *testing.T, age bool) {
	f := orderFixture(t, 0, "0s", "0s")
	script := strings.Replace(orderScript, "gates=inp.get(s+'_gates',{})", "print('completed details',flush=True)\ngates=inp.get(s+'_gates',{})", 1)
	writeTest(t, filepath.Join(f.root, "runner.py"), []byte(script), 0700)
	submitted := submitCore(t, f, "runtime", "old-log", "old-log", map[string]any{"label": "old-log"})
	stop := configDaemon(t, f)
	views := completedRuns(t, f, submitted, 1)
	before, _ := json.Marshal(views[0])
	admission := f.durableAdmission(t)
	paths := []string{}
	for _, raw := range views[0]["steps"].([]any) {
		result := raw.(map[string]any)["result"].(map[string]any)
		path := result["stdout_path"].(string)
		info, e := os.Stat(path)
		if e != nil || info.Size() == 0 {
			t.Fatalf("missing completed log before cleanup: %v", e)
		}
		paths = append(paths, path, result["stderr_path"].(string))
	}
	if len(paths) == 0 {
		t.Fatal("no completed log paths")
	}
	if age {
		// Observe the original long retention through a complete maintenance interval.
		time.Sleep(1100 * time.Millisecond)
		for _, path := range paths {
			if _, e := os.Stat(path); e != nil {
				t.Fatalf("young logs removed before retention change: %v", e)
			}
		}
	}
	stop()
	config := "completed_log_bytes: 1"
	if age {
		config = "completed_log_retention: 1ns"
	}
	writeTest(t, filepath.Join(f.dir, "config.yaml"), []byte(config), 0600)
	stop = configDaemon(t, f)
	waitUntil(t, func() bool {
		for _, path := range paths {
			if _, e := os.Stat(path); !os.IsNotExist(e) {
				return false
			}
		}
		return true
	})
	after, _ := json.Marshal(f.call(t, "run", "show", submitted["run_id"].(string), "--json"))
	if string(before) != string(after) || f.durableAdmission(t) != admission {
		t.Fatal("completed cleanup changed results or admission")
	}
	replay := f.call(t, "run", "submit", filepath.Join(f.root, "core-input.json"))
	sameSubmission(t, submitted, replay)
	if f.durableAdmission(t) != admission {
		t.Fatal("log cleanup replay changed admission")
	}
	stop()
}

func configConcurrentClients(t *testing.T) {
	f := newIntake(t)
	lock, e := os.OpenFile(filepath.Join(f.dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = lock.Close() }()
	// Hold another client's shared configuration probe open across a real CLI
	// invocation. Only the daemon's exclusive ownership can mean a live daemon.
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); e != nil {
		t.Fatal(e)
	}
	f.call(t, "run", "submit", f.input)
	f.call(t, "task", "enable", "concurrent")
	f.noExecution(t)
}
