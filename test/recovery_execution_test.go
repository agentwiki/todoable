package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func coreFixture(t *testing.T) runtimeFixture {
	t.Helper()
	f := orderFixture(t, 0, "0s", "0s")
	script := strings.Replace(orderScript, "gates=inp.get(s+'_gates',{})", `if s=='before' and inp.get('critical_path'):
 try: open(inp['critical_path'],'x').write(c['run_id'])
 except FileExistsError:
  open(inp['critical_path']+'.collision','a').write(c['run_id']+'\n');sys.exit(9)
gates=inp.get(s+'_gates',{})`, 1)
	script = strings.Replace(script, " sys.exit(7 if seq==inp.get('fail_after_run') else 0)", ` if inp.get('critical_path'):os.unlink(inp['critical_path'])
 record['Stage']='after_done'
 fd=os.open(os.path.join(sys.argv[3],'events'),os.O_WRONLY|os.O_APPEND)
 os.write(fd,(json.dumps(record)+'\n').encode());os.close(fd)
 sys.exit(7 if seq==inp.get('fail_after_run') else 0)`, 1)
	writeTest(t, filepath.Join(f.root, "runner.py"), []byte(script), 0700)
	second := map[string]any{}
	for k, v := range f.task {
		second[k] = v
	}
	second["id"] = "other-task"
	b, _ := json.Marshal(second)
	path := filepath.Join(f.root, "other-task.json")
	writeTest(t, path, b, 0600)
	f.call(t, "task", "register", path)
	return f
}
func submitCore(t *testing.T, f runtimeFixture, task, key, resource string, input map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"task_id": task, "input_key": key, "input": input, "concurrency_key": resource})
	path := filepath.Join(f.root, "core-input.json")
	writeTest(t, path, b, 0600)
	return f.call(t, "run", "submit", path)
}
func sharedResource(t *testing.T, unknown bool) {
	t.Helper()
	f := coreFixture(t)
	critical := filepath.Join(f.root, "critical-owner")
	gate := filepath.Join(f.root, "A-release")
	input := map[string]any{"label": "A", "critical_path": critical}
	if unknown {
		input["block_before_run"] = 1
	} else {
		input["before_gate"] = gate
	}
	a := submitCore(t, f, "runtime", "A", "global-resource", input)
	f.daemon(t)
	waitExternal(t, f, "A", "before", 1)
	if unknown {
		v := f.await(t, a["run_id"].(string))
		if v["stage"] != "blocked:outcome_unknown" {
			t.Fatalf("changing result not unknown %v", v)
		}
	}
	b := submitCore(t, f, "other-task", "B", "global-resource", map[string]any{"label": "B", "critical_path": critical})
	waitReady(t, f, b["run_id"].(string))
	submitCore(t, f, "runtime", "observer", "unrelated-resource", map[string]any{"label": "observer", "start_false": true})
	waitUntil(t, func() bool {
		n := 0
		for _, event := range orderEvents(t, f) {
			if event.Label == "observer" && event.Stage == "start_check" {
				n++
			}
		}
		return n >= 2
	})
	for _, event := range orderEvents(t, f) {
		if event.Label == "B" && event.Stage != "start_check" {
			t.Fatalf("same key executed changing stage before resource release: %v", event)
		}
	}
	var owner string
	if e := f.database(t).QueryRow("SELECT run_id FROM resources WHERE key='global-resource'").Scan(&owner); e != nil || owner != a["run_id"] {
		t.Fatalf("global owner changed %s %v", owner, e)
	}
	raw, e := os.ReadFile(critical)
	if e != nil || string(raw) != a["run_id"] {
		t.Fatalf("external critical section lost %q %v", raw, e)
	}
	if unknown {
		av := f.call(t, "run", "show", a["run_id"].(string), "--json")
		bv := f.call(t, "run", "show", b["run_id"].(string), "--json")
		if av["state"] != "blocked" || bv["state"] != "waiting" || bv["calls_used"] != float64(0) {
			t.Fatalf("blocked owner/successor %v %v", av, bv)
		}
		assertRunFiles(t, f, map[string]string{})
	} else {
		writeTest(t, gate, nil, 0600)
		ar := completedRuns(t, f, a, 1)
		br := completedRuns(t, f, b, 1)
		for _, run := range []map[string]any{ar[0], br[0]} {
			if run["stage"] != "succeeded" || run["calls_used"] != float64(1) {
				t.Fatalf("shared resource result %v", run)
			}
		}
		stages := append(runtimeStages(1), "after", "after_done")
		assertOrderExecutions(t, f, []expectedOrderRun{{a["run_id"].(string), "A", 1, stages}, {b["run_id"].(string), "B", 1, stages}})
		phase := 0
		for _, event := range orderEvents(t, f) {
			if event.Label == "A" && event.Stage == "before" {
				if phase != 0 {
					t.Fatal("duplicate first entry")
				}
				phase = 1
			}
			if event.Label == "A" && event.Stage == "after_done" {
				if phase != 1 {
					t.Fatal("missing first interval")
				}
				phase = 2
			}
			if event.Label == "B" && event.Stage == "before" {
				if phase != 2 {
					t.Fatalf("overlapping external sections %v", event)
				}
				phase = 3
			}
			if event.Label == "B" && event.Stage == "after_done" {
				if phase != 3 {
					t.Fatal("missing second interval")
				}
				phase = 4
			}
		}
		if phase != 4 {
			t.Fatalf("incomplete critical intervals %d", phase)
		}
		if _, e = os.Stat(critical); !os.IsNotExist(e) {
			t.Fatalf("critical section not released %v", e)
		}
	}
	if _, e = os.Stat(critical + ".collision"); !os.IsNotExist(e) {
		t.Fatalf("external resource collision %v", e)
	}
}

