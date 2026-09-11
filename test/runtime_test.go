package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

type runtimeFixture struct {
	intakeFixture
	root, manifest, records string
	task                    map[string]any
}

func newRuntime(t *testing.T) runtimeFixture {
	t.Helper()
	root := t.TempDir()
	f := runtimeFixture{intakeFixture: intakeFixture{bin: filepath.Join(root, "todoable"), dir: filepath.Join(root, "data"), input: filepath.Join(root, "input.json")}, root: root, manifest: filepath.Join(root, "task.json"), records: filepath.Join(root, "records")}
	if e := os.MkdirAll(f.records, 0700); e != nil {
		t.Fatal(e)
	}
	build := exec.Command("go", "build", "-race", "-o", f.bin, "./cmd/todoable")
	build.Dir = ".."
	if b, e := build.CombinedOutput(); e != nil {
		t.Fatalf("build %v %s", e, b)
	}
	script := filepath.Join(root, "runner.py")
	writeTest(t, script, []byte(runtimeScript), 0700)
	command := []string{"/usr/bin/python3", script, "$HOME; $(touch NEVER)", "literal {{input}}", f.records}
	f.task = map[string]any{"version": 1, "id": "runtime", "workdir": root, "agent": command, "prompt": "literal {{input}} $HOME", "before": command, "after": command, "start": map[string]any{"check": command, "poll_every": "1s"}, "finish": map[string]any{"check": command, "max_calls": 3}, "env": map[string]string{"OVERRIDE": "task-value"}, "inherit_env": []string{"CAPTURE"}}
	t.Setenv("CAPTURE", "submission-secret")
	t.Setenv("OVERRIDE", "parent-value")
	t.Setenv("NOT_ALLOWED", "hidden-parent")
	f.register(t)
	return f
}
func (f runtimeFixture) register(t *testing.T) {
	t.Helper()
	b, e := json.Marshal(f.task)
	if e != nil {
		t.Fatal(e)
	}
	writeTest(t, f.manifest, b, 0600)
	f.call(t, "task", "register", f.manifest)
}
func (f runtimeFixture) submit(t *testing.T, input map[string]any) map[string]any {
	t.Helper()
	b, e := json.Marshal(map[string]any{"task_id": "runtime", "input_key": "item", "input": input, "concurrency_key": "resource"})
	if e != nil {
		t.Fatal(e)
	}
	writeTest(t, f.input, b, 0600)
	return f.call(t, "run", "submit", f.input)
}
func (f runtimeFixture) daemon(t *testing.T) {
	t.Helper()
	cmd := exec.Command(f.bin, "--data-dir", f.dir, "daemon", "run")
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0", "CAPTURE=daemon-wrong", "OVERRIDE=daemon-wrong")
	log := filepath.Join(f.root, "daemon.stderr")
	stderr, e := os.Create(log)
	if e != nil {
		t.Fatal(e)
	}
	cmd.Stderr = stderr
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			if err != nil {
				b, _ := os.ReadFile(log)
				t.Errorf("daemon %v %s", err, b)
			}
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			t.Error("daemon did not stop")
		}
		_ = stderr.Close()
	})
}
func (f runtimeFixture) await(t *testing.T, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		v := f.call(t, "run", "show", id, "--json")
		if v["state"] == "succeeded" || v["state"] == "failed" || v["state"] == "blocked" {
			return v
		}
		time.Sleep(15 * time.Millisecond)
	}
	b, _ := os.ReadFile(filepath.Join(f.root, "daemon.stderr"))
	t.Fatalf("Run did not finish: %s", b)
	return nil
}
func (f runtimeFixture) recordsFor(t *testing.T, id string) []map[string]any {
	t.Helper()
	paths, e := filepath.Glob(filepath.Join(f.records, id+"-*.json"))
	if e != nil {
		t.Fatal(e)
	}
	records := []map[string]any{}
	for _, p := range paths {
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		var r map[string]any
		if e = json.Unmarshal(b, &r); e != nil {
			t.Fatal(e)
		}
		records = append(records, r)
	}
	return records
}
func assertRuntime(t *testing.T, f runtimeFixture, submitted, view map[string]any, want string, calls int, stages []string) {
	t.Helper()
	if view["state"] != strings.Split(want, ":")[0] || view["stage"] != want || view["calls_used"] != float64(calls) || view["run_id"] != submitted["run_id"] || view["submission_id"] != submitted["submission_id"] {
		t.Fatalf("Run result: %v", view)
	}
	steps := view["steps"].([]any)
	source, err := os.ReadFile(f.input)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Input struct{ SuccessAt, AgentExit, AfterExit int }
	}
	// Decode the independently submitted fixture controls, not persisted results.
	var fields struct {
		Input map[string]json.RawMessage `json:"input"`
	}
	if err = json.Unmarshal(source, &fields); err != nil {
		t.Fatal(err)
	}
	for key, target := range map[string]*int{"success_at": &request.Input.SuccessAt, "agent_exit": &request.Input.AgentExit, "after_exit": &request.Input.AfterExit} {
		if raw, ok := fields.Input[key]; ok {
			if err = json.Unmarshal(raw, target); err != nil {
				t.Fatal(err)
			}
		}
	}
	expectedCall := 0
	actual := []string{}
	for _, item := range steps {
		step := item.(map[string]any)
		actual = append(actual, step["stage"].(string))
		if step["result"] == nil {
			t.Fatal("missing result")
		}
		exit := 0
		switch step["stage"] {
		case "agent":
			expectedCall++
			exit = request.Input.AgentExit
		case "finish_check":
			if expectedCall < request.Input.SuccessAt {
				exit = 1
			}
		case "after":
			exit = request.Input.AfterExit
		}
		result := step["result"].(map[string]any)
		if result["kind"] != "exited" || result["exit_code"] != float64(exit) || step["call_index"] != float64(expectedCall) {
			t.Fatalf("recorded Step result disagrees with fixture: stage=%s kind=%v exit=%v call=%v want exit=%d call=%d", step["stage"], result["kind"], result["exit_code"], step["call_index"], exit, expectedCall)
		}
	}
	if !reflect.DeepEqual(actual, stages) {
		t.Fatalf("stages %v want %v", actual, stages)
	}
	records := f.recordsFor(t, submitted["run_id"].(string))
	if len(records) != len(stages) {
		t.Fatalf("external steps %d want %d", len(records), len(stages))
	}
	for _, record := range records {
		c := record["context"].(map[string]any)
		want := []any{c["stage"], c["call_index"], float64(1), c["call_index"]}
		if !reflect.DeepEqual(record["intent"], want) {
			t.Fatalf("command started without committed Step and slot: %v want %v", record["intent"], want)
		}
	}

	db := f.database(t)
	var state string
	var used int
	if e := db.QueryRow("SELECT state,calls_used FROM runs WHERE id=?", submitted["run_id"]).Scan(&state, &used); e != nil || state != view["state"] || used != calls {
		t.Fatalf("persisted state %s %d %v", state, used, e)
	}
	var substate string
	if e := db.QueryRow("SELECT state FROM submissions WHERE id=?", submitted["submission_id"]).Scan(&substate); e != nil || substate != "completed" {
		t.Fatalf("submission %s %v", substate, e)
	}
}

