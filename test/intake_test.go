package test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

type intakeFixture struct{ bin, dir, input, log, artifact string }

func newIntake(t *testing.T) intakeFixture {
	t.Helper()
	root := t.TempDir()
	f := intakeFixture{bin: filepath.Join(root, "todoable"), dir: filepath.Join(root, "data"), input: filepath.Join(root, "input.json"), log: filepath.Join(root, "executions"), artifact: filepath.Join(root, "artifact")}
	cmd := exec.Command("go", "build", "-race", "-o", f.bin, "./cmd/todoable")
	cmd.Dir = ".."
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, b)
	}
	// The fake waits at a caller-controlled gate and records both start and output.
	runner := filepath.Join(root, "runner.sh")
	script := "#!/bin/sh\nprintf 'start\\n' >> \"$1\"\nwhile [ ! -f \"$3\" ]; do sleep 0.01; done\nprintf 'output\\n' > \"$2\"\nprintf 'end\\n' >> \"$1\"\n"
	writeTest(t, runner, []byte(script), 0700)
	// Independently prove the observer detects this fake before testing CLI non-execution.
	probeLog := filepath.Join(root, "probe-log")
	probeArtifact := filepath.Join(root, "probe-artifact")
	gate := filepath.Join(root, "gate")
	writeTest(t, gate, nil, 0600)
	if b, e := exec.Command(runner, probeLog, probeArtifact, gate).CombinedOutput(); e != nil {
		t.Fatalf("fake probe: %v %s", e, b)
	}
	if b, e := os.ReadFile(probeLog); e != nil || string(b) != "start\nend\n" {
		t.Fatalf("fake observation: %q %v", b, e)
	}
	if b, e := os.ReadFile(probeArtifact); e != nil || string(b) != "output\n" {
		t.Fatalf("fake output: %q %v", b, e)
	}
	manifest := map[string]any{"version": 1, "id": "concurrent", "workdir": root, "agent": []string{runner, f.log, f.artifact, gate}, "prompt": "work", "finish": map[string]any{"check": []string{runner, f.log, f.artifact, gate}}, "repeat": 2}
	raw, e := json.Marshal(manifest)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(root, "task.json")
	writeTest(t, path, raw, 0600)
	f.call(t, "task", "register", path)
	writeTest(t, f.input, []byte(`{"task_id":"concurrent","input_key":"item","input":{"b":[true,"1"],"a":1},"concurrency_key":"resource"}`), 0600)
	return f
}
func writeTest(t *testing.T, path string, b []byte, mode os.FileMode) {
	t.Helper()
	if e := os.WriteFile(path, b, mode); e != nil {
		t.Fatal(e)
	}
}
func (f intakeFixture) call(t *testing.T, args ...string) map[string]any {
	t.Helper()
	cmd := exec.Command(f.bin, append([]string{"--data-dir", f.dir}, args...)...)
	var out, stderr bytes.Buffer
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0")
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if e := cmd.Run(); e != nil {
		t.Fatalf("CLI %v: %v stderr=%s stdout=%s", args, e, stderr.String(), out.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("success stderr: %s", stderr.String())
	}
	var v map[string]any
	if e := json.Unmarshal(out.Bytes(), &v); e != nil {
		t.Fatal(e)
	}
	if v["protocol_version"] != float64(1) {
		t.Fatalf("protocol: %v", v)
	}
	return v
}
func (f intakeFixture) stored(t *testing.T, want map[string]any) {
	t.Helper()
	db, e := sql.Open("sqlite3", "file:"+f.dir+"/todoable.db?mode=ro")
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = db.Close() }()
	for _, table := range []string{"submissions", "runs", "repeat_budgets"} {
		var n int
		if e = db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); e != nil || n != 1 {
			t.Fatalf("%s count=%d err=%v", table, n, e)
		}
	}
	var sid, rid, input, state, snap, submissionState string
	var seq, total, remaining, calls, version int
	e = db.QueryRow(`SELECT s.id,r.id,s.input,r.state,r.run_seq,b.total,b.remaining,r.calls_used,s.task_version,s.snapshot,s.state FROM submissions s JOIN runs r ON r.submission_id=s.id JOIN repeat_budgets b ON b.submission_id=s.id`).Scan(&sid, &rid, &input, &state, &seq, &total, &remaining, &calls, &version, &snap, &submissionState)
	if e != nil {
		t.Fatal(e)
	}
	if submissionState != "active" {
		t.Fatalf("submission state: %s", submissionState)
	}
	if sid != want["submission_id"] || rid != want["run_id"] || input != `{"a":1,"b":[true,"1"]}` || state != "waiting" || seq != 1 || total != 2 || remaining != 2 || calls != 0 || version != 1 {
		t.Fatalf("DB mismatch: %s %s %s %s %d %d %d %d %d", sid, rid, input, state, seq, total, remaining, calls, version)
	}
	var snapshot map[string]any
	if e = json.Unmarshal([]byte(snap), &snapshot); e != nil {
		t.Fatal(e)
	}
	if snapshot["repeat"] != float64(2) || snapshot["prompt"] != "work" {
		t.Fatalf("snapshot: %v", snapshot)
	}
	shown := f.call(t, "run", "show", rid, "--json")
	if shown["submission_id"] != sid || shown["run_seq"] != float64(1) || shown["repeat_remaining"] != float64(2) || shown["calls_used"] != float64(0) || shown["state"] != "waiting" {
		t.Fatalf("run show: %v", shown)
	}
	for _, p := range []string{f.log, f.artifact} {
		if _, e = os.Stat(p); !os.IsNotExist(e) {
			t.Fatalf("CLI executed external command: %s err=%v", p, e)
		}
	}
}
func concurrentIntake(t *testing.T, f intakeFixture) []map[string]any {
	t.Helper()
	const count = 12
	results := make([]map[string]any, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; results[i] = f.call(t, "run", "submit", f.input) }()
	}
	close(start)
	wg.Wait()
	return results
}