func faultFixture(t *testing.T) runtimeFixture {
	t.Helper()
	f := coreFixture(t)
	path := filepath.Join(f.root, "runner.py")
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	injection := `if s=='agent' and inp.get('output_flood'):
 sys.stdout.buffer.write(b'\xff'+b'O'*(7*1024*1024));sys.stdout.buffer.flush()
 sys.stderr.buffer.write(b'\xfe'+b'E'*(7*1024*1024));sys.stderr.buffer.flush()
if s=='start_check' and inp.get('hang_check'):
 while True:time.sleep(.05)
if s=='start_check' and inp.get('escape_check'):
 import subprocess
 code="import os,time;open("+repr(os.path.join(rd,'escaped.pid'))+",'w').write(str(os.getpid()))\nwhile not os.path.exists("+repr(inp['escape_stop'])+") and os.path.isdir("+repr(rd)+"):time.sleep(.02)"
 subprocess.Popen(['/usr/bin/python3','-c',code],start_new_session=True)
 sys.exit(0)
`
	script := strings.Replace(string(b), "gates=inp.get(s+'_gates',{})", injection+"gates=inp.get(s+'_gates',{})", 1)
	writeTest(t, path, []byte(script), 0700)
	f.task["limits"] = map[string]string{"start_check_timeout": "200ms"}
	updateOrderTask(t, f)
	return f
}
func outputAndTimeout(t *testing.T) {
	t.Helper()
	f := faultFixture(t)
	flood := submitCore(t, f, "runtime", "flood", "flood-resource", map[string]any{"label": "flood", "output_flood": true})
	hang := submitCore(t, f, "runtime", "hang", "hang-resource", map[string]any{"label": "hang", "hang_check": true})
	f.daemon(t)
	hv := f.await(t, hang["run_id"].(string))
	if hv["stage"] != "blocked:check_error" || hv["calls_used"] != float64(0) {
		t.Fatalf("hanging check outcome %v", hv)
	}
	runs := completedRuns(t, f, flood, 1)
	assertFloodLogs(t, runs[0])
	next := submitCore(t, f, "runtime", "next", "next-resource", map[string]any{"label": "next"})
	nr := completedRuns(t, f, next, 1)
	for _, run := range []map[string]any{runs[0], nr[0]} {
		if run["stage"] != "succeeded" {
			t.Fatalf("control loop stopped %v", run)
		}
	}
	stages := append(runtimeStages(1), "after", "after_done")
	assertOrderExecutions(t, f, []expectedOrderRun{{flood["run_id"].(string), "flood", 1, stages}, {next["run_id"].(string), "next", 1, stages}})
	var held int
	if e := f.database(t).QueryRow("SELECT count(*) FROM check_slots").Scan(&held); e != nil || held != 0 {
		t.Fatalf("stopped checks retained slots %d %v", held, e)
	}
	for _, event := range orderEvents(t, f) {
		if event.Label == "hang" && event.Stage != "start_check" {
			t.Fatalf("failed check changed state externally %v", event)
		}
	}
	step := hv["steps"].([]any)[0].(map[string]any)
	result := step["result"].(map[string]any)
	if result["kind"] != "unknown" || result["ElapsedNS"].(float64) < float64(150*time.Millisecond) || result["ElapsedNS"].(float64) > float64(3*time.Second) {
		t.Fatalf("check timeout not applied %v", result)
	}
}
func assertFloodLogs(t *testing.T, run map[string]any) {
	t.Helper()
	found := false
	for _, value := range run["steps"].([]any) {
		step := value.(map[string]any)
		if step["stage"] != "agent" {
			continue
		}
		found = true
		result := step["result"].(map[string]any)
		if result["kind"] != "exited" || result["exit_code"] != float64(0) || result["truncated"] != true {
			t.Fatalf("flood did not finish/truncate: kind=%v exit=%v truncated=%v", result["kind"], result["exit_code"], result["truncated"])
		}
		total := 0
		for _, stream := range []string{"stdout", "stderr"} {
			raw, e := os.ReadFile(result[stream+"_path"].(string))
			if e != nil {
				t.Fatal(e)
			}
			prefix := byte(255)
			fill := byte('O')
			if stream == "stderr" {
				prefix = 254
				fill = 'E'
			}
			want := append([]byte{prefix}, bytes.Repeat([]byte{fill}, 7*1024*1024)...)
			if len(raw) == 0 || !bytes.HasPrefix(want, raw) {
				t.Fatalf("log is not an original byte prefix: %s bytes=%d", stream, len(raw))
			}
			total += len(raw)
		}
		if total != 10485760 {
			t.Fatalf("step storage bound %d want 10485760", total)
		}
	}
	if !found {
		t.Fatal("flooding agent never executed")
	}
}
func unknownCheckSlots(t *testing.T) {
	t.Helper()
	f := faultFixture(t)
	stop := filepath.Join(f.root, "escape-stop")
	t.Cleanup(func() {
		writeTest(t, stop, nil, 0600)
		paths, e := filepath.Glob(filepath.Join(f.dir, "runs", "*", "escaped.pid"))
		if e != nil {
			t.Error(e)
			return
		}
		until := time.Now().Add(2 * time.Second)
		for _, path := range paths {
			raw, e := os.ReadFile(path)
			if e != nil {
				continue
			}
			stopped := false
			for time.Now().Before(until) {
				stat, e := os.ReadFile(filepath.Join("/proc", string(raw), "stat"))
				if os.IsNotExist(e) || strings.Contains(string(stat), ") Z ") {
					stopped = true
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !stopped {
				t.Errorf("escaped fixture child did not stop: %s", raw)
			}
		}
	})
	unknown := []map[string]any{submitCore(t, f, "runtime", "unknown-0", "resource-0", map[string]any{"label": "unknown-0", "escape_check": true, "escape_stop": stop})}
	f.daemon(t)
	first := f.await(t, unknown[0]["run_id"].(string))
	if first["stage"] != "blocked:process_unknown" {
		t.Fatalf("unknown check classified as stopped %v", first)
	}
	good := submitCore(t, f, "runtime", "good", "good-resource", map[string]any{"label": "good", "output_flood": true})
	gr := completedRuns(t, f, good, 1)
	assertFloodLogs(t, gr[0])
	for i := 1; i < 4; i++ {
		label := fmt.Sprintf("unknown-%d", i)
		unknown = append(unknown, submitCore(t, f, "runtime", label, label, map[string]any{"label": label, "escape_check": true, "escape_stop": stop}))
	}
	for _, sub := range unknown {
		v := f.await(t, sub["run_id"].(string))
		if v["stage"] != "blocked:process_unknown" || v["calls_used"] != float64(0) {
			t.Fatalf("unconfirmed check state %v", v)
		}
		raw, e := os.ReadFile(filepath.Join(f.dir, "runs", sub["run_id"].(string), "escaped.pid"))
		if e != nil {
			t.Fatal(e)
		}
		pid, e := strconv.Atoi(string(raw))
		if e != nil || syscall.Kill(pid, 0) != nil {
			t.Fatalf("unconfirmed child not alive %q %v", raw, e)
		}
	}
	fifth := submitCore(t, f, "runtime", "fifth", "fifth-resource", map[string]any{"label": "fifth"})
	directTask := map[string]any{}
	for k, v := range f.task {
		directTask[k] = v
	}
	directTask["id"] = "direct-check"
	delete(directTask, "start")
	delete(directTask, "before")
	rawTask, _ := json.Marshal(directTask)
	taskPath := filepath.Join(f.root, "direct-check.json")
	writeTest(t, taskPath, rawTask, 0600)
	f.call(t, "task", "register", taskPath)
	direct := submitCore(t, f, "direct-check", "direct", "direct-resource", map[string]any{"label": "direct"})
	time.Sleep(150 * time.Millisecond)
	dv := f.call(t, "run", "show", direct["run_id"].(string), "--json")
	if dv["state"] != "waiting" || len(dv["steps"].([]any)) != 0 {
		t.Fatalf("direct finish check exceeded cap %v", dv)
	}
	fv := f.call(t, "run", "show", fifth["run_id"].(string), "--json")
	if fv["state"] != "waiting" || len(fv["steps"].([]any)) != 0 {
		t.Fatalf("check cap ignored %v", fv)
	}
	var held int
	if e := f.database(t).QueryRow("SELECT count(*) FROM check_slots").Scan(&held); e != nil || held != 4 {
		t.Fatalf("unknown check slots %d want4: %v", held, e)
	}
	for _, event := range orderEvents(t, f) {
		if event.Label == "fifth" || event.Label == "direct" {
			t.Fatalf("fifth check exceeded cap %v", event)
		}
		if strings.HasPrefix(event.Label, "unknown-") && event.Stage != "start_check" {
			t.Fatalf("unknown check ran changing stage %v", event)
		}
	}
	assertOrderExecutions(t, f, []expectedOrderRun{{good["run_id"].(string), "good", 1, append(runtimeStages(1), "after", "after_done")}})
}