const runtimeScript = `import sys,os,json,time,sqlite3
path=os.environ['TODOABLE_CONTEXT_PATH']
raw=open(path,'rb').read()
c=json.loads(raw)
s=c['stage']; inp=c['input']; run=c['run_id']; directory=sys.argv[3]
stdin=sys.stdin.buffer.read()
db=sqlite3.connect(os.path.join(os.path.dirname(os.path.dirname(c['run_dir'])),'todoable.db'))
intent=db.execute('SELECT s.stage,s.call_index,s.result IS NULL,r.calls_used FROM steps s JOIN runs r ON r.id=s.run_id WHERE s.id=?',(c['step_id'],)).fetchone()
db.close()
record={'intent':intent,'context':c,'context_path':path,'context_raw':raw.decode(),'stdin':stdin.decode(),'argv':sys.argv[1:3], 'env':dict(os.environ),'cwd':os.getcwd()}
open(os.path.join(directory,run+'-'+c['step_id']+'.json'),'w').write(json.dumps(record))
countpath=os.path.join(c['run_dir'],'count')
count=int(open(countpath).read()) if os.path.exists(countpath) else 0
if s=='start_check': sys.exit(0)
if s=='before': sys.exit(0)
if s=='finish_check':
 sys.stdout.buffer.write(b'\xff'+b'O'*9000)
 sys.stderr.buffer.write(b'\xfe'+b'E'*9000)
 sys.exit(0 if count>=inp.get('success_at',0) else 1)
if s=='agent':
 count+=1
 open(countpath,'w').write(str(count))
 open(os.path.join(c['run_dir'],'artifact'),'w').write('effect-'+str(count))
 print('succeeded even if no finish condition')
 sys.exit(inp.get('agent_exit',0))
if s=='after':
 gate=inp.get('after_gate')
 while gate and not os.path.exists(gate): time.sleep(.01)
 open(os.path.join(c['run_dir'],'published'),'w').write('published')
 sys.exit(inp.get('after_exit',0))
`

func runtimeStages(calls int) []string {
	stages := []string{"start_check", "start_check", "before", "finish_check"}
	for range calls {
		stages = append(stages, "agent", "finish_check")
	}
	return stages
}

func assertNonzero(t *testing.T, success int) {
	t.Helper()
	f := newRuntime(t)
	submitted := f.submit(t, map[string]any{"success_at": success, "agent_exit": 17})
	f.daemon(t)
	view := f.await(t, submitted["run_id"].(string))
	calls := 3
	want := "failed:max_calls"
	stages := runtimeStages(calls)
	if success == 1 {
		calls = 1
		want = "succeeded"
		stages = append(runtimeStages(calls), "after")
	}
	assertRuntime(t, f, submitted, view, want, calls, stages)
	for _, step := range view["steps"].([]any) {
		s := step.(map[string]any)
		if s["stage"] == "agent" {
			r := s["result"].(map[string]any)
			if r["kind"] != "exited" || r["exit_code"] != float64(17) {
				t.Fatalf("nonzero reclassified %v", r)
			}
		}
	}
}

func contractExecution(t *testing.T) {
	t.Helper()
	f := newRuntime(t)
	submitted := f.submit(t, map[string]any{"success_at": 1, "value": "$(touch NEVER)"})
	t.Setenv("CAPTURE", "changed-after-submission")
	f.daemon(t)
	id := submitted["run_id"].(string)
	view := f.await(t, id)
	assertRuntime(t, f, submitted, view, "succeeded", 1, append(runtimeStages(1), "after"))
	records := f.recordsFor(t, id)
	startFeedback := map[string]any{"kind": "start", "exit_code": float64(0), "stdout": "", "stderr": "", "truncated": false}
	finishFalse := map[string]any{"kind": "finish", "exit_code": float64(1), "stdout": "�" + strings.Repeat("O", 8191), "stderr": "�" + strings.Repeat("E", 8191), "truncated": true}
	finishTrue := map[string]any{"kind": "finish", "exit_code": float64(0), "stdout": "�" + strings.Repeat("O", 8191), "stderr": "�" + strings.Repeat("E", 8191), "truncated": true}
	// The seven expected contexts follow this fixture's independent command script.
	wantFeedback := []any{nil, startFeedback, startFeedback, startFeedback, finishFalse, finishFalse, finishTrue}
	wantCalls := []float64{0, 0, 0, 0, 1, 1, 1}
	stepIndex := map[string]int{}
	for index, item := range view["steps"].([]any) {
		stepIndex[item.(map[string]any)["step_id"].(string)] = index
	}
	stages := map[string]int{}
	for _, record := range records {
		c := record["context"].(map[string]any)
		stage := c["stage"].(string)
		stages[stage]++
		env := record["env"].(map[string]any)
		if c["protocol_version"] != float64(1) || c["task_id"] != "runtime" || c["task_version"] != float64(1) || c["submission_id"] != submitted["submission_id"] || c["run_id"] != id || c["run_seq"] != float64(1) || c["input_key"] != "item" || c["prompt"] != "literal {{input}} $HOME" || c["workdir"] != f.root || c["run_dir"] != filepath.Join(f.dir, "runs", id) {
			t.Fatalf("context fields %v", c)
		}
		input := c["input"].(map[string]any)
		if input["value"] != "$(touch NEVER)" || input["success_at"] != float64(1) {
			t.Fatalf("input changed %v", input)
		}
		index, ok := stepIndex[c["step_id"].(string)]
		if !ok || index >= len(wantFeedback) {
			t.Fatalf("external Step absent from persisted order: %v", c)
		}
		if c["call_index"] != wantCalls[index] {
			t.Fatalf("Step %d call index %v want %v", index, c["call_index"], wantCalls[index])
		}
		if !reflect.DeepEqual(c["last_check"], wantFeedback[index]) {
			t.Fatalf("Step %d (%s) last_check mismatch: got %v want %v", index, stage, c["last_check"], wantFeedback[index])
		}
		reserved := map[string]string{"TODOABLE_CONTEXT_PATH": record["context_path"].(string), "TODOABLE_RUN_DIR": filepath.Join(f.dir, "runs", id), "TODOABLE_TASK_ID": "runtime", "TODOABLE_TASK_VERSION": "1", "TODOABLE_SUBMISSION_ID": submitted["submission_id"].(string), "TODOABLE_RUN_ID": id, "TODOABLE_STEP_ID": c["step_id"].(string), "TODOABLE_CALL_INDEX": fmt.Sprint(c["call_index"])}
		for k, v := range reserved {
			if env[k] != v {
				t.Fatalf("reserved %s=%v want %s", k, env[k], v)
			}
		}
		if env["CAPTURE"] != "submission-secret" || env["OVERRIDE"] != "task-value" || env["NOT_ALLOWED"] != nil {
			t.Fatalf("environment snapshot %v", env)
		}
		if !reflect.DeepEqual(record["argv"], []any{"$HOME; $(touch NEVER)", "literal {{input}}"}) || record["cwd"] != f.root {
			t.Fatalf("argv/workdir %v", record)
		}
		b, e := os.ReadFile(record["context_path"].(string))
		if e != nil || string(b) != record["context_raw"] {
			t.Fatalf("context changed %s %v", b, e)
		}
		stat, e := os.Stat(record["context_path"].(string))
		if e != nil || stat.Mode().Perm() != 0400 {
			t.Fatalf("context mode %v %v", stat, e)
		}
		if stage == "agent" {
			if record["stdin"] != string(b) {
				t.Fatal("agent stdin differs from immutable context")
			}
		} else if record["stdin"] != "" {
			t.Fatalf("hook/check stdin nonempty %v", record)
		}

	}
	for _, stage := range []string{"start_check", "before", "finish_check", "agent", "after"} {
		if stages[stage] == 0 {
			t.Fatalf("stage missing %s", stage)
		}
	}
	for _, item := range view["steps"].([]any) {
		step := item.(map[string]any)
		if step["stage"] != "finish_check" {
			continue
		}
		result := step["result"].(map[string]any)
		for _, stream := range []string{"stdout", "stderr"} {
			b, e := os.ReadFile(result[stream+"_path"].(string))
			first := byte(255)
			fill := "O"
			if stream == "stderr" {
				first = 254
				fill = "E"
			}
			want := append([]byte{first}, []byte(strings.Repeat(fill, 9000))...)
			if e != nil || !bytes.Equal(b, want) {
				t.Fatalf("raw %s lost %d %v", stream, len(b), e)
			}
		}
	}
	if _, e := os.Stat(filepath.Join(f.root, "NEVER")); !os.IsNotExist(e) {
		t.Fatalf("shell expansion effect: %v", e)
	}
	encoded, _ := json.Marshal(view)
	for _, secret := range []string{"submission-secret", "task-value", "changed-after-submission"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("query exposed environment value %s", secret)
		}
	}
}
func contractRejections(t *testing.T) {
	t.Helper()
	t.Run("missing inherited value", func(t *testing.T) {
		f := newRuntime(t)
		if e := os.Unsetenv("CAPTURE"); e != nil {
			t.Fatal(e)
		}
		writeTest(t, f.input, []byte(`{"task_id":"runtime","input_key":"item","input":{},"concurrency_key":"resource"}`), 0600)
		f.rejected(t, 2, "validation_error", "run", "submit", f.input)
		db := f.database(t)
		var n int
		if e := db.QueryRow("SELECT count(*) FROM submissions").Scan(&n); e != nil || n != 0 {
			t.Fatalf("rejected submission persisted %d %v", n, e)
		}
	})
	t.Run("reserved variables", func(t *testing.T) {
		f := newRuntime(t)
		for i, field := range []string{"env", "inherit_env"} {
			f.task["id"] = fmt.Sprintf("bad-%d", i)
			if field == "env" {
				f.task[field] = map[string]string{"TODOABLE_RUN_ID": "forged"}
			} else {
				f.task["env"] = map[string]string{}
				f.task[field] = []string{"TODOABLE_TASK_ID"}
			}
			b, _ := json.Marshal(f.task)
			writeTest(t, f.manifest, b, 0600)
			f.rejected(t, 2, "validation_error", "task", "register", f.manifest)
		}
		db := f.database(t)
		var n int
		if e := db.QueryRow("SELECT count(*) FROM tasks").Scan(&n); e != nil || n != 1 {
			t.Fatalf("invalid definition persisted %d %v", n, e)
		}
	})
	t.Run("missing workdir", func(t *testing.T) {
		f := newRuntime(t)
		work := filepath.Join(f.root, "working")
		if e := os.Mkdir(work, 0700); e != nil {
			t.Fatal(e)
		}
		f.task["workdir"] = work
		b, _ := json.Marshal(f.task)
		writeTest(t, f.manifest, b, 0600)
		f.call(t, "task", "update", f.manifest, "--if-version", "1")
		submitted := f.submit(t, map[string]any{})
		if e := os.Remove(work); e != nil {
			t.Fatal(e)
		}
		f.daemon(t)
		view := f.await(t, submitted["run_id"].(string))
		if view["state"] != "blocked" || view["stage"] != "blocked:check_error" || view["calls_used"] != float64(0) {
			t.Fatalf("missing workdir outcome %v", view)
		}
		steps := view["steps"].([]any)
		if len(steps) != 1 || steps[0].(map[string]any)["result"].(map[string]any)["kind"] != "start_failed" {
			t.Fatalf("start failure %v", steps)
		}
		if records := f.recordsFor(t, submitted["run_id"].(string)); len(records) != 0 {
			t.Fatalf("missing workdir executed %v", records)
		}
		b, _ = json.Marshal(view)
		if bytes.Contains(b, []byte("submission-secret")) || bytes.Contains(b, []byte("task-value")) {
			t.Fatalf("diagnostics exposed environment %s", b)
		}
	})
}

func missingWorkdirStages(t *testing.T) {
	for _, stage := range []string{"before", "finish_check", "agent", "after"} {
		t.Run(stage, func(t *testing.T) {
			f := newRuntime(t)
			work := filepath.Join(f.root, "working")
			if e := os.Mkdir(work, 0700); e != nil {
				t.Fatal(e)
			}
			f.task["workdir"] = work
			delete(f.task, "start")
			if stage != "before" {
				delete(f.task, "before")
			}
			if stage == "agent" || stage == "after" {
				exit := "0"
				if stage == "agent" {
					exit = "1"
				}
				f.task["finish"] = map[string]any{"check": []string{"/bin/sh", "-c", "rmdir \"$1\"; exit \"$2\"", "sh", work, exit}, "max_calls": 3}
			}
			b, _ := json.Marshal(f.task)
			writeTest(t, f.manifest, b, 0600)
			f.call(t, "task", "update", f.manifest, "--if-version", "1")
			submitted := f.submit(t, map[string]any{})
			if stage == "before" || stage == "finish_check" {
				if e := os.Remove(work); e != nil {
					t.Fatal(e)
				}
			}
			f.daemon(t)
			view := f.await(t, submitted["run_id"].(string))
			want := "blocked:check_error"
			if stage == "before" || stage == "after" {
				want = "failed:" + stage
			}
			if view["stage"] != want {
				t.Fatalf("stage failure %v", view)
			}
			steps := view["steps"].([]any)
			found := false
			for _, item := range steps {
				s := item.(map[string]any)
				if s["stage"] == stage && s["result"].(map[string]any)["kind"] == "start_failed" {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing stage start failure %v", steps)
			}
			calls := float64(0)
			if stage == "agent" {
				calls = 1
				if len(steps) != 3 || steps[2].(map[string]any)["stage"] != "finish_check" {
					t.Fatalf("agent failure skipped finish check %v", steps)
				}
			}
			if view["calls_used"] != calls {
				t.Fatalf("reserved slot count %v", view)
			}
		})
	}
}
